# ais and tsm

This repository contains two command-line programs for working with coding
agents (Claude Code, pi) in tmux.

- **ais** starts a program from a named profile inside a tmux pane and records
  the run in `~/.ais/sessions.json`. It is mostly used to start coding
  agents. Other commands find a recorded session again, restart or fork it,
  give it git worktrees, and record the notifications it sends.
  `ais flow` adds planned work on top: it reads tickets from a GitHub
  project, runs a planner session that proposes agent sessions, and starts
  the accepted ones within configured limits.
- **tsm** is a tmux manager that opens as a popup. It lists every tmux
  session, window and pane, and adds what ais knows about the agents running
  in them. From the popup you can switch, create, move, kill and restart
  panes and sessions, including on remote hosts over ssh. It also relays
  notifications from remote hosts to the machine you are working on. See
  [cmd/tsm/README.md](cmd/tsm/README.md).

tsm reads ais only through `ais state` and `ais env`, and works without ais
as a plain tmux manager.

## Build and install

Both are Go programs. From the repository root:

```
go build -o ~/.local/bin/ais ./cmd/ais
go build -o ~/.local/bin/tsm ./cmd/tsm
GOOS=darwin GOARCH=arm64 go build -o dist/ais-darwin-arm64 ./cmd/ais   # cross-compile
```

Then wire up the harnesses you use:

```
ais install                # Claude Code
ais install --harness pi   # pi
```

`ais install` writes three things:

| | Claude Code | pi |
|---|---|---|
| how to resume a conversation | `[profile.claude.session]` in `~/.ais/config.toml` | `[profile.pi.session]` |
| hooks reporting the conversation id and your prompts | `SessionStart` and `UserPromptSubmit` in `~/.claude/settings.json` | `session_start` and `input` in `~/.pi/agent/extensions/ais.ts` |
| instructions for the agent on using ais | a section in `~/.claude/CLAUDE.md` | a section in `~/.pi/agent/AGENTS.md` |

The instructions go between `<!-- ais:begin -->` and `<!-- ais:end -->`; the
rest of the file is not touched. Changed files are backed up as
`<name>.ais.bak`. `-n` previews; `--force` replaces wiring ais did not write.
`ais instructions [claude|pi]` prints the instructions text, for setups that
assemble those files themselves.

## ais

### Starting a session

```
ais run claude -d "fix the auth bug"
ais run claude -w -d "fix the auth bug"                  # in a git worktree
ais run claude -d "auth" --repos ~/git/backend,~/git/iac # one worktree per repo
ais run claude --prompt "/work-tickets <issue url>"      # with a first message
ais run pi -d "port the parser" -- --thinking high       # args after -- go to the harness
ais -d "port the parser" -- pi --thinking high           # short form of the line above
```

ais must run inside a tmux pane. It records the session, runs the harness in
the foreground, and records the exit code when it ends.

| option | effect |
|---|---|
| `-d, --description TEXT` | what the session is for; also gives the session its name |
| `-w, --worktree` | start in a git worktree of the repo in the current directory |
| `--no-worktree` | no worktree, even with `worktree.auto = true` |
| `--from auto\|origin\|local\|head`, `--[no-]detach` | how the worktree's branch starts (see Worktrees) |
| `--repos A,B,...` | a worktree of each repo, side by side; the session starts in the directory that holds them |
| `--prompt TEXT`, `--prompt-file PATH` | first message for the harness (`-` reads stdin); the session stays interactive |
| `--cwd DIR` | start in DIR |
| `--fork ID` | a second session beside ID: same profile, directory and worktree, conversation forked |
| `-n, --dry-run` | print the command and environment, run nothing |

The harness gets `AIS_SESSION` (the 10-character session id), `AIS_PROFILE`,
`AIS_SESSION_DESCRIPTION` and, with a worktree, `AIS_WORKTREE`,
`AIS_WORKTREE_BASE` and `AIS_REPO_MAIN`.

### Profiles

A profile is a named way to start a program. It lives in
`~/.ais/config.toml`:

```toml
[defaults]
flags = []                         # added to every profile

[profile.claude]
flags = ["--dangerously-skip-permissions"]
# command = ["claude"]             # default: the profile name
# prompt = ["--", "{prompt}"]      # how --prompt is passed; built in for claude and pi

[profile.claude.session]           # how the harness names conversations
id = "uuid4"
new = ["--session-id", "{id}"]
resume = ["--resume", "{id}"]
fork = ["--resume", "{id}", "--fork-session"]

[profile.api]                      # profiles are not limited to agents
command = ["just", "dev"]
env = { PORT = "8001" }
```

The final command is the profile's command, then `[defaults]` and profile
flags, then the session flags, then what you typed after `--`, then the
prompt. With a `session` table, ais chooses the conversation id up front, so
`restart` can resume and `--fork` can fork it. `[harness.<name>]` is the older
name for `[profile.<name>]` and is still read.

