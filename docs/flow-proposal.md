# Proposal: `ais flow` — planned, dispatched agent sessions

Status: **built; steps 0–7 done** (2026-11-20)

## 1. What this is for

Today every agent session starts because I start it. When planning produces ten
tickets, I open the sessions, point them at the tickets and keep track of what
runs where. `ais flow` takes over the *starting and organising* of work. It
does not change how autonomous a session is once it runs.

The loop:

1. A **poller** (deterministic) looks at a workspace's task source on a
   schedule. If tickets are `ready`, it starts a **planner session**.
2. The **planner** (an agent, interactive) reads the ready tickets, proposes
   sessions, and waits for me. I decide; it queues what I picked, and the
   tickets go to `scheduled`. Ignored for 48 h, it closes with no action.
3. A **dispatcher** (deterministic) starts queued sessions within WIP and rate
   limits, each with its initial prompt, in its own tmux window, across as many
   repos as it needs. The tickets go to `In Progress`.
4. I mark a session **done** when I'm finished with it; that frees its slot.

Everything stays a normal ais session: tsm shows it, `ais notify` reaches me,
`ais restart` resumes it.

## 2. Decisions this builds on

From the requirements round and the survey (sources archived under
`~/.local/share/agents/sources/2026-11-20-agent-orchestration/`):

| Decision | Choice |
|---|---|
| Build or adopt | Build on ais. kandev was the only close fit, but it means a web UI instead of tmux and its Claude mode sits on the Agent SDK. |
| Runtime | No daemon. **systemd user timers** run one-shot `ais flow` commands; state lives in **SQLite** (`modernc.org/sqlite`, pure Go). |
| Where | agalloch only, local tmux. Remote dispatch later. |
| Harnesses | Claude Code required, signed-in binary (subscription). pi and others via ais profiles. |
| Task source | GitHub Projects v2 **Status** field: `ready` → `scheduled` → `In Progress`. Issue search with labels as a fallback source. |
| Planner | One per workspace, singleton, closed explicitly or after 48 h without my input. It always sees **every** `ready` ticket; whatever I don't accept stays `ready` for the next planner. |
| WIP | Live *dispatched* sessions count; hand-started ones don't. Global and per-workspace caps. A slot is freed **only** by an explicit `done`. |
| Rate | Global: while under WIP, ≤ 4 starts/hour and ≤ 16/day, rolling windows. Overridable from the command line for a while (`ais flow limit per_day 32 --until eod`). Manual start command exists. Usage-limit awareness later. |
| Queue | Local SQLite is the source of truth, plus a short comment on each ticket. Editable via CLI (and later tsm). |
| Language | **Go.** ais is ported to Go first, then flow is built in it (§13). |

Why not a workflow engine (DBOS, Temporal, pueue…): a handful of events per day
for one user. Everything they offer that we need — scheduling, mutual
exclusion, retries, logs — systemd already gives a one-shot unit, and the rest
is a few SQL rows. Nothing to keep running, nothing to upgrade.

## 3. Concepts

**Workspace** — a named scope of work (`soi`, `personal`). It has:

- a task source (project + Status field, or an issue search),
- the repos its sessions may touch, by short name,
- a tmux session where its agents live (created if missing),
- a default profile and model, env vars, WIP limit,
- a planner: profile, initial prompt (normally a skill), schedule, idle timeout.

**Item** — a ticket from the source: issue URL, title, repo, project item id,
status.

**Planner** — an ais session that proposes work for one workspace. States:
`open` → `decided` | `expired` | `aborted`.

**Proposal** — a session I agreed should happen. Stored by the planner via
`ais flow queue add`. Fields: workspace, title, tickets (1..n), repos (1..n),
profile, initial prompt, priority, dependencies (`after` another proposal, or
after a ticket closes). States: `queued` → `starting` → `running` → `done`;
plus `cancelled`.

**Run** — the binding of a proposal to an ais session id once dispatched.

## 4. Configuration

Workspaces go into `~/.ais/config.toml`, next to the profiles they use:

