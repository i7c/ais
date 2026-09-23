# ais — ai-session

Starts an agent harness (e.g. Claude Code) with the right flags and env,
tracks the run in `~/.ais/sessions.json`, and lets you jump back to its tmux
pane later. Sessions are tracked only inside tmux — a session ais can't point
back to isn't worth recording.

## Install

```
./ais install                # claude
./ais install --harness pi   # pi
```

Wires a harness up three ways: session flags in `~/.ais/config.toml` so
`restart` can resume a conversation, something that tells `ais` which
conversation the harness is in now, and a section in the harness's global
instructions telling the agent how to use `ais` from inside a session.

| Harness | Reports its conversation through | Instructions land in |
|---|---|---|
| `claude` | a `SessionStart` hook in `~/.claude/settings.json` | `~/.claude/CLAUDE.md` |
| `pi` | a `session_start` extension at `~/.pi/agent/extensions/ais.ts` | `~/.pi/agent/AGENTS.md` |

The instructions are one shared text; only the sentences naming the harness
differ. They go between `<!-- ais:begin -->` and `<!-- ais:end -->`, so a later
install replaces its own section and nothing else. `-n/--dry-run` previews,
`--force` rewrites wiring `ais` did not write itself.

## Starting a session

```
ais -d "fix the auth bug" -- claude
ais -w -d "port the parser" -- pi --thinking high
```

- `-d, --description TEXT` — what the session is for; shown by `select`
- `-w, --worktree` — give the session its own git worktree of the repo you're
  standing in, and start the harness there (see below). Fails if you're not in
  a repo.
- `--no-worktree` — run right here, even with `worktree.auto` on
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
| `restart [id]` | Re-run a dead session's harness in this pane, resuming its conversation. Keeps directory, name and worktree, gets a new ais id. `--fresh` for a new conversation. |
| `wt <cmd>` | This session's worktrees: `add [repo]`, `ls [-a]`, `path [repo]`, `prune`. |
| `prune [--all]` | Drop dead sessions older than `--days` (default 7), and the finished worktrees of the ones it drops. `--keep-worktrees` to leave those alone. |

## Worktrees

One session, one directory — however many repos it turns out to need.

```
ais -w -d "fix the auth bug" -- claude   # starts in ~/wt/fix-auth-bug/<repo>
```

Everything that session makes lands under `~/wt/<session name>/`, one
subdirectory per repo, all on a branch named after the session — so the
directory, the branch and `ais name` always agree, and two sessions never share
a checkout. The agent gets `$AIS_WORKTREE` and `$AIS_WORKTREE_BASE`, and reaches
for another repo through `ais wt`:

```
cd "$(ais wt add ~/git/other-repo)"   # prints the path, and only the path
ais wt ls                             # branch and state of each one
ais wt path other-repo
ais wt prune                          # drop registrations for worktrees already gone
```

`ais prune` eventually removes the worktrees of the dead sessions it drops, but
never one holding uncommitted changes or unpushed commits, and never one a
surviving session is still standing in.

This replaces the standalone `wt` script. Configure it under `[worktree]`:

```toml
[worktree]
base = "~/wt"
copy = [".env", ".claude/settings.local.json"]   # untracked files a checkout needs
fetch = true
auto = false                                     # -w by default, where it applies
```

`$AIS_WT_BASE` overrides `base` for a single run.

`auto = true` makes a worktree the default: start a session inside a repo and it
gets one without `-w`, start one anywhere else and it just runs where you are.
Only a `-w` you typed yourself insists on a repo and fails without one; pass
`--no-worktree` to opt a single run out.

## Files

- `~/.ais/config.toml` — per-harness flags/env, `[worktree]`, `[tmux]`, `[notify]`
- `~/.ais/sessions.json` — session state

Anything else is the harness's own: `ais install` touches `~/.claude/` or
`~/.pi/agent/` and backs up every file it changes as `<name>.ais.bak`.

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