### Commands

| command | what it does |
|---|---|
| `ls [-a]` | active sessions (`-a`: ended ones too), with the age of their last notification |
| `status [-a]` | `ls` plus the recent notifications |
| `select [ID]` | switch the tmux client to a session's pane; without ID, pick from a list (fzf if installed) |
| `current` | the session this shell runs in |
| `notify TEXT` | send a desktop notification and record it on the session |
| `name [--set N] [--of TEXT]` | the session's name, used for its worktree directory and branch |
| `restart [ID]` | run an ended session again in this pane, resuming its conversation; `--fresh` starts a new one |
| `wt add\|ls\|path\|prune` | the current session's worktrees |
| `prune [--all] [--days N] [--keep-worktrees]` | remove ended sessions (default: older than 7 days) and their clean worktrees |
| `state [--active] [--since ID]` | all profiles and sessions as JSON |
| `env ID [--json]` | the directory and environment of a session, for a shell beside it |
| `install`, `instructions` | see Build and install |
| `flow ...` | planned and dispatched sessions, see below |

`ais --help` has the full reference.

### Worktrees

A session's worktrees live in `<base>/<session name>/<repo>`, all on a branch
named after the session. The default base is `~/wt`.

```
ais run claude -w -d "fix the auth bug"   # ~/wt/fix-auth-bug/<repo>
cd "$(ais wt add ~/git/other-repo)"       # add another repo to the running session
ais wt ls                                 # branch and state of each
```

```toml
[worktree]
base = "~/wt"                                  # $AIS_WT_BASE overrides it for one run
copy = [".env", ".claude/settings.local.json"] # untracked files copied into new worktrees
fetch = true                                   # git fetch before branching
auto = false                                   # true: -w by default inside a repo
from = "auto"                                  # where a new branch starts
detach = false                                 # true: a detached worktree, no branch
```

| `from` | a new branch starts at |
|---|---|
| `origin` | the remote's default branch, after a fetch |
| `local` | the default branch as the main checkout has it |
| `head` | the commit the main checkout has checked out |
| `auto` | `origin`, or `local` when it has commits not yet pushed |

An existing branch with the session's name is reused. `ais prune` removes the
worktrees of the sessions it removes, except ones with uncommitted changes or
unpushed commits, and ones another recorded session is in.

### Notifications

`ais notify TEXT` sends a desktop notification (osascript on macOS,
notify-send on Linux) and records it on the session. If the session's pane is
on screen, ais records it without a banner; `-f` sends one anyway.

A notification counts as seen once its pane has been on screen for
`seen_after` seconds. ais installs tmux hooks (slot 78 of the pane, window,
session, attach and focus change hooks) that run `ais _mark-seen` to do this.
`ls`, `select` and tsm mark unseen recent notifications with `●`.

With `[notify] sink` set, each notification is first passed to that command
as JSON on stdin. The command answers `{"result": "shown"}` or
`{"result": "suppressed"}`, or exits non-zero to let ais deliver it.
`sink = ["~/.local/bin/tsm", "notify", "--stdin"]` sends notifications from a
server to the laptop connected to it through `tsm relay`.

```toml
[notify]
sound = "Glass"          # macOS sound; "" for none
window = 60              # minutes a notification counts as recent
when_visible = "skip"    # or "send"
seen_after = 3           # seconds
# sink = ["~/.local/bin/tsm", "notify", "--stdin"]
```

### tmux integration

While a session runs, ais names its window after the session
(`[tmux] rename = false` turns this off) and sets the pane options `@ais_id`,
`@ais_profile`, `@ais_desc` and `@cwd`. ais itself changes to the session's
directory, so `#{pane_current_path}` and splits made from it point there.
Sessions are tracked by pane id, so panes can be moved between windows and
sessions.

### State and API

`~/.ais/sessions.json` holds every recorded session. A session is marked
ended when its process exits, or when a later read finds its process or pane
gone. `ais state` and `ais env --json` are the interface for other programs.
Their output carries an `"api"` version, and readers ignore keys they do not
know. Each session lists what can be done with it (`can_restart`,
`can_resume`, `can_fork`).

## ais flow

`ais flow` takes tickets from a GitHub project to running agent sessions:

1. **Planner.** A timer checks each workspace hourly on weekdays. When there
   are tickets to plan and room for more sessions, it starts a planner
   session: an agent with the `plan-sessions` skill, which reads the ready
   tickets, proposes sessions and waits for you.
2. **Queue.** You accept some of the proposals. The planner queues them with
   `ais flow queue add`, which moves their tickets to `scheduled`.
3. **Dispatcher.** A second timer, every five minutes, starts queued sessions
   while the limits allow. Each runs in a new window of the workspace's tmux
   session, with its first prompt and a worktree of each repo it needs. Its
   tickets move to `In Progress`.
