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
/plugin install emacs@sidekick    # or nvim@sidekick
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

Once a session starts, `global-sidekick-annotation-mode` gives `REQ:`, `ACK:`
and `RESP:` comments a background color (customize `sidekick-req-face`,
`sidekick-ack-face` and `sidekick-resp-face`).

Requests can also be *threads*, kept out of the file: `M-x sidekick-thread-add`
(`a` in `sidekick-command-map`) asks Claude about the selected lines, or the
current one, and draws the request above them, followed by Claude's
`ACK:`/`RESP:` replies as they arrive. Nothing is written into the buffer, so
threads are never saved and vanish with their buffer;
`sidekick-thread-materialize` (`m`) turns one into comments to keep it. Asking
again on a thread's lines adds a follow-up. `]`/`[` move between threads, `l`
lists them all, `t` collapses them to one line each, `x` resolves (deletes) the
one at point and `X` all of the buffer's (every buffer's with `C-u`). Threads
are drawn over the code, so point cannot enter them: `v` shows the one at point
as plain text in a window, where it can be selected, copied, searched and
followed up.
