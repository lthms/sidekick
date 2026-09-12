-- This Source Code Form is subject to the terms of the Mozilla Public
-- License, v. 2.0. If a copy of the MPL was not distributed with this
-- file, You can obtain one at https://mozilla.org/MPL/2.0/.

-- Maintainer: Thomas Letan <lthms@soap.coffee>

local M = {}

M.defaults = {
  server_url = "http://127.0.0.1:8000",
  backend = "codex",
  claude = {
    default_model = "opus",
    auto_install = true,
    marketplace = {
      path = nil,
      repo = "lthms/sidekick",
      ref = "main",
    },
  },
  codex = {
    -- nil: let codex pick its own default, instead of pinning an id that goes
    -- stale (`model/list` is the source of truth, e.g. gpt-6-astra today).
    default_model = nil,
  },
}

-- Merge the user options over the defaults, and return the resolved config.
function M.setup(opts)
  return vim.tbl_deep_extend("force", {}, M.defaults, opts or {})
end

return M