```toml
[flow]
wip = 4                   # live dispatched sessions, all workspaces together
per_hour = 4              # starts in any rolling 60 min (dispatcher only)
per_day = 16              # starts in any rolling 24 h
                          # all three apply across workspaces: one subscription

[workspace.soi]
tmux_session = "soi"
profile = "claude"                 # default for proposals that don't say
wip = 3                            # cap for this workspace, under flow.wip
env = { SOI_ENV = "dev" }

[workspace.soi.source]
kind = "github-project"            # or "github-search"
owner = "gpwsoi"
project = 3
field = "Status"
ready = "ready"
scheduled = "scheduled"
in_progress = "In Progress"

[workspace.soi.repos]              # short name -> main checkout
backend = "~/git/soi-backend"
iac = "~/git/soi-iac"
frontend = "~/git/soi-frontend"

[workspace.soi.planner]
profile = "claude"
prompt = "/plan-sessions soi"      # a skill, see §8
cwd = "~/git/soi-backend"          # where the planner runs; no worktree
schedule = "Mon..Fri *-*-* 09,14:00:00"    # systemd OnCalendar; every firing
                                           # with ready tickets and no open
                                           # planner starts one
idle_timeout = "48h"

[workspace.soi.session]            # defaults for dispatched sessions
prompt = "/work-tickets {tickets}\n\n{notes}"
```

Profiles gain one key, telling ais how to hand a harness its first prompt:

```toml
[profile.claude]
prompt = ["--", "{prompt}"]   # claude "<prompt>" stays interactive
[profile.pi]
prompt = ["--", "{prompt}"]   # pi "<msg>" likewise
```

The `--` keeps a prompt that starts with `-` from being read as a flag. A
leading `/` is fine without it: `claude "/work-tickets <url>"` runs the skill
with its arguments (verified, §17).

A profile without `prompt` can't be dispatched with one; `ais flow` says so at
`queue add`, not at 3 a.m.

## 5. State

`~/.ais/flow.db`, SQLite in WAL mode. ais keeps owning `sessions.json`; flow
refers to sessions by ais id and never writes that file.

```
planners   (id, workspace, ais_session, state, started_at, last_input_at,
            closed_at, reason)
overrides  (key, value, until, set_at)                -- temporary limits
proposals  (id, workspace, title, tickets JSON, repos JSON, profile, prompt,
            priority, after JSON, state, planner_id, created_at,
            ais_session, started_at, done_at, manual BOOL)
starts     (at, proposal_id, manual BOOL)          -- the rate-limit log
items      (workspace, url, item_id, status, title, seen_at)   -- source cache
gh_ids     (workspace, project_id, field_id, option ids JSON)  -- resolved once
events     (at, kind, subject, detail JSON)        -- audit trail for `ais flow log`
```

A partial unique index on `planners(workspace) WHERE state='open'` makes the
singleton a database fact, not a convention. Every command runs in one
`BEGIN IMMEDIATE` transaction, so a timer and my CLI can't race.

## 6. Commands

All under `ais flow`, all one-shot. `--json` on every read.

| Command | Who runs it | What it does |
|---|---|---|
| `ais flow plan <ws>` | planner timer, or me | Expire an idle planner; if none is open and the source has any `ready` item, start one. Run by hand, it is "plan now". |
| `ais flow ready <ws>` | planner agent | List all `ready` items. |
| `ais flow queue add …` | planner agent | Store a proposal, set its tickets to `scheduled`, comment on them. |
| `ais flow queue [ls\|edit\|mv\|cancel] …` | me | Inspect, change priority, edit prompt, cancel (tickets go back to `ready`). |
| `ais flow plan done` | planner agent | Mark the current planner `decided`; its window closes on the next tick. |
| `ais flow dispatch` | dispatch timer | Reconcile, then start queued proposals while limits allow. |
| `ais flow dispatch --now <id>` | me | Start one proposal now, past WIP and rate limits (still logged and counted). |
| `ais flow start <id>` | the new tmux pane | Make worktrees, set `In Progress`, exec the harness through ais. Internal. |
| `ais flow done [<ais id>]` | me, or the agent when I say so | Mark the run done; frees the slot. Defaults to the current session. |
| `ais flow limit <key> <value> [--until eod\|HH:MM\|<date> \| --for 3h]` | me | Override `wip`, `per_hour` or `per_day` until the given time; the default is the end of the day. `ais flow limit` alone shows the effective limits and where each comes from; `--clear` drops overrides. |
| `ais flow ls` | me | Planners, queue, running, done-today, limits used. |
| `ais flow log` | me | The events table, newest first. |
| `ais flow install` | me, once | Write and enable the systemd units (§7). Like `ais install`, idempotent, `-n` previews. |

