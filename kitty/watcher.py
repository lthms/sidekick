#!/usr/bin/env python3

import json
import os
import shutil
import subprocess
import urllib.request

SERVER_URL = os.environ.get("SIDEKICK_SERVER_URL", "http://127.0.0.1:8000")

def _rpc(method, params):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode()
    req = urllib.request.Request(
        SERVER_URL, data=body, headers={"Content-Type": "application/json"}, method="POST"
    )
    try:
        urllib.request.urlopen(req, timeout=2).read()
    except Exception:
        pass  # sidekick daemon not up yet: fail quiet, same as the nvim plugin


def _mcp_config(pid):
    path = f"/tmp/kitty-sidekick-mcp-{pid}.json"
    with open(path, "w") as f:
        json.dump(
            {"mcpServers": {"sidekick": {"type": "http", "url": f"{SERVER_URL}/mcp/{pid}"}}}, f
        )
    return path


# Paired with _cleanup_session(), which tears down the scratch dir and its
# ~/.claude.json trust entry from on_close when the sidekick window (and thus
# this kitty session) goes away.
def _trusted_project(pid):
    # Claude gates a fresh directory behind a "do you trust this folder?" dialog
    # that can't be answered by the background session. We create a per-pid scratch
    # dir and pre-seed that entry — hasTrustDialogAccepted skips the trust prompt,
    # hasCompletedProjectOnboarding skips the first-run onboarding.
    project = f"/tmp/kitty-sidekick-project-{pid}"
    os.makedirs(project, exist_ok=True)

    config = os.path.expanduser("~/.claude.json")
    try:
        with open(config) as f:
            data = json.load(f)
    except (FileNotFoundError, json.JSONDecodeError):
        data = {}

    projects = data.setdefault("projects", {})
    entry = projects.setdefault(project, {})
    entry["hasTrustDialogAccepted"] = True
    entry["hasCompletedProjectOnboarding"] = True

    with open(config, "w") as f:
        json.dump(data, f, indent=2)
    return project


def _cleanup_session(pid):
    # Undo everything _trusted_project()/_mcp_config() left on disk for this pid.
    project = f"/tmp/kitty-sidekick-project-{pid}"
    shutil.rmtree(project, ignore_errors=True)
    try:
        os.remove(f"/tmp/kitty-sidekick-mcp-{pid}.json")
    except FileNotFoundError:
        pass

    config = os.path.expanduser("~/.claude.json")
    try:
        with open(config) as f:
            data = json.load(f)
        if data.get("projects", {}).pop(project, None) is not None:
            with open(config, "w") as f:
                json.dump(data, f, indent=2)
    except (FileNotFoundError, json.JSONDecodeError):
        pass


def _is_sidekick_tab(tab):
    # A tab is "the sidekick tab" when it has windows and every one of them
    # carries the sidekick=1 user var we set at launch. Requiring *all* windows
    # avoids closing a tab where the user split a real shell alongside claude.
    windows = list(tab.windows)
    return bool(windows) and all(w.user_vars.get("sidekick") for w in windows)


def _plugin_installed():
    out = subprocess.run(
        ["claude", "plugin", "list", "--json"], capture_output=True, text=True
    ).stdout
    return "kitty@sidekick" in out


def _ensure_plugin():
    if _plugin_installed():
        return
    subprocess.run(["claude", "plugin", "marketplace", "add", "lthms/sidekick#main"])
    subprocess.run(["claude", "plugin", "install", "kitty@sidekick"])


def on_load(boss, data):
    socket = getattr(boss, "listening_on", "") or os.environ.get("KITTY_LISTEN_ON")
    if not socket:
        return  # remote control not enabled (no listen_on) — nothing to drive
    if os.environ.get("SIDEKICK_BOOTSTRAPPED"):
        return
    os.environ["SIDEKICK_BOOTSTRAPPED"] = "1"

    pid = os.getpid()  # the kitty process pid; the key everything is registered under
    _rpc("register", {"pid": pid, "app": "kitty", "socket": socket})
    mcp_config = _mcp_config(pid)
    project = _trusted_project(pid)
    _ensure_plugin()

    def _launch_sidekick(timer_id):
        boss.launch(
            "--type=tab", "--title", "sidekick", "--cwd", project, "--keep-focus",
            "--var", "sidekick=1",
            "claude", "--mcp-config", mcp_config,
            "--allowedTools", "mcp__sidekick",
            "--", f"/kitty:monitor {SERVER_URL} {pid}",
        )

    # Defer the launch to the next event-loop tick (0-delay timer) instead of
    # calling boss.launch() inline: launching mid-on_load races kitty's own
    # focus handling, and the new tab can steal focus despite --keep-focus.
    # Once on_load has returned, --keep-focus reliably lands the user back on
    # their original window and the claude session starts in the background.
    from kitty.fast_data_types import add_timer
    add_timer(_launch_sidekick, 0, False)


def on_close(boss, window, data):
    os_window_id = window.os_window_id

    if window.user_vars.get("sidekick"):
        _cleanup_session(os.getpid())

    # on_close fires at the start of Window.destroy(), before this window is
    # removed from its tab, so tm.tabs still counts it. Defer to the next event
    # loop tick so the count reflects the post-close state before we check it.
    def _maybe_close(timer_id):
        tm = boss.os_window_map.get(os_window_id)
        if not tm:
            return  # OS window already gone
        if len(tm.tabs) == 1 and _is_sidekick_tab(tm.tabs[0]):
            boss.mark_os_window_for_close(os_window_id)

    from kitty.fast_data_types import add_timer
    add_timer(_maybe_close, 0, False)
