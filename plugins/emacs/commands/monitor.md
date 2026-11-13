INPUT: `$1` (the sidekick server URL) `$2` (the session pid)

Create the following Monitor:

```
while true; do curl -sN $1/listen/$2 || true; sleep 1; done
```

/!\ You need to create a Monitor, NOT a Shell

This will give you one event per line, everytime i ping you from emacs.

The user leaves requests in two forms:

- `REQ:` comments written in a buffer. The event gives you that buffer's name
  (the `buf` field): search it for the comment starting with `REQ:`, and
  follow its instruction.
- Threads: requests attached to lines but drawn above them rather than written
  into the buffer, so `read_buffer` and `grep` never see them. When the event
  has a `thread` field, that thread holds the request. On every event, call
  `list_threads` too, and handle each open thread it lists.

Golden rules:

- Once you have read it, insert an `ACK:` comment right below the `REQ:`
  comment, acknowledging you started processing the request. Do it as soon as
  possible. `ACK:` is reserved for that first reply: every later comment about
  the request (progress, findings, questions, the answer) is prefixed by
  `RESP:`.
- Repeat the tag on every line of a comment spanning several lines: each line
  of a multi-line answer starts with `RESP:`. The user does the same, so
  consecutive `REQ:` lines are one request.
- NEVER delete or edit a `REQ:` comment: it stays in the buffer as the record
  of what was asked, and your replies go below it.
- A `REQ:` comment already followed by an `ACK:` or `RESP:` comment has been
  handled before: skip it.
- Respect the scope of the request. DO NOT overdo.
- ALWAYS prefer using the sidekick MCP instead of usual tools (Read, Write,
  Grep, Glob, etc.). Open new buffers if you need to.
- Buffers are identified by their emacs buffer name (e.g. `main.go`), exactly
  as reported by `list_buffers` and the `buf` field.
- Save your changes (using the sidekick tool) if you need them to be reflected
  on disk (e.g., before building)
- Rename with `rename_symbol` and use `code_actions` for fixes and
  refactorings rather than hand-editing every site.

Iterate over every unhandled `REQ:` comment this way.

Threads follow the same rules, through the thread tools instead of comments:
reply with `reply_thread`, kind `ACK` first and `RESP` afterwards. NEVER write
`REQ:`, `ACK:` or `RESP:` comments into a buffer for a thread. A thread whose
last entry is a `REQ` is open, even if you answered it before: it is a
follow-up, so read the whole thread for context. The lines a thread is
attached to are the code its request is about.

Assume the user do not read your chat. To communicate, insert comments prefixed
by `RESP:` (or `ACK:` for your first reply to a request) when answering a
`REQ:` comment, reply on the thread when answering a thread, and otherwise
open a thread with `new_thread` rather than writing a comment. Prefer the
active buffer. Remember you can change the active buffer with the jump tool (the user
can press M-, to come back), so plan ahead.
