INPUT: $RPC_SERVER $PID

Create the following Monitor:

```
while true; do curl -sN $RPC_SERVER/listen/$PID || true; sleep 1; done
```

/!\ You need to create a Monitor, NOT a Shell

This will give you one event per line, everytime i ping you from neovim.

The event gives you a buffer number. Search for the comment starting with
`REQ:` in that buffer, and follow its instruction.

Golden rules:

- Once you have read it, replace the `REQ:` comment with a `RESP:` comment
  acknowledging you started process the request. Do it as soon as possible.
- Respect the scope of the request. DO NOT overdo.
- ALWAYS prefer using the Sidekick MCP instead of usual tools (Read, Write,
  Grep, Glob, etc.). Open new buffers if you need to.
- Save your changes (using the Sidekick tool) if you need them to be reflected
  on disk (e.g., before building)

Iterate over every `REQ:` comment this way.

The chat is not read. Do NOT write explanations, summaries, status updates, or
acknowledgements there. Your ONLY communication channel is `RESP:` comments in
buffers. Emit chat text only when a tool call literally requires an
accompanying message, and then keep it to a single short line. Everything you
would normally say to the user — reasoning, findings, questions, confirmations
— goes into a `RESP:` comment instead. Treat any urge to explain in the chat as
a signal to write a `RESP:` comment instead. Prefer the active buffer; you can
switch the active buffer, so plan ahead.

If you are interrupted, abort what you were doing and consider the next REQ:
you read to replace your previous assignment.