## 7. Scheduling with systemd

`ais flow install` writes these to `~/.config/systemd/user/` and enables them:

```ini
# ais-flow-plan@.service   (instance = workspace)
[Service]
Type=oneshot
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin
ExecStart=%h/.local/bin/ais flow plan %i

# ais-flow-plan@.timer — OnCalendar from workspace.<ws>.planner.schedule
[Timer]
OnCalendar=Mon..Fri *-*-* 09,14:00:00
Persistent=false

# ais-flow-dispatch.service
[Service]
Type=oneshot
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin
ExecStart=%h/.local/bin/ais flow dispatch

# ais-flow-dispatch.timer
[Timer]
OnCalendar=*:0/5
RandomizedDelaySec=30
```

The plan timer's schedule is written from the config, so changing
`planner.schedule` means re-running `ais flow install` (it says so).

What systemd gives us for free:

- **No overlap.** A timer firing while its one-shot service is still active is
  a no-op.
- **Survives reboots and logouts.** Linger is already on for `cmw` on agalloch.
- **Logs.** `journalctl --user -u 'ais-flow-*'`.
- **Visibility.** `systemctl --user list-timers 'ais-flow*'` shows the next
  runs; `systemctl --user start ais-flow-dispatch` forces a tick.

Checked on agalloch: linger is enabled, `gh` stores its token in
`~/.config/gh/hosts.yml` with `project` scope (so a unit can use it without a
keyring), and other user timers already run there.

Reaching tmux from a unit: tmux's default socket (`/tmp/tmux-1000/default`) is
the same one my interactive tmux uses, because the units don't set
`PrivateTmp` or `TMUX_TMPDIR`. If no tmux server is running, flow starts one
with `tmux new-session -d -s <ws>`.

## 8. The planner

### Starting it — `ais flow plan <ws>`

1. If a planner is `open`:
   - its process is gone → mark `aborted`, continue;
   - no input from me for `idle_timeout` → close its window, mark `expired`,
     continue;
   - otherwise → stop; there is already one waiting for me.
2. Fetch items with Status = `ready`. If there are none, stop.
3. Open a window named `plan` in the workspace's tmux session and type into it
   `ais flow start planner:<id>`, which runs the profile with the planner prompt
   in `planner.cwd` (no worktree).

The planner raises its own `ais notify` when its proposals are ready, as every
session already does when it stops for me.

Every planner sees all `ready` tickets, including the ones the last planner
proposed and I didn't take. Ready tickets are simply the pile of work. How
often a planner appears is set by `planner.schedule`. With leftover tickets
that pile never empties, so the schedule should be what I'd tolerate: twice a
weekday by default.

### Idle time

"Interaction" means **I submitted a prompt**. A new hook records it:

- Claude Code: a `UserPromptSubmit` hook → `ais _activity`, installed by
  `ais install` next to the existing `SessionStart` hook.
- pi: the `input` event in the existing ais extension.

`ais _activity` stamps `last_input_at` on the session record, and flow reads it
from `ais state --json`.

The initial prompt must not count, but `UserPromptSubmit` **does fire for it**
(verified, §17). The hook payload carries the prompt text, so ais records the
initial prompt on the session when it starts it. `_activity` ignores the first
`UserPromptSubmit` whose `prompt` equals it, and counts every later one.

### What the planner does — skill `plan-sessions`

A new shared skill under `~/.agents/skills/` (written with
`agents-maintenance`). It reuses work-tickets' "read the context up the chain".
It does not do the work.