4. **Done.** `ais flow done`, run by you or by the agent when you tell it to,
   frees the session's slot.

Nothing runs between timer ticks. State is kept in `~/.ais/flow.db`
(SQLite). Every session is a normal ais session.

### Configuration

```toml
[flow]                   # limits across all workspaces
wip = 4                  # sessions started by flow and not yet done
per_hour = 4             # starts in any 60 minutes
per_day = 16             # starts in any 24 hours

[workspace.soi]
tmux_session = "soi"
profile = "claude"       # for proposals that do not name one
# wip = 3                # a lower limit for this workspace

[workspace.soi.source]   # a GitHub Projects (v2) board, read through gh
owner = "gpwsoi"
project = 3
field = "Status"
ready = "ready"
scheduled = "scheduled"
in_progress = "In Progress"
comment = true           # comment on a ticket when it is scheduled

[workspace.soi.repos]    # the repos sessions may use, by short name
backend = "~/git/gpwsoi/backend"
iac = "~/git/gpwsoi/infra-workloads"

[workspace.soi.planner]
schedule = "Mon..Fri *-*-* 08..20:00:00"   # systemd OnCalendar
idle_timeout = "48h"     # close a planner after this long without your input
min_interval = "2h"      # least time between two timer-started planners
close_after = "5m"       # close a decided planner's window after this long

[workspace.soi.session]
prompt = "/work-tickets {tickets}\n\n{notes}"   # first prompt of a dispatched session
```

`ais flow install` writes and enables the systemd user units:
`ais-flow-dispatch.timer` and one `ais-flow-plan-<workspace>.timer` per
workspace. Run it again after changing a schedule. Logs:
`journalctl --user -u 'ais-flow*'`.

### What gets planned

A ticket is plannable when it is open, not already in an active proposal, not
`scheduled` or `In Progress` on the board, not blocked by an open issue, and
has no open sub-issues.

A ready ticket with open sub-issues is an epic. Its plannable sub-issues are
proposed instead of the epic. The epic stays `ready` until its last sub-issue
is closed, and only then can it be queued itself. `ais flow ready` shows the
tree and what in it is plannable.

The timer starts a planner only when all of these hold:
- something is plannable;
- running plus queued sessions are below `wip`;
- no planner is open;
- `min_interval` has passed since the last planner started.

`ais flow done` starts one right away if a slot became free, without waiting
for `min_interval`. `ais flow plan <ws>` run by hand only needs something
plannable.

### Commands

| command | what it does |
|---|---|
| `workspace [WS] [--json]` | a workspace's repos, profile and statuses |
| `ready [--ws WS] [--json]` | ready tickets, with sub-issues, holders and blockers |
| `plan [WS] [--auto] [--dry-run]` | start a planner if the checks pass; close one that has been idle or whose session ended |
| `plan done [ID\|WS]` | end a planner after you decided |
| `queue add --ws WS --title T --tickets T1,T2 --repos R1,R2 [...]` | queue a session; its tickets move to `scheduled` |
| `queue ls\|show\|edit\|cancel` | inspect and change the queue; `cancel` moves tickets back to `ready` |
| `dispatch [--dry-run]` | start queued sessions within the limits (the timer runs this) |
| `dispatch --now ID` | start one now, past the limits and its dependencies |
| `done [ID]` | mark a session's work finished and free its slot |
| `limit [KEY VALUE] [--until eod\|HH:MM\|DATE \| --for 3h]` | show the limits, or override one for a while |
| `ls` | per workspace: open planner, queued, running; limits used |
| `log [-n N]` | what flow did, newest first |
| `install [-n] [--no-enable]` | write and enable the systemd timers |

Tickets are written as issue URLs or `owner/repo#n`. `queue add` takes
`--after ID|TICKET` (start only after that proposal is done or that ticket is
closed), `--priority N`, `--notes` and `--prompt`.

The design and the decisions behind it are in
[docs/flow-proposal.md](docs/flow-proposal.md).

## Files

| file | contents |
|---|---|
| `~/.ais/config.toml` | profiles, `[worktree]`, `[tmux]`, `[notify]`, `[flow]`, workspaces |
| `~/.ais/sessions.json` | recorded sessions |
| `~/.ais/flow.db` | flow's planners, queue, start log and events |
| `~/.config/systemd/user/ais-flow-*` | flow's timers, written by `ais flow install` |

## Development

```
go test ./...
```

This runs the unit tests and the acceptance suite in `acceptance/`. The suite
runs the `ais` binary against an isolated HOME, `AIS_HOME` and tmux server
(`tmux -S`), with stand-ins for the agent harness, `gh` and the notification
backends. `AIS_BIN=/path/to/ais go test ./acceptance/` tests another build.
See [acceptance/README.md](acceptance/README.md).

ais was a Python script until November 2026 (`git log -- ais`). The Go
version reads and writes the same files.
