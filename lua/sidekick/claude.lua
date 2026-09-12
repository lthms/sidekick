-- This Source Code Form is subject to the terms of the Mozilla Public
-- License, v. 2.0. If a copy of the MPL was not distributed with this
-- file, You can obtain one at https://mozilla.org/MPL/2.0/.

-- Maintainer: Thomas Letan <lthms@soap.coffee>

local M = {}
local Rpc = require("sidekick.rpc")

-- `M` is both the module and the instance metatable: `__index = M` makes
-- methods defined below reachable from every instance.
M.__index = M

function M.new(config, pid)
  local mcp_config = vim.fn.tempname()
  vim.fn.writefile({
    vim.json.encode({
      mcpServers = {
        sidekick = {
          type = "http",
          url = config.server_url .. "/mcp/" .. pid,
        },
      },
    }),
  }, mcp_config)

  return setmetatable(
    {
      rpc = Rpc.new(config.server_url),
      mcp_config = mcp_config,
      pid = pid,
      config = config.claude,
      sidekick_url = config.server_url,
      session = nil
    },
    M
  )
end

function M:notify(buf, file, pid)
  self.rpc:request("notify", {buf = buf, file = file, pid = pid})
  vim.print("Notification sent to Claude")
end

function M:setup(on_ready)
  -- Ensure the plugin is installed, then spawn — all without blocking startup.
  vim.system({ "claude", "plugin", "list", "--json" }, { text = true }, function(list_out)
    if (list_out.stdout or ""):find('"nvim@sidekick"', 1, true) then
      vim.schedule(on_ready)
      return
    end

    if self.config.auto_install then
      local marketplace = ""
      if self.config.marketplace.path ~= nil then
        marketplace = self.config.marketplace.path
      else
        marketplace = self.config.marketplace.repo .. "#" .. self.config.marketplace.ref
      end
      vim.system({ "claude", "plugin", "marketplace", "add", marketplace }, {}, function()
        vim.system({ "claude", "plugin", "install", "nvim@sidekick" }, {}, function()
          vim.schedule(on_ready)
        end)
      end)
    else
      vim.schedule(function()
        vim.notify(
          "sidekick: nvim@sidekick is not installed and auto_install is off",
          vim.log.levels.ERROR
        )
      end)
    end
  end)
end

function M:spawn()
  if self.session ~= nil then
    vim.notify("sidekick: internal error", vim.log.levels.ERROR)
    return
  end

  self.session = {}

  self.session.buf = vim.api.nvim_create_buf(true, false)
  vim.api.nvim_buf_call(self.session.buf, function()
    vim.fn.jobstart(
      {
        "claude", "--mcp-config", self.mcp_config,
        "--allowedTools", "mcp__sidekick",
        "--model", self.config.default_model,
        "--", "/nvim:monitor " .. self.sidekick_url .. " " .. self.pid
      },
      { term = true }
    )
  end)

  vim.print("sidekick: Claude Code is running in buffer " .. self.session.buf)
end

function M:job()
  if self.session == nil then
    return
  end

  local buf = self.session.buf
  if not buf or not vim.api.nvim_buf_is_valid(buf) then
    return
  end

  -- A terminal buffer exposes its PTY job as the buffer-local variable
  -- `terminal_job_id`.
  local ok, job = pcall(vim.api.nvim_buf_get_var, buf, "terminal_job_id")
  if not ok or not job then
    return
  end

  return job
end

function M:send(data)
  local job = self:job()
  if job then
    vim.fn.chansend(job, data)
  end
end

function M:interrupt()
  -- Sending the raw Ctrl-C byte (ETX, "\003") to claude
  self:send("\003")
end

function M:restart()
  if self.session == nil then
    return
  end

  self:interrupt()

  local buf = self.session.buf
  if buf and vim.api.nvim_buf_is_valid(buf) then
    -- force delete: the terminal job is still attached, so drop it too.
    vim.api.nvim_buf_delete(buf, { force = true })
  end
  self.session = nil

  self:spawn()
end

local function inject_user_prompt(self, prompt, on_inject)
  self:send(prompt)
  -- Claude TUI has autocomplete features and sending <Enter> too quickly may
  -- mess with the prompt if sent too quickly. So we wait a little just in
  -- case.
  vim.defer_fn(function()
    self:send("\r")
    if on_inject ~= nil then
      on_inject()
    end
  end, 300)
end

function M:change_model(model_name)
  inject_user_prompt(self, "/model " .. model_name, function()
    -- Waiting a little then sending <Enter> again, which is necessary to
    -- accept the “Switch model” modal
    vim.defer_fn(function() self:send("\r") end, 300)
  end)
end

return M
