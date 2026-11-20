# Proposal: `ais flow` — planned, dispatched agent sessions

Status: **draft for review** (2026-11-20)

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
| Runtime | No daemon. **systemd user timers** run one-shot `ais flow` commands; state lives in **SQLite** (Python stdlib). |
| Where | agalloch only, local tmux. Remote dispatch later. |
| Harnesses | Claude Code required, signed-in binary (subscription). pi and others via ais profiles. |
| Task source | GitHub Projects v2 **Status** field: `ready` → `scheduled` → `In Progress`. Issue search with labels as a fallback source. |
| Planner | One per workspace, singleton, closed explicitly or after 48 h without my input. |
| WIP | Live *dispatched* sessions count; hand-started ones don't. Global and per-workspace caps. A slot is freed **only** by an explicit `done`. |
| Rate | While under WIP: ≤ 4 starts/hour and ≤ 16/day, rolling windows. Manual start command exists. Usage-limit awareness later. |
| Queue | Local SQLite is the source of truth, plus a short comment on each ticket. Editable via CLI (and later tsm). |

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
# quiet = "22:00-08:00"   # optional: no automatic starts in this window

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
schedule = "Mon..Fri *-*-* 08..19:00:00"   # systemd OnCalendar
idle_timeout = "48h"

[workspace.soi.session]            # defaults for dispatched sessions
prompt = "/work-tickets {tickets}\n\n{notes}"
```

Profiles gain one key, telling ais how to hand a harness its first prompt:

```toml
[profile.claude]
prompt = ["{prompt}"]     # claude "<prompt>" stays interactive
[profile.pi]
prompt = ["{prompt}"]     # pi "<msg>" likewise
```

A profile without `prompt` can't be dispatched with one; `ais flow` says so at
`queue add`, not at 3 a.m.

## 5. State

`~/.ais/flow.db`, SQLite in WAL mode. ais keeps owning `sessions.json`; flow
refers to sessions by ais id and never writes that file.

```
planners   (id, workspace, ais_session, state, started_at, last_input_at,
            closed_at, reason, items_seen JSON)
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
| `ais flow plan <ws>` | planner timer, or me | Expire an idle planner; if none is open and the source has new `ready` items, start one. `--force` starts one regardless. |
| `ais flow ready <ws>` | planner agent | List `ready` items not already queued. |
| `ais flow queue add …` | planner agent | Store a proposal, set its tickets to `scheduled`, comment on them. |
| `ais flow queue [ls\|edit\|mv\|cancel] …` | me | Inspect, change priority, edit prompt, cancel (tickets go back to `ready`). |
| `ais flow plan done` | planner agent | Mark the current planner `decided`; its window closes on the next tick. |
| `ais flow dispatch` | dispatch timer | Reconcile, then start queued proposals while limits allow. |
| `ais flow dispatch --now <id>` | me | Start one proposal now, past WIP and rate limits (still logged and counted). |
| `ais flow start <id>` | the new tmux pane | Make worktrees, set `In Progress`, exec the harness through ais. Internal. |
| `ais flow done [<ais id>]` | me, or the agent when I say so | Mark the run done; frees the slot. Defaults to the current session. |
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
OnCalendar=Mon..Fri *-*-* 08..19:00:00
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
2. Fetch items with Status = `ready`. Drop those already in the queue.
3. Start a planner only if some item was **not presented to the last planner**
   (`items_seen`), or `--force`. Without this rule, tickets I deliberately left
   `ready` would summon a fresh planner every hour.
4. Open a window named `plan` in the workspace's tmux session and type into it
   `ais flow start planner:<id>`, which runs the profile with the planner prompt
   in `planner.cwd` (no worktree).

The planner raises its own `ais notify` when its proposals are ready, as every
session already does when it stops for me.

### Idle time

"Interaction" means **I submitted a prompt**. A new hook records it:

- Claude Code: a `UserPromptSubmit` hook → `ais _activity`, installed by
  `ais install` next to the existing `SessionStart` hook.
- pi: the `input` event in the existing ais extension.

`ais _activity` stamps `last_input_at` on the session record, and flow reads it
from `ais state --json`. The initial prompt typed by flow doesn't count.

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
5. `ais flow plan done`, then say what was queued.

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
2. **Capacity.**
   - Global: `wip − running > 0`.
   - Workspace: `ws.wip − running(ws) > 0`.
   - `starts(last 60 min) < per_hour` and `starts(last 24 h) < per_day`.
   - Not in `quiet`, when it is set.
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

## 13. Build plan

Each step is one or two commits and usable on its own.

0. **Spike (½ day).** Settle the two open facts before writing flow code:
   - Does `claude "/work-tickets <url>"` invoke the skill?
   - Does a `UserPromptSubmit` hook fire for typed prompts but not for the
     positional initial prompt?
1. **ais groundwork.**
   - The `prompt` key on profiles; `ais run … --prompt-file`.
   - `ais _activity` with its claude and pi hooks; `last_input_at` in
     `ais state`.
   - A multi-repo `ais run --repos a,b,c` that creates all the worktrees up
     front.
2. **flow core.**
   - `flow.db`, the `[workspace]` config, the GitHub project source (read,
     write, id cache).
   - `ais flow ready`, `queue add/ls/cancel`, `ls`, `log`.
3. **Dispatcher.**
   - `dispatch`, `start`, `done`, `dispatch --now`, the limits, reconcile.
   - `ais flow install` for the dispatch timer.
   - Tested on an isolated `tmux -L` server (never `TMUX_TMPDIR`) against a
     scratch project.
4. **Planner.**
   - `ais flow plan` with singleton, idle expiry and the new-items rule.
   - The plan timer.
   - The `plan-sessions` skill.
5. **Live on soi.**
   - Mark two or three real tickets `ready` and run a week with `wip = 2`.
   - Then raise the limits.
6. **Later items** (§12) as they become worth it.

## 14. Open questions (with the default I'd take)

1. **Rate limits: global or per workspace?** Default: global, because the
   Claude subscription is shared. A workspace can set a lower cap of its own.
2. **Quiet hours.** You didn't pick a window. Default: off. The planner
   schedule (weekdays 08–19) already keeps planners out of the night, and a
   dispatched session waiting overnight costs nothing.
3. **Leftover `ready` tickets.** Should a ticket I skipped in one planner
   trigger the next? Default: no, only *new* ready items start a planner
   (§8.3). Skipped tickets still show up when something new arrives.
   `ais flow plan --force` covers the rest.
4. **"Not now" for a ticket.** Default: the planner can move it back to Todo
   when I say so. A `defer --until` is not needed yet.
5. **Who may call `ais flow done`?** Default: me, or the agent when I tell it
   to. The agent never calls it on its own judgment.
6. **Dead but not done.** Should a dispatched session that exited still hold
   its slot? Default: yes, as you said: the slot frees on explicit done only.
   `ls` flags it, and after 24 h a quiet notification reminds me.
7. **Code layout.** ais is one 3.5k-line file. Default: put flow in a sibling
   module, `ais_flow.py`, imported by `ais` (resolving through its symlink),
   so the main file stops growing.