1. `ais flow ready <ws> --json`, then read every ticket, its parents and its
   blockers.
2. Group tickets into sessions: one ticket each by default, several when they
   share code or must land together. For each one, say which repos, which
   profile/model, and which order or dependency.
3. Present a numbered table and **stop**. No queuing without my answer.
4. For each one I accept: `ais flow queue add --ws soi --title … --tickets …
   --repos backend,iac --priority N [--after P] [--profile pi] --notes -`.
   flow sets the Status and comments on the tickets; the agent doesn't touch
   Status itself.
5. Leave everything else alone. I only ever say "yes, now" to sessions. There
   is no "not now" and no defer: an unaccepted ticket stays `ready` and goes
   to the next planner.
6. `ais flow plan done`, then say what was queued.

**Abandoning tickets** is separate from scheduling. When I say "abandon
those" during a planner session, the agent closes them as *not planned* with a
one-line reason (`gh issue close --reason "not planned" --comment …`). The
project's own workflow then moves them out of `ready`. flow has no part in it.

### Closing

`plan done` marks the planner `decided` and releases the singleton
immediately. The next dispatch tick closes its window, so the last message
stays readable for a few minutes. If I keep typing after `done`, the planner
stays alive until I'm finished: the tick only closes a pane with no input in
the last 5 minutes.

## 9. The dispatcher

### `ais flow dispatch`, every 5 minutes

1. **Reconcile.**
   - `starting` proposals whose pane never came up within 2 min → back to
     `queued`, logged.
   - `running` proposals whose ais session died → stay `running`. Only
     `done` frees a slot. `ais flow ls` shows them as *dead, not done* so I
     notice.
   - Closed planners' windows → closed (§8).
2. **Capacity.** Uses the effective limits: config, overridden by any
   unexpired `ais flow limit`.
   - Global: `wip − running > 0`.
   - Workspace: `ws.wip − running(ws) > 0`.
   - `starts(last 60 min) < per_hour` and `starts(last 24 h) < per_day`,
     counted across all workspaces.
   - *Later:* a usage gate (§12).
3. **Pick.** Among `queued` proposals whose `after` is satisfied (the earlier
   proposal is `done`, or the ticket is closed), take the highest priority,
   then the oldest.
4. **Start.**
   - Open a window in the workspace's tmux session (the session is created if
     missing). The window is a login shell, so it stays after the agent exits,
     as tsm's create flow does.
   - Type `ais flow start <id>` into it.
   - Mark the proposal `starting` and log it in `starts`.
5. Repeat 2–4 until a limit is hit or the queue is empty. At 4/hour this is
   rarely more than one start per tick.

### `ais flow start <id>`, inside the new pane

1. Compute the session name from the title, as `ais name` does.
2. For each repo, create a worktree at `<worktree.base>/<name>/<repo>` on
   branch `<name>`, reusing `ensure_worktree`/`record_worktree`. This is the
   layout `ais wt add` already produces, so `ais wt ls`/`prune` keep working.
3. The session directory `<worktree.base>/<name>/` becomes the cwd. It holds
   every repo as a subdirectory, so the agent sees all of them. Claude Code
   loads each repo's `CLAUDE.md` when it reads files there.

   This directory is not a git repo, so Claude Code's **folder-trust dialog**
   would stop an unattended start (§17). It does not appear when
   `worktree.base` itself is trusted, because trust covers everything below a
   folder. `ais flow install` checks for that, and `ais flow start` refuses to
   launch without it. Neither writes Claude's state file: the fix is to run
   `claude` once in `~/wt` and accept. The alternative layout, starting in
   the first repo with `--add-dir` for the others, also needs no dialog,
   because each worktree inherits its main checkout's trust. It is the
   fallback if the session-directory layout misbehaves.
4. Build the prompt from `workspace.session.prompt`. It fills in `{tickets}`
   and `{notes}`, and appends a short block listing repo → path → branch.
5. Exec `ais run <profile> --cwd <dir> -d <title> -- <prompt argv>`. ais
   records the session as always and exports `AIS_FLOW_PROPOSAL`.
