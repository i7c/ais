# ais acceptance suite

Black-box tests of the `ais` command line. They are the contract a port of ais
(to Go, `docs/flow-proposal.md` §13) has to meet, so they assert what callers
depend on: exit codes, stdout, the `ais state` / `ais env` JSON API (api 1),
`sessions.json` (version 1), files, git state and tmux state. They avoid
pinning messages meant for people.

```
go test ./acceptance/                      # against ../ais (the Python one today)
AIS_BIN=/path/to/ais go test ./acceptance/ # against another build
```

About 40 s on agalloch. Needs tmux, git, bash and `script` (util-linux).

## Isolation

Each test builds a lab (`lab_test.go`):

- **Its own `HOME`, `AIS_HOME` and worktree base** under `t.TempDir()`, with an
  environment built from nothing. No `TMUX`, `AIS_*` or `CLAUDE_*` leaks in
  from the session running the tests.
- **Its own tmux server on a private socket** (`tmux -S`). `TMUX_TMPDIR` is
  never set: once, that took down the real server.
- **`ais` on the lab's PATH is the binary under test**, so hooks and the fake
  harness call it by name.
- **`testdata/fake` plays the agent.** It records argv, cwd and env, then exits
  when the test writes an exit code for it. `--report` and `--nested-report`
  make it act like a SessionStart hook.
- **`testdata/sink` and `testdata/notify-send`** stand in for the notification
  routes.
- **`Lab.Attach`** puts a real client on the server through a pty, so "on
  screen" and "seen" can be tested.

## What is covered

| Area | Files |
|---|---|
| run, `--` form, dry run, refusals, pane marks, window naming, reaping | `run_test.go`, `race_test.go` |
| worktrees: `-w`, `from` modes, detach, branch reuse, copy, `AIS_WT_BASE`, `auto`, `ais wt` | `worktree_test.go` |
| `state` schema and filters, `env` | `api_test.go` |
| notify via sink, fallback, no backend, on-screen suppression, seen hooks, `ls`/`status` | `notify_test.go` |
| `_session-seen` guard, restart, fork, name, current, select, prune | `session_test.go` |
| install (claude, pi), instructions | `install_test.go` |
| legacy and other-host records, old config spelling, starter config | `compat_test.go` |

Checked on 2026-11-20: three hand-made bugs in ais were each caught by at
least one test. The bugs were reordered argv parts, a missing `AIS_SIBLING`,
and a renamed API key.

Not covered:

- the interactive `select` picker (fzf or numbered prompt) and `_preview`;
- macOS notification delivery;
- the `seen_after` delay (the lab sets it to 0);
- `restart` with no record of `harness_args` beyond the one legacy case.

## Found while writing it

- **Fixed:** a session could be reaped as dead while it was still starting.
  The record had no pid until the harness was spawned, and tsm polls `state`
  all the time. ais now records `launcher_pid`. The regression test is
  `race_test.go`.
- **Known issue:** `ais wt prune` is meant to drop git's registrations of
  worktrees that were deleted by hand. It finds repos only through worktrees
  that still exist, so a repo whose session worktrees are all gone keeps them
  listed as "prunable" in `git worktree list`. Fixing it means recording each
  worktree's main checkout, which `ais wt add` does not do today.
