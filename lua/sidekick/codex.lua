-- This Source Code Form is subject to the terms of the Mozilla Public
-- License, v. 2.0. If a copy of the MPL was not distributed with this
-- file, You can obtain one at https://mozilla.org/MPL/2.0/.

-- Maintainer: Thomas Letan <lthms@soap.coffee>

-- Codex backend, driven through `codex app-server`: a JSON-RPC server speaking
-- newline-delimited JSON over stdio. No terminal, no keystroke injection; what
-- codex does is visible in the buffers it edits through the sidekick MCP
-- server, and summarized in a log buffer of our own.
--
--   nvim --(stdin: turn/start)--> codex app-server --(MCP over http)--> sidekick
--        <--(stdout: notifications)--                                      |
--        <--------------------- buffer edits ---------------------------- +

-- The behavioural contract, passed once as the thread's developer instructions.
-- It is plugins/nvim/commands/monitor.md minus the parts that only make sense
-- for claude: there is no monitor to set up here (a save is a turn) and no chat
-- window to stay out of.
local INSTRUCTIONS = [[
You are driven from neovim by sidekick. Each turn tells you that a buffer was
saved. Find the comment starting with `REQ:` in that buffer and follow its
instruction.

Golden rules:

- Once you have read it, replace the `REQ:` comment with a `RESP:` comment
  acknowledging you started processing the request. Do it as soon as possible.
- Respect the scope of the request. DO NOT overdo.
- ALWAYS use the sidekick MCP tools (read_buffer, edit_buffer, grep, glob,
  open_buffer, save_buffer) instead of reading or writing files directly. The
  buffers may hold unsaved changes, so the file on disk is not the truth. Open
  new buffers if you need to.
- Save your changes with save_buffer when they must reach disk (e.g. before
  building).

Everything you would normally say — reasoning, findings, questions,
confirmations — goes into a `RESP:` comment instead. Your assistant messages
are only read as a log; keep them to a single short line.
]]

local M = {}

M.__index = M

function M.new(config, pid)
  return setmetatable(
    {
      pid = pid,
      config = config.codex,
      sidekick_url = config.server_url,
      session = nil
    },
    M
  )
end

-- Claude needs its plugin installed first; codex needs nothing prepared.
function M:setup(on_ready)
  vim.schedule(on_ready)
end

-- JSON null decodes to vim.NIL, which is neither nil nor falsy — an absent
-- field would otherwise be logged as "vim.NIL".
local function present(value)
  return value ~= nil and value ~= vim.NIL
end

local function send(session, msg)
  vim.fn.chansend(session.job, vim.json.encode(msg) .. "\n")
end

-- The log is a plain scratch buffer the user can open like any other
-- (`:b sidekick://codex`); it is never written to disk.
local function log_buf(session)
  if session.log ~= nil and vim.api.nvim_buf_is_valid(session.log) then
    return session.log
  end

  -- A previous session may still own the named scratch buffer.
  for _, buf in ipairs(vim.api.nvim_list_bufs()) do
    if vim.api.nvim_buf_get_name(buf) == "sidekick://codex" then
      session.log = buf
      return buf
    end
  end

  session.log = vim.api.nvim_create_buf(true, true)
  vim.api.nvim_buf_set_name(session.log, "sidekick://codex")
  vim.bo[session.log].buftype = "nofile"

  return session.log
end

local function log(session, lines)
  if #lines == 0 then
    return
  end

  -- nvim_buf_set_lines rejects embedded newlines, and anything coming from
  -- codex (a command, a tool result, an error message) may contain them.
  local flat = {}
  for _, line in ipairs(lines) do
    vim.list_extend(flat, vim.split(line, "\n"))
  end

  local buf = log_buf(session)
  vim.api.nvim_buf_set_lines(buf, -1, -1, false, flat)

  -- Follow the tail in whatever window already shows the log, if any.
  for _, win in ipairs(vim.api.nvim_list_wins()) do
    if vim.api.nvim_win_get_buf(win) == buf then
      vim.api.nvim_win_set_cursor(win, { vim.api.nvim_buf_line_count(buf), 0 })
    end
  end
