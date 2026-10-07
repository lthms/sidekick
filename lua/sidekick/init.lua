-- This Source Code Form is subject to the terms of the Mozilla Public
-- License, v. 2.0. If a copy of the MPL was not distributed with this
-- file, You can obtain one at https://mozilla.org/MPL/2.0/.

-- Maintainer: Thomas Letan <lthms@soap.coffee>

local config = require("sidekick.config")
local Rpc = require("sidekick.rpc")

-- Agents sidekick knows how to drive, keyed by the `backend` config value.
local backends = {
  claude = require("sidekick.claude"),
  codex = require("sidekick.codex"),
}

local M = {}

M.config = {}
M.backend = nil

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

local function on_start()
  local pid = vim.fn.getpid()
  local rpc_addr = vim.fn.serverstart("127.0.0.1:0")
  Rpc.new(M.config.server_url):request("register", { pid = pid, app = "nvim", endpoint = rpc_addr })

  local backend = backends[M.config.backend]
  if backend == nil then
    vim.notify("sidekick: unknown backend " .. M.config.backend, vim.log.levels.ERROR)
    return
  end

  M.backend = backend.new(M.config, pid)
  M.backend:setup(function()
    M.backend:spawn()
  end)
end

-- User commands run against the session spawned at VimEnter. Wrap them so a
-- missing session is reported instead of erroring out.
local function with_backend(f)
  return function(o)
    if M.backend == nil then
      vim.notify("sidekick: no current session", vim.log.levels.WARN)
      return
    end

    f(M.backend, o)
  end
end

local function notify(backend)
  local buf = vim.api.nvim_get_current_buf()
  local file = vim.api.nvim_buf_get_name(buf)
  local pid = vim.fn.getpid()
  backend:notify(buf, file, pid)
end

function M.setup(opts)
  M.config = config.setup(opts)
  local group = vim.api.nvim_create_augroup("Sidekick", { clear = true })
  vim.api.nvim_create_autocmd("VimEnter", {
    group = group,
    callback = on_start,
  })
  vim.api.nvim_create_user_command("SidekickNotify", with_backend(notify), {
    desc = "Notify the current session"
  })
  vim.api.nvim_create_user_command("SidekickInterrupt", with_backend(function(backend)
    backend:interrupt()
  end), {
    desc = "Interrupt the current session"
  })
  vim.api.nvim_create_user_command("SidekickRestart", with_backend(function(backend)
    backend:restart()
  end), {
    desc = "Restart the current session from scratch"
  })
  vim.api.nvim_create_user_command("SidekickModel", with_backend(function(backend, o)
    backend:change_model(o.args)
  end), {
    nargs = "+",
    desc = "Request a change of model"
  })
end

return M
