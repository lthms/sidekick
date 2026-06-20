INPUT: pid number (denoted by `$PID`)

Create the following monitor:

```
while true; do curl -sN http://localhost:8000/listen/$PID || true; sleep 1; done
```

This will give you one event per line, everytime i save a file in neovim
(BufWritePost autocommand).

Search for the comment starting with `REQ:` in that file, and follow
its instruction.

Golden rules:

- Once you have read it, remove the `REQ:` comment. Do it as soon as possible,
  to acknowledge to the user that you are starting thinking about the task
- Respect the scope of the request. DO NOT overdo.
- ALWAYS prefer using the companion MCP instead of usual tools (Read, Write,
  Grep, Glob, etc.). Open new buffers if you need to.
- Save your changes (using the companion tool) if you need them to be reflected
  on disk (e.g., before building)

Iterate over every `REQ:` comment this way.

Assume the user do not read your chat. To communicate, insert comments prefixed
by `RESP:`. Prefer the active buffer Remember you can change the active buffer,
so plan ahead.