6. Mark the proposal `running` with the ais id. Set the tickets to
   `In Progress`. Notify quietly (recorded, no banner): "soi: started
   <title>".

Only `ais flow start` writes worktrees. The dispatcher (a systemd unit) never
touches git or harness flags.

### Done

- `ais flow done` marks the run `done`, logs it, and frees the slot.
- It does not close tickets. work-tickets already closes them with evidence or
  leaves a hand-off.
- It does not kill the pane or remove worktrees. `ais wt prune` / `ais prune`
  stay my call.

## 10. Ticket status contract

| Transition | Done by | When |
|---|---|---|
| Todo → `ready` | me | I'm happy for it to be planned |
| `ready` → `scheduled` | `ais flow queue add` | I accepted a proposal containing it |
| `scheduled` → `ready` | `ais flow queue cancel` | I changed my mind |
| `scheduled` → `In Progress` | `ais flow start` | its session started |
| `In Progress` → Done | GitHub (issue closed by a merged PR) / work-tickets | work finished |

Writes use `gh project item-edit --project-id … --id … --field-id …
--single-select-option-id …`. The ids are resolved once with
`gh project field-list` and cached in `gh_ids`. Reads use `gh project
item-list --format json`. There is no webhook (Projects v2 item webhooks are
org-only, and polling is enough at this pace).

If someone moves a ticket out of `scheduled` by hand, the next dispatch tick
notices. It leaves the proposal alone and logs a warning, since it can't tell
whether the move was a mistake.

## 11. Things this deliberately doesn't do

- Change how autonomous a running session is: its profile flags decide.
- Close tickets or merge PRs.
- Run a long-lived process, web UI or webhook endpoint.
- Coordinate PRs across repos as one change. Each repo gets its own branch and
  PR, and the session links them in the ticket.
- Dispatch to remote hosts. The layout leaves room for it: `ais flow start`
  could just as well be typed into a karif pane.

## 12. Later

- **Usage gate.** Before a start, read Claude's 5 h / 7 d utilisation. Above a
  threshold, hold the queue until `resets_at`. There are two ways to read it:
  - the OAuth usage endpoint, which is undocumented and so could change;
  - the statusline's `rate_limits`, written to a file by a statusline script.

  Either gives `resets_at`. The gate would be a step in dispatch, behind
  config.
- **tsm view.** Queue and planners in the popup: reorder, cancel, dispatch
  now.
- **More sources.** Linear, or a local TOML/markdown inbox for "personal".
- **Other workflows.** With proposals and runs as data, a proposal could chain
  into a follow-up, e.g. "review session after this one is done". The `after`
  field is where that starts.

## 13. Implementation language and layout

**Decision: Go, one module, two binaries.** ais is ported from Python before
flow is written.

Why Go:

- **One language for the whole system.** tsm is already Go (Bubble Tea,
  BurntSushi/toml). Today it decodes ais's JSON API with its own types. A
  shared `internal/` package makes the types and the contract one piece of
  code instead of two that must agree.
- **Deployment.** Static binaries, cross-compiled. tsm already ships to
  agalloch, karif and the Mac that way. ais currently needs a Python ≥ 3.11 on
  every host and a symlink into a checkout.
- **What flow adds favours static types.** SQLite state with transitions, time
  windows, GitHub GraphQL ids, a config that grows workspaces. These are the
  parts where a typo in a dict key is a 3 a.m. bug in Python and a compile
  error in Go.
- **Hooks are latency-sensitive.** `ais _session-seen`, `ais _activity` and
  `ais notify` run inside every harness event. A Go binary starts in a few ms.
- **SQLite without cgo.** `modernc.org/sqlite` is pure Go, so
  cross-compilation keeps working.

Why port first rather than writing flow in Go beside Python ais: flow reaches
deep into ais — `run`, worktree creation, the session record, the harness
hooks. Writing flow against Python ais would mean either duplicating the
worktree and launch logic in two languages or building flow in Python and
porting it later. Porting first means each piece is written once.