end

-- One line per item, enough to follow what codex is doing without
-- reimplementing its TUI. Streaming deltas are ignored; only completed items
-- are rendered.
local function render(item)
  if item.type == "agentMessage" then
    return vim.split(item.text or "", "\n")
  elseif item.type == "commandExecution" then
    local status = present(item.exitCode) and (" (exit " .. item.exitCode .. ")") or ""
    return { "$ " .. (item.command or "?") .. status }
  elseif item.type == "fileChange" then
    local lines = {}
    for _, change in ipairs(item.changes or {}) do
      table.insert(lines, "~ " .. change.kind .. " " .. change.path)
    end
    return lines
  elseif item.type == "mcpToolCall" then
    local call = "· " .. (item.server or "?") .. "/" .. (item.tool or "?")
    if present(item.error) then
      call = call .. " — error: " .. vim.inspect(item.error)
    end
    return { call }
  elseif item.type == "reasoning" then
    return { "  (reasoning)" }
  end

  -- Anything not rendered above still gets a line: while the prompt and the
  -- tool wiring are being tuned, silence is indistinguishable from inactivity.
  return { "  (" .. (item.type or "?") .. ")" }
end

-- Send a JSON-RPC request and remember `on_result` under its id, so the stdout
-- handler can dispatch the matching response back to it.
local function request(session, method, params, on_result)
  session.next_id = session.next_id + 1
  session.pending[session.next_id] = on_result

  log(session, { "> " .. method })

  send(session, {
    jsonrpc = "2.0",
    id = session.next_id,
    method = method,
    params = params,
  })
end

-- Accept approval requests for our own MCP tools. Deny command, file and
-- permission approval requests until an approval UI exists. Commands already
-- allowed by the sandbox may run without sending an approval request.
local function approval(msg)
  if msg.method == "mcpServer/elicitation/request" then
    -- An MCP tool call needing approval arrives as an elicitation, tagged
    -- `codex_approval_kind = "mcp_tool_call"`. A genuine elicitation (a server
    -- asking the user to fill a form) is not one, and we have no form.
    local meta = msg.params._meta or {}
    local ours = meta.codex_approval_kind == "mcp_tool_call"
      and msg.params.serverName == "sidekick"

    return { action = ours and "accept" or "decline", content = ours and vim.empty_dict() or vim.NIL }, ours
  elseif msg.method == "item/commandExecution/requestApproval"
      or msg.method == "item/fileChange/requestApproval" then
    return { decision = "decline" }, false
  elseif msg.method == "item/permissions/requestApproval" then
    return { permissions = vim.empty_dict(), scope = "turn" }, false
  end

  return nil, false
end

local function answer(session, msg)
  local result, approved = approval(msg)

  if result == nil then
    log(session, { "✗ unsupported server request: " .. msg.method })
    send(session, {
      jsonrpc = "2.0",
      id = msg.id,
      error = { code = -32601, message = "Unsupported server method: " .. msg.method },
    })
    return
  end

  log(session, { (approved and "✓ " or "✗ ") .. msg.method })
  send(session, { jsonrpc = "2.0", id = msg.id, result = result })
end

-- Keep activity readable as a single short line, even for multiline commands.
local function progress(emoji, text)
  text = text:gsub("%s+", " ")
  if vim.fn.strchars(text) > 100 then
    text = vim.fn.strcharpart(text, 0, 99) .. "…"
  end
  vim.print(emoji .. " " .. text)
end

