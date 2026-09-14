# Sidekick

<p align="center"><img src="sophie.png" alt="Turn Claude into your own sidekick" width="300px"></p>

Drive Claude Code from your editor. Sidekick is a small daemon that bridges
your editor and a `claude` session: you keep writing code, leave a request as a
comment, and Claude answers in place.

## Getting Started

```sh
go build -o sidekick .
./sidekick --port 8000   # or SIDEKICK_PORT
```

Each session is bootstrapped with a slash command that ships as a Claude Code
plugin, so install the marketplace once:

```
/plugin marketplace add lthms/sidekick
/plugin install emacs@sidekick    # or nvim@sidekick, or vscode@sidekick
```

Without it, `claude` never attaches to the daemon and `REQ:` comments are
silently ignored.

### Neovim

Install the Neovim plugin provided in this repository. For instance, with
[lazy.nvim](https://github.com/folke/lazy.nvim):

```lua
{
    "lthms/sidekick",
    lazy = false,
    opts = {},
}
```

After leaving a comment starting with `REQ:`, you can use `:SidekickNotify` to
nudge the background Claude session to read it. Sidekick exposes a MCP server
allowing it to interact with your editor (opening, reading, writing buffers,
etc.).

### Emacs

Install `emacs@sidekick` (see above), load the Emacs plugin (see
`emacs/sidekick.el`), then run `M-x sidekick-setup`.

After leaving a comment starting with `REQ:`, you can use `M-x sidekick-notify`
to nudge the background Claude session to read it. Sidekick exposes the same
MCP server, plus a few Emacs-native tools (`xref`-based definition/reference
lookup and buffer diagnostics).

### VSCode

Requires VSCode 1.85 or later, Node.js 20 or later (what `@vscode/vsce` needs),
and the daemon running (see above).

The extension lives in `vscode/` and is not published on the Marketplace, so
build it from this repository:

```sh
cd vscode
npm install
npm run compile
```

To install it, package that build and hand the `.vsix` to VSCode, both from
`vscode/`:

```sh
npx @vscode/vsce package
code --install-extension sidekick-vscode-0.1.0.vsix
```

`npx` fetches `vsce` on demand, so there is nothing to install globally.

To work on the extension rather than install it, open `vscode/` as the workspace
folder and press F5 (`Run Extension`): a second VSCode window opens with the
extension loaded from `out/`. Open a folder in it, since a session needs one,
and keep `npm run watch` running to recompile on save.

Opening a folder registers the window and starts the background Claude session,
installing `vscode@sidekick` on the way if it is missing (set
`sidekick.claude.autoInstall` to `false` to keep plugin management to yourself).
When the daemon was not running yet, `Sidekick: Setup` retries the handshake.

After leaving a comment starting with `REQ:`, you can use `Sidekick: Notify` to
nudge the background Claude session to read it. Sidekick exposes the same MCP
server as the Emacs integration, backed by VSCode's language servers for
definition/reference lookup and diagnostics, and by a quick pick for questions
Claude puts to you. `Sidekick: Interrupt`, `Sidekick: Restart` and `Sidekick:
Change Model` drive the session itself.
