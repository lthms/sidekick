# Companion

Drive Claude Code from your editor. Companion is a small daemon that bridges
your editor and a `claude` session: you keep writing code, leave a request as a
comment, and Claude answers in place.

## Getting Started

```sh
go build -o companion .
./companion --port 8000   # or COMPANION_PORT
```

### Neovim

Load the neovim plugin (see `plugins/nvim`) pointing at the same `server_url`.

After leaving a comment starting with `REQ:`, you can use `:CompanionNotify` to
nudge the background Claude session to read it. Companion exposes a MCP server
allowing it to interact with your editor (opening, reading, writing buffers,
etc.).