local function tool_progress(item)
  local args = item.arguments
  if type(args) == "string" then
    local ok, decoded = pcall(vim.json.decode, args)
    args = ok and decoded or nil
  end
  args = type(args) == "table" and args or {}

  local target = "buffer " .. tostring(args.buffer or "?")
  local buffer = tonumber(args.buffer)
  if buffer and buffer >= 0 and buffer % 1 == 0 and vim.api.nvim_buf_is_valid(buffer) then
    local name = vim.api.nvim_buf_get_name(buffer)
    if name ~= "" then
      target = vim.fn.fnamemodify(name, ":.")
    end
  end

  if item.server == "sidekick" then
    if item.tool == "open_buffer" then
      progress("📂", "Open " .. (args.path or "?"))
    elseif item.tool == "read_buffer" then
      local range = args.start and (":" .. args.start .. (args["end"] and ("-" .. args["end"]) or "")) or ""
      progress("📖", "Read " .. target .. range)
    elseif item.tool == "edit_buffer" then
      progress("📝", "Edit " .. target .. (args.start and (":" .. args.start) or ""))
    elseif item.tool == "save_buffer" then
      progress("💾", "Save " .. target)
    elseif item.tool == "jump" then
      progress("📍", "Jump to " .. target .. ":" .. (args.line or "?"))
    elseif item.tool == "grep" then
      progress("🔎", "Search " .. (args.pattern or "?") .. (args.include and (" in " .. args.include) or ""))
    elseif item.tool == "glob" then
      progress("🗂️", "Find buffers matching " .. (args.pattern or "?"))
    elseif item.tool == "list_buffers" then
      progress("🗂️", "List open buffers")
    else
      progress("🛠️", "Run sidekick/" .. (item.tool or "?"))
    end
  else
    progress("🛠️", "Run " .. (item.server or "?") .. "/" .. (item.tool or "?"))
  end
end

local function item_progress(item)
  if item.type == "reasoning" then
    progress("🧠", "Thinking…")
  elseif item.type == "commandExecution" then
    progress("⚙️", "Running " .. (item.command or "a command"))
  elseif item.type == "mcpToolCall" then
    tool_progress(item)
  elseif item.type == "fileChange" then
    progress("📝", "Editing files…")
  end
end

local function on_message(session, msg)
  -- A response carries an id we issued; anything else is a server-initiated
  -- notification or request.
  if msg.id ~= nil and msg.method == nil then
    local on_result = session.pending[msg.id]
    session.pending[msg.id] = nil

    if msg.error ~= nil then
      log(session, { "! error: " .. vim.inspect(msg.error) })
      vim.notify("sidekick: codex: " .. vim.inspect(msg.error), vim.log.levels.ERROR)
    elseif on_result ~= nil then
      on_result(msg.result)
    end

    return
  end

  -- A server-initiated request blocks the turn until answered.
  if msg.id ~= nil then
    answer(session, msg)
    return
  end

  -- turn/interrupt needs the id of the turn in flight.
  if msg.method == "turn/started" then
    session.turn_id = msg.params.turnId
    log(session, { "", "── turn started ──" })
    progress("🚀", "Working…")
  elseif msg.method == "turn/completed" then
    session.turn_id = nil
    log(session, { "── turn completed ──" })
    local turn = msg.params.turn
    local status = type(turn) == "table" and turn.status or nil
    if status == "failed" then
      progress("❌", "Turn failed; see :b sidekick://codex")
    elseif status == "interrupted" then
      progress("⏸️", "Interrupted")
    else
      progress("✅", "Done")
    end
  elseif msg.method == "item/started" then
    item_progress(msg.params.item)
  elseif msg.method == "item/completed" then
    log(session, render(msg.params.item))
  elseif msg.method == "mcpServer/startupStatus/updated" then
    -- Where to look first when codex ignores the sidekick tools: if this never
    -- says "ready" for sidekick, the server never came up.
    local status = "mcp " .. msg.params.name .. ": " .. msg.params.status
    if present(msg.params.error) then
      status = status .. " — " .. vim.inspect(msg.params.error)
    end
    log(session, { status })
  elseif msg.method:find("error") or msg.method:find("warning") then
    -- These are the ones that explain an empty turn, so dump their payload.
    log(session, vim.split("! " .. msg.method .. " " .. vim.inspect(msg.params), "\n"))
  elseif not vim.startswith(msg.method, "item/") then
    -- Deltas are the noisy ones; everything else gets a bare line.
    log(session, { "  " .. msg.method })
  end
end

-- stdout arrives in chunks that may split a line in two: the last element of
-- each chunk is a fragment to be completed by the next one.
local function on_stdout(session, lines)
  for i, line in ipairs(lines) do
    session.buffered = session.buffered .. line

    if i < #lines then
      local ok, msg = pcall(vim.json.decode, session.buffered)
      session.buffered = ""

      if ok and type(msg) == "table" then
        on_message(session, msg)
      end
    end
  end
