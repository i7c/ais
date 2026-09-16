# ais — ai-session

Starts an agent harness (e.g. Claude Code) with the right flags and env,
tracks the run in `~/.ais/sessions.json`, and lets you jump back to its tmux
pane later. Sessions are tracked only inside tmux — a session ais can't point
back to isn't worth recording.

## Install

```
./ais install
```

Wires up a harness (default `claude`): adds its flags to `~/.ais/config.toml`,
a `SessionStart` hook in `~/.claude/settings.json`, and a section in
`~/.claude/CLAUDE.md` telling the agent how to use `ais` from inside a
session. Use `--harness NAME`, `-n/--dry-run` to preview, `--force` to rewrite
an existing hook.

## Starting a session

```
ais -d "fix the auth bug" -- claude
```

- `-d, --description TEXT` — what the session is for; shown by `select`
- `-n, --dry-run` — print the command and env, run nothing

## Commands

| Command | What it does |
|---|---|
| `select [id]` | Switch tmux to a session's pane. Recently-notified sessions sort first (marked `●`). `--client NAME` targets a client tmux can't infer. |
| `ls [-a]` | List active sessions (`-a` includes dead ones) with time since last notification. |
| `status [-a]` | `ls` plus the notifications themselves. |
| `notify TEXT` | Raise a desktop notification and record it. Skipped (but still recorded) if this session's pane is already on screen; `-f/--force` sends anyway. `-t/--title`, `--tag`, `-q/--quiet`. |
| `current` | Show the session this shell is running in. |
| `name [--set N]` | Print (or set) this session's stable base name, for worktrees/branches. |
| `restart [id]` | Re-run a dead session's harness in this pane, resuming its conversation. Keeps directory and name, gets a new ais id. `--fresh` for a new conversation. |
| `prune [--all]` | Drop dead sessions older than `--days` (default 7). |

## Files

- `~/.ais/config.toml` — per-harness flags/env, `[tmux]` and `[notify]` settings
- `~/.ais/sessions.json` — session state

## tmux integration

A session renames its window after itself while running, and restores the old
name on exit (disable via `rename = false` under `[tmux]`).

```
bind-key a display-popup -E -w 80% -h 60% -T " ais " \
  "~/git/ais/ais select"
```

Picking a session from that popup switches the underlying client and the
popup closes on its own. Don't pass `--client '#{client_name}'` — popups hand
commands to the shell unexpanded, so `select` works out the client itself.

See `ais --help` for the full reference.
