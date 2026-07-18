-- This Source Code Form is subject to the terms of the Mozilla Public
-- License, v. 2.0. If a copy of the MPL was not distributed with this
-- file, You can obtain one at https://mozilla.org/MPL/2.0/.

-- Maintainer: Thomas Letan <lthms@soap.coffee>

local M = {}

M.config = {}
M.state = {}

local rpc_id = 0

local function stop_claude()
  -- A terminal buffer exposes its PTY job as the buffer-local variable
  -- `terminal_job_id`. Send the raw Ctrl-C byte (ETX, "\003") to that channel
  local buf = M.state.claude_buff
  if not buf or not vim.api.nvim_buf_is_valid(buf) then
    vim.print("No Claude session to interrupt")
    return
  end

  local ok, job = pcall(vim.api.nvim_buf_get_var, buf, "terminal_job_id")
  if not ok or not job then
    vim.print("No Claude session to interrupt")
    return
  end

  vim.fn.chansend(job, "\003")
end

function M.write_buf(buf, start, previous_content, new_content)
  local stop = start + #previous_content
  local current = vim.api.nvim_buf_get_lines(buf, start, stop, false)

  if #current ~= #previous_content then
    return { ok = false, reason = "range out of date: expected " ..
      #previous_content .. " lines, found " .. #current }
  end
  for i = 1, #previous_content do
    if current[i] ~= previous_content[i] then
      return { ok = false, reason = "content changed at line " .. (start + i - 1) }
    end
  end

  vim.api.nvim_buf_set_lines(buf, start, stop, false, new_content)
  return { ok = true }
end

local function rpc_request(method, params)
  rpc_id = rpc_id + 1

  local body = vim.json.encode({
    jsonrpc = "2.0",
    id = rpc_id,
    method = method,
    params = params,
  })

  vim.system({
    "curl", "-sS", "-X", "POST",
    "-H", "Content-Type: application/json",
    "-d", body,
    M.config.server_url,
  }, { text = true }, function(out)
    if out.code ~= 0 then
      vim.schedule(function()
        vim.notify("sidekick: " .. method .. " failed: " .. (out.stderr or ""), vim.log.levels.WARN)
      end)
    end
  end)
end

local function spawn_terminal(mcp_config, pid)
  vim.schedule(function()
    -- Spawn a `claude` terminal in its own buffer. The buffer stays in the
    -- background; the user can select it later.
    local term_buf = vim.api.nvim_create_buf(true, false)
    M.state.claude_buff = term_buf
    vim.api.nvim_buf_call(term_buf, function()
      vim.fn.jobstart(
        { "claude", "--mcp-config", mcp_config, "--", "/nvim:monitor " .. M.config.server_url .. " " .. pid },
        { term = true }
      )
    end)
    vim.print("Claude Code is running in buffer " .. term_buf)
  end)
end

local function on_start()
  local pid = vim.fn.getpid()
  local rpc_addr = vim.fn.serverstart("127.0.0.1:0")
  rpc_request("register", { pid = pid, editor = "nvim", endpoint = rpc_addr })

  -- Generate an MCP config pointing this session's claude at the sidekick
  -- server's per-pid endpoint, served over SSE at /mcp/<pid>.
  local mcp_config = vim.fn.tempname()
  vim.fn.writefile({
    vim.json.encode({
      mcpServers = {
        sidekick = {
          type = "http",
          url = M.config.server_url .. "/mcp/" .. pid,
        },
      },
    }),
  }, mcp_config)

  -- Ensure the plugin is installed, then spawn — all without blocking startup.
  vim.system({ "claude", "plugin", "list", "--json" }, { text = true }, function(list_out)
    if (list_out.stdout or ""):find('"nvim@sidekick"', 1, true) then
      spawn_terminal(mcp_config, pid)
      return
    end
    if M.config.claude.auto_install then
      local marketplace = ""
      if M.config.claude.marketplace.path ~= nil then
        marketplace = M.config.claude.marketplace.path
      else
        marketplace = M.config.claude.marketplace.repo .. "#" .. M.config.claude.marketplace.ref
      end
      vim.system({ "claude", "plugin", "marketplace", "add", marketplace }, {}, function()
        vim.system({ "claude", "plugin", "install", "nvim@sidekick" }, {}, function()
            -- Spawning claude requires to use function not marked “fast” (see :h
            -- api-fast). So we use vim.schedule to defer the function back to
            -- the main loop, where they can be executed.
            spawn_terminal(mcp_config, pid)
        end)
      end)
    end
  end)
end

local function on_buf_write()
  local buf = vim.api.nvim_get_current_buf()
  local file = vim.api.nvim_buf_get_name(buf)
  local pid = vim.fn.getpid()
  rpc_request("notify", {buf = buf, file = file, pid = pid} )
  vim.print("Notification sent to Claude")
end

local defaults = {
  server_url = "http://127.0.0.1:8000",
  claude = {
    auto_install = true,
    marketplace = {
      path = nil,
      repo = "lthms/sidekick",
      ref = "main",
    },
  },
}

function M.setup(opts)
  M.config = vim.tbl_deep_extend("force", {}, defaults, opts or {})
  local group = vim.api.nvim_create_augroup("Sidekick", { clear = true })
  vim.api.nvim_create_autocmd("VimEnter", {
    group = group,
    callback = on_start,
  })
  vim.api.nvim_create_user_command("SidekickNotify", on_buf_write, {
    desc = "Notify the sidekick server about the current buffer"
  })
  vim.api.nvim_create_user_command("SidekickInterrupt", stop_claude, {
    desc = "Interrupt the background Claude Code session"
  })
end

return M