Why not TypeScript: no gain over Go for a CLI that shells out to tmux, git and
gh. It would add a runtime (Node or Bun) on every host and a third language
next to Go and the remaining shell glue.

Cost, said plainly: the port is the largest single step, about 3.5k lines of
Python. Its 154 unit tests exercise Python internals, so they can't verify the
port. That makes step 1 below — a black-box suite written against today's
Python ais — what makes the port safe.

Layout after the port:

```
go.mod                      module github.com/i7c/ais
cmd/ais/                    the ais binary (sessions, notify, wt, flow …)
cmd/tsm/                    the tsm binary (moved from tsm/)
internal/api/               the versioned JSON contract (state, env)
internal/session/           sessions.json, locking, reaping
internal/profile/           config.toml, profiles, harness install
internal/worktree/
internal/tmux/              shared by ais and tsm
internal/flow/              workspaces, db, source, planner, dispatcher
internal/flow/github/       Projects v2 via gh, behind a Source interface
acceptance/                 black-box CLI tests on a `tmux -L` lab server
```

`sessions.json` (version 1), `config.toml`, the `ais state --json` API and the
pane marks stay exactly as they are. Running sessions survive the switch, and
the Python and Go binaries can be swapped back and forth until the port has
proven itself.

## 14. Build plan

Each step is one or two commits and usable on its own.

0. **Spike.** Done 2026-11-20, results in §17.
1. **Acceptance suite.** Done 2026-11-20: `acceptance/`, see its README.
   - Go tests that drive the `ais` CLI as a black box: run, ls/state/env,
     notify and its sink, wt add/ls/path/prune, restart, fork, name, install
     (dry run), and the hook entry points.
   - Each test gets its own `AIS_HOME`, worktree base and `tmux -L` server
     (never `TMUX_TMPDIR`), with a fake harness script standing in for claude
     and pi.
   - Green against today's Python ais.
2. **Module restructure.** Done 2026-11-20.
   - tsm moved to `cmd/tsm` in the root module. Build it with
     `go build ./cmd/tsm`.
   - The JSON contract types moved to `internal/api`, which tsm uses through
     type aliases.
   - `internal/tmux` is deferred to step 3. tsm's tmux wrapper is tied to its
     snapshot types, so it gets a shared core only once ais needs one.
3. **Port ais to Go.** Ported 2026-11-20 (`cmd/ais`, `internal/ais`).
   - Green against the acceptance suite, and identical to the Python on a
     copy of the real `~/.ais`.
   - Installed as `~/.local/bin/ais-go` beside the Python one.
   - Left: a few days of real use, then switch `~/.local/bin/ais` on
     agalloch and karif, and delete the Python.
   - `internal/tmux` stays deferred: ais's tmux helpers are small, and
     tsm's are tied to its snapshot types.
4. **ais groundwork for flow.** Done 2026-11-20.
   - **First prompts:** `ais run --prompt` / `--prompt-file` use a
     profile `prompt` template. claude and pi have `-- {prompt}` built in,
     and `ais state` reports which profiles take a prompt.
   - **Your last input:** `ais _activity` stamps `last_input_at`, skipping
     the first prompt ais gave. It is wired to claude's `UserPromptSubmit`
     and pi's `input` by `ais install`. Verified with real claude.
   - **Multi-repo starts:** `ais run --repos a,b` makes one worktree per
     repo under the session directory.
5. **flow core.** Done 2026-11-20 (`internal/flow`).
   - **State:** `flow.db` uses SQLite (`modernc.org/sqlite`, pure Go), with
     one open planner per workspace enforced by a unique index.
   - **Config:** `[flow]` and `[workspace.<name>]`.
   - **GitHub:** the Projects v2 source goes through `gh`, with the board's
     ids cached.
   - **Commands:** `ais flow ready`, `queue add/ls/show/edit/cancel`,
     `limit`, `ls` and `log`.
   - **Tests:** black-box against a fake `gh` board, and read-only against
     gpwsoi/3.
