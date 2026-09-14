# Sidekick

Drive Claude Code from your editor. Leave a `REQ:` comment where you want help,
and a background Claude session answers in place, in your own buffers, without
ever taking the focus.

## How it works

A sidekick daemon bridges this window and a background `claude` session: the
extension registers the window on startup, then starts Claude in a terminal it
never shows. Leave a comment starting with `REQ:`, run **Sidekick: Notify**, and
Claude turns it into a `RESP:` acknowledgement and works from there, editing
through the editor so every change lands in your buffers for you to review, undo
or save. Answers come back as `RESP:` comments, never as chat.

## Commands

| Command | Description |
| --- | --- |
| `Sidekick: Notify` | Point Claude at the active editor, where you left a `REQ:` comment. |
| `Sidekick: Interrupt` | Interrupt whatever the session is currently doing. |
| `Sidekick: Restart` | Replace the session with a fresh one, reusing this window's registration. |
| `Sidekick: Change Model` | Switch the running session to another model. |
| `Sidekick: Setup` | Retry the handshake with the daemon, once it is running. |
| `Sidekick: Show Pending Question` | Bring back the question the session is waiting on. |

## Settings

| Setting | Default | Description |
| --- | --- | --- |
| `sidekick.serverUrl` | `http://127.0.0.1:8000` | URL of the sidekick daemon. |
| `sidekick.claude.defaultModel` | `opus` | Model the session is started with. |
| `sidekick.claude.autoInstall` | `true` | Install the `vscode@sidekick` plugin when it is missing. Without it a session has no command to bootstrap itself with, and none is started. |
| `sidekick.claude.marketplace.repo` | `lthms/sidekick` | Marketplace repository the plugin is installed from. |
| `sidekick.claude.marketplace.ref` | `main` | Git ref of that repository. |
| `sidekick.claude.marketplace.path` | `null` | Local marketplace checkout, used instead of the repository when set. |

## Requirements

- The `claude` CLI, on your `PATH`.
- The sidekick daemon, running and reachable at `sidekick.serverUrl`.

The daemon lives in [the sidekick
repository](https://github.com/lthms/sidekick), whose README covers building it,
running it, and installing the Claude Code plugin that bootstraps each session.
