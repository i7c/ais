# ais — ai-session

Starts a process from a named **profile** — the right command, flags and env —
tracks the run in `~/.ais/sessions.json`, and lets you jump back to its tmux
pane later. The first profiles are agent harnesses (Claude Code, pi), which
also get their conversation resumed on restart; any other process you start
the same way often enough deserves one too. Sessions are tracked only inside
tmux — a session ais can't point back to isn't worth recording.

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
ais run claude -d "fix the auth bug"
ais run pi -w -d "port the parser" -- --thinking high
ais -w -d "port the parser" -- pi --thinking high   # the same, abbreviated
ais run --fork <id>                                 # a second agent beside <id>
```

- `-d, --description TEXT` — what the session is for; shown by `select`
- `-w, --worktree` — give the session its own git worktree of the repo you're
  standing in, and start it there (see below). Fails if you're not in a repo.
- `--no-worktree` — run right here, even with `worktree.auto` on
- `--cwd DIR` — start in `DIR` instead of here
- `--fork ID` — start a copy of session `ID` beside it: same profile, directory
  and worktree, conversation forked through the profile's `session.fork`
- `-n, --dry-run` — print the command and env, run nothing

## Profiles

```toml
[profile.claude]
flags = ["--dangerously-skip-permissions"]

[profile.claude.session]          # how it names conversations: restart/fork
id = "uuid4"
new = ["--session-id", "{id}"]
resume = ["--resume", "{id}"]
fork = ["--resume", "{id}", "--fork-session"]

[profile.api]                     # not every profile is an agent
command = ["just", "dev"]
env = { PORT = "8001" }
```

`[harness.<name>]` is the old spelling and is still read; where both exist,
`[profile.<name>]` wins key by key and `env` merges.

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
| `state` | Every profile and session as JSON — the API other programs (tsm) read. `--active`, `--since <notification id>`. |
| `env <id>` | Where a session runs and the env a shell beside it needs, as shell to `eval` or `--json`. |
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

## Notifications from servers

`[notify] sink` hands every notification to a command first, as JSON on
stdin; it answers `{"result": "shown"}` or `{"result": "suppressed"}`, or
declines by exiting non-zero, and ais then delivers it itself. With
`sink = ["~/.local/bin/tsm", "notify", "--stdin"]`, notifications raised on a
server reach the laptop looking at it through `tsm relay` — see
[tsm](tsm/README.md#notifications-from-anywhere-tsm-relay).

## Files

- `~/.ais/config.toml` — per-harness flags/env, `[worktree]`, `[tmux]`, `[notify]`
- `~/.ais/sessions.json` — session state

Anything else is the harness's own: `ais install` touches `~/.claude/` or
`~/.pi/agent/` and backs up every file it changes as `<name>.ais.bak`.

## tsm

[`tsm/`](tsm/README.md) is a tmux session manager built on top of ais: one
popup over every session, window and pane, with the agents ais started
labelled, the ones calling for you first, and ended sessions a key away from
restarting. It reads ais only through `ais state` and `ais env`.

## tmux integration

A session renames its window after itself while running, and restores the old
name on exit (disable via `rename = false` under `[tmux]`).

While it runs it also marks its pane with `@ais_id`, `@ais_profile`,
`@ais_desc` and `@cwd`, and ais itself stands in the session's directory — so
`#{pane_current_path}`, and every split made from it, lands in the agent's
worktree. Panes can be moved freely: ais follows them by pane id.

## API

`ais state` and `ais env --json` are a contract, not a dump of
`sessions.json`: both carry an `"api"` version, and readers must ignore keys
they don't know. Each session says what can be done with it (`can_restart`,
`can_resume`, `can_fork`); notifications carry ids that sort by time, which
is what `--since` compares.

```
bind-key a display-popup -E -w 80% -h 60% -T " ais " \
  "~/git/ais/ais select"
```

(or bind `a` to [tsm](tsm/README.md), which covers `select` and more)

Picking a session from that popup switches the underlying client and the
popup closes on its own. Don't pass `--client '#{client_name}'` — popups hand
commands to the shell unexpanded, so `select` works out the client itself.

See `ais --help` for the full reference.