end

function M:spawn()
  if self.session ~= nil then
    vim.notify("sidekick: internal error", vim.log.levels.ERROR)
    return
  end

  local mcp_url = self.sidekick_url .. "/mcp/" .. self.pid

  local session =
    { next_id = 0, pending = {}, buffered = "", thread_id = nil, turn_id = nil, log = nil }
  self.session = session

  -- `-c` overrides the ~/.codex/config.toml value, which is how an MCP server
  -- is declared to codex (there is no --mcp-config flag). `auto` (of auto |
  -- prompt | writes | approve) means our own tools run without an approval
  -- round trip — they only touch buffers the user is already looking at.
  local ok, job = pcall(vim.fn.jobstart,
    {
      "codex", "app-server",
      "-c", 'mcp_servers.sidekick.url="' .. mcp_url .. '"',
      "-c", 'mcp_servers.sidekick.default_tools_approval_mode="auto"',
    },
    {
      on_stdout = function(_, lines)
        -- Ignore trailing output from a stopped or replaced process.
        if self.session == session then
          on_stdout(session, lines)
        end
      end,
      -- app-server logs to stderr; it is where a bad -c override or a crash
      -- shows up.
      on_stderr = function(_, lines)
        for _, line in ipairs(lines) do
          if line ~= "" then
            log(session, { "! " .. line })
          end
        end
      end,
      on_exit = function(_, code)
        if self.session == session then
          self.session = nil
        end
        log(session, { "! codex app-server exited (" .. code .. ")" })
      end,
    }
  )

  if not ok or job <= 0 then
    self.session = nil
    local reason = not ok and tostring(job)
      or (job == -1 and "codex executable not found or not executable"
        or "jobstart failed (invalid arguments or process creation failure)")
    vim.notify("sidekick: failed to start codex app-server: " .. reason, vim.log.levels.ERROR)
    return
  end
  session.job = job

  -- Handshake, then open the thread every notification will start a turn on.
  request(session, "initialize", {
    clientInfo = { name = "sidekick", version = "0.1.0" },
  }, function()
    send(session, { jsonrpc = "2.0", method = "initialized" })

    request(session, "thread/start", {
      cwd = vim.fn.getcwd(),
      model = self.config.default_model,
      -- What /nvim:monitor is to claude, said once for the whole thread instead
      -- of once per turn.
      developerInstructions = INSTRUCTIONS,
      -- "never" does not mean "allow": it makes anything needing approval fail
      -- outright, MCP tool calls included. "on-request" lets codex ask, and
      -- `answer` above replies.
      approvalPolicy = "on-request",
    }, function(result)
      session.thread_id = result.thread.id
      log(session, { "codex thread " .. session.thread_id })
      vim.notify("sidekick: codex log in :b sidekick://codex")
    end)
  end)
end

function M:notify(buf, file, _pid)
  if self.session == nil or self.session.thread_id == nil then
    vim.notify("sidekick: codex is not ready yet", vim.log.levels.WARN)
    return
  end

  -- The monitor loop claude.lua needs is just a turn here; how to behave was
  -- said once, in the thread's developer instructions.
  request(self.session, "turn/start", {
    threadId = self.session.thread_id,
    input = {
      {
        type = "text",
        text = "Buffer " .. buf .. " (" .. file .. ") was saved.",
      },
    },
  })
end

function M:interrupt()
  if self.session == nil or self.session.turn_id == nil then
    return
  end

  request(self.session, "turn/interrupt", {
    threadId = self.session.thread_id,
    turnId = self.session.turn_id,
  })
end

function M:restart()
  if self.session == nil then
    return
  end

  vim.fn.jobstop(self.session.job)
  self.session = nil

  self:spawn()
end

function M:change_model(model_name)
  -- Remembered here, applied by the next thread/start; switching the model of a
  -- live thread would mean restarting it.
  self.config.default_model = model_name
  self:restart()
end

return M