6. **Dispatcher.** Done 2026-11-20.
   - **Starting sessions:** `ais flow dispatch` runs from a systemd timer
     every five minutes, and `--now ID` starts one by hand. It opens a
     window in the workspace's tmux session and types
     `ais flow start <id>`. That runs `ais run --prompt … --repos …`
     in-process, with `AIS_FLOW_PROPOSAL`, so the session records its
     proposal. A restart keeps the proposal; a fork doesn't.
   - **Done:** `ais flow done`.
   - **Reconcile:** a proposal still "starting" after 2 minutes goes back
     to the queue.
   - **Trust:** claude isn't started under a worktree base it doesn't
     trust.
   - **Install:** `ais flow install` writes and enables the timer.
7. **Planner.** Done 2026-11-20.
   - **`ais flow plan <ws>`:** runs from a timer per workspace. It starts a
     planner when tickets are ready and none is open. It closes one that has
     gone `idle_timeout` without your input, or whose session ended.
   - **Deciding:** `ais flow plan done` ends a planner. A decided planner's
     window closes after `close_after` (5 min) without input.
   - **Workspace info:** `ais flow workspace` tells the planner which repos
     it may propose.
   - **The skill:** `plan-sessions` lives in `~/.agents/skills/`.
8. **Live on soi.**
   - Mark two or three real tickets `ready` and run a week with `wip = 2`.
   - Then raise the limits.
9. **Later items** (§12) as they become worth it.

## 15. Settled in review

- Rate limits are global. `ais flow limit` overrides them from the command
  line until a given time, by default the end of the day.
- No quiet hours.
- Every planner considers all `ready` tickets. Unaccepted ones stay `ready` for
  the next planner. There is no "not now" or defer.
- "Abandon these tickets" is separate from scheduling. The planner closes them
  as not planned when asked.
- `ais flow done` is run by me, or by the agent when I tell it to. It frees the
  slot, and nothing else does.
- Language: Go, porting ais first (§13).

## 16. Settled after review

- Abandoning closes the issue as *not planned*, with a one-line reason.
- The planner schedule is weekdays at 09:00 and 14:00.

## 17. Spike results (2026-11-20)

These were run on a lab tmux server (`tmux -L flowspike`) with Claude Code
2.1.327. A throwaway skill wrote a marker file, and `--settings` added a hook
that logged every event.

| Question | Result |
|---|---|
| Does `claude "/spike-mark alpha-42"` invoke the skill and stay interactive? | **Yes.** The skill ran with `$ARGUMENTS = alpha-42`, then the session waited at the prompt. |
| Does `UserPromptSubmit` fire for the positional first prompt? | **Yes**, with `prompt` = the exact text. A prompt typed later fires it again with its own text. So idle tracking must skip the first one (§8). |
| `Stop` | Fired within ~1 s of each reply finishing. This is the fast "waiting for me" signal. |
| `Notification` | `idle_prompt` ("Claude is waiting for your input") fired 60 s after `Stop`. |
| `SessionStart` | `source: startup` before the first prompt. |
| Trust dialog in a new non-repo folder | **Blocks the start**, even with `--dangerously-skip-permissions`. The first prompt is not processed until it is answered. |
| Trust in a subfolder of a trusted folder | No dialog. |
| Trust in a git worktree of a trusted repo | No dialog. This is why `ais -w` sessions under `~/wt` never asked. |
| Trust in a new session folder under a trusted base | No dialog, once the base was accepted. |
| `--add-dir` of an untrusted folder | Dialog. `--add-dir` of a worktree of a trusted repo: no dialog. |
| pi | Not run, to avoid billing its provider. Its help shows `pi [options] [--] [@files...] [messages...]`, an interactive initial prompt, and `--skill <path>`. Whether a `/skill:` first message works is unverified. |

Follow-ups this creates:

- On agalloch `~/wt` has **no** trust entry today. Before the first dispatch,
  run `claude` once in `~/wt` and accept. `ais flow install` checks for it.
- The `Stop` and `idle_prompt` hooks are enough to give every session a
  working/waiting state, the `@ais_state` the tsm plan left for later. flow
  doesn't need it yet, but the same hooks would make it cheap.
