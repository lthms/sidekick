-- This Source Code Form is subject to the terms of the Mozilla Public
-- License, v. 2.0. If a copy of the MPL was not distributed with this
-- file, You can obtain one at https://mozilla.org/MPL/2.0/.

-- Maintainer: Thomas Letan <lthms@soap.coffee>

local M = {}

local rpc_id = 0

M.__index = M

function M.new(server_url)
  return setmetatable({server_url = server_url}, M)
end

-- REQ: vim.schedule callback: /home/lthms/perso/sidekick/lua/sidekick/rpc.lua:35: attempt to concatenate upvalue 'method' (a table value)                                                                                                                    
-- stack traceback:                                                                                                                                                                                                                                      
--         /home/lthms/perso/sidekick/lua/sidekick/rpc.lua:35: in function </home/lthms/perso/sidekick/lua/sidekick/rpc.lua:34> 
function M:request(method, params)
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
    self.server_url,
  }, { text = true }, function(out)
    if out.code ~= 0 then
      vim.schedule(function()
        vim.notify("sidekick: " .. method .. " failed: " .. (out.stderr or ""), vim.log.levels.WARN)
      end)
    end
  end)
end

return M
