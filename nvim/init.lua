local M = {}

local SERVER_URL = "http://127.0.0.1:8000"

local rpc_id = 0

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
    SERVER_URL,
  }, { text = true }, function(out)
    if out.code ~= 0 then
      vim.schedule(function()
        vim.notify("companion: " .. method .. " failed: " .. (out.stderr or ""), vim.log.levels.WARN)
      end)
    end
  end)
end

local function on_start()
  local pid = vim.fn.getpid()
  print(pid)
  local rpc_addr = vim.fn.serverstart("127.0.0.1:0")
  rpc_request("register", { pid = pid, endpoint = rpc_addr })

  -- Generate an MCP config pointing this session's claude at the companion
  -- server's per-pid endpoint, served over SSE at /mcp/<pid>.
  local mcp_config = vim.fn.tempname()
  vim.fn.writefile({
    vim.json.encode({
      mcpServers = {
        companion = {
          type = "sse",
          url = SERVER_URL .. "/mcp/" .. pid,
        },
      },
    }),
  }, mcp_config)

  -- Spawn a `claude` terminal in its own buffer, without stealing focus. The
  -- buffer stays in the background; the user can select it later.
  local prev_buf = vim.api.nvim_get_current_buf()
  local term_buf = vim.api.nvim_create_buf(true, false)
  vim.api.nvim_win_set_buf(0, term_buf)
  vim.fn.jobstart({ "claude", "--mcp-config", mcp_config, "--", "/nvim " .. pid }, { term = true })
  vim.api.nvim_win_set_buf(0, prev_buf)
end

local function on_buf_write()
  local buf = vim.api.nvim_get_current_buf()
  local file = vim.api.nvim_buf_get_name(buf)
  local pid = vim.fn.getpid()
  rpc_request("notify", {buf = buf, file = file, pid = pid} )
  vim.print("Notification sent to Claude")
end

function M.setup()
  local group = vim.api.nvim_create_augroup("Companion", { clear = true })
  vim.api.nvim_create_autocmd("VimEnter", {
    group = group,
    callback = on_start,
  })
  vim.api.nvim_create_user_command("CompanionNotify", on_buf_write, {
    desc = "Notify the companion server about the current buffer"
  })
end

return M

