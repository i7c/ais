# tsm — tmux session manager

One popup over everything in tmux: every session, window and pane, what is
running where, and the things you do to them a key away.

```
bind-key a display-popup -E -w 90% -h 80% -T " tsm " "tsm"
```

tsm holds no state of its own. tmux is the record of what exists — read in
one `list-panes` call each time you open it, and once a second while it is
open — and [ais](../README.md), when it is installed, adds what it knows about
the processes it started: what an agent is for, whether it is calling for
you, and which ended sessions can be picked up again.

## Build

```
cd tsm && go build -o ~/.local/bin/tsm .
```

It needs the `ais` from this repository (`ais state` is its API); point
`$TSM_AIS` at one when the `ais` on `PATH` is older. Without ais at all, tsm
is still a full tmux manager, just without the agent layer.

## The list

```
 tsm                                            14 panes · 6 sessions · ● 1 need you
 ── NEEDS YOU ─────────────────────── │ deploy the admin panel from the pipeline
●  soi:3 deploy the admin panel claude │ runs     claude via ais 7hwgx2dnc4  pid 3858939
 ▾ soi           4 windows · attached │ dir      ~/git/gpwsoi/infra-workloads/dev
   1 Migration plan             claude │ git      ⎇ main  ✚2
   2 setup infra for auth       claude │ at       soi:3.1  %332  230x62
   3 dev                       2 panes │ started  3h ago
     · ~/git/gpwsoi/infra/dev      zsh │ ● 1m   need approval to run tofu apply
     · ~/git/gpwsoi/infra/dev     tofu │ ─────────────────────────────────────
 ── RESUMABLE ─────────────────────── │  (the pane's screen, live)
   ○ Backend prototype      claude 4d │
```

- **needs you** — ais sessions whose last notification is recent and was not
  already seen, listed first so they are a few keys up. A window that rang
  the terminal bell while you were not looking joins them (weechat's default
  `beep` trigger rings it for highlights and private messages), until you
  visit it and tmux clears the flag. Remote ones too, including a bell in
  the remote window you are showing through `tsm attach`: the remote's tmux
  does not flag that one, but the bell reaches the connection's pane here,
  and tsm marks the remote pane it came from.

The popup opens on the pane you came from (`◆`). When that pane is your way
to a remote, it opens on the pane you are looking at over there instead (see
below), so you navigate from where you are rather than from the top.
- One row per window when the window is one pane; a window of several lists
  them under it.
- **resumable** — ais sessions that ended and can be restarted here, the
  latest run of each piece of work only.

## Keys

| key | does |
|---|---|
| `⏎` | go there; on an ended session, restart it in a new window |
| `z` | go there, zoomed |
| `v` / `V` | shell beside it, split right / below |
| `t` | shell beside it, in a new window |
| `d` | duplicate: `ais run --fork` an agent whose profile can fork, a fresh one of the same profile (same args, no new worktree) where it cannot, a shell otherwise |
| `x` | kill the pane, window or session — asks first unless it is an idle shell |
| `r` | rename the session or window |
| `m` | move: pick up the selection, then rearrange and confirm (below) |
| `n` | new: a profile or a shell, somewhere, in a new worktree or not, as a window, split or session (below) |
| `/` | filter on session, window, command, directory and description; every word must match |
| `j k` `tab` `h l` `g G` | move, next session, collapse/expand, top/bottom |
| `?` | help |

## Making something new

`n` opens a form next to the list:

```
 NEW ────────────────────────────────────────────────
 what       claude  pi  shell
 where     / wid
           ▸ ~/git/widget                    ⎇ repo
             ~/wt/fix-lexer/widget           ⎇ worktree
 worktree   new  here
 place      window in soi  split right  split below  new session
 about     port the parser

 runs      ais run claude -w -d 'port the parser'
 in        ~/git/widget
```

- **where** is a list to filter (or a path to type): the selection's
  directory, yours, every pane's, where ais sessions ran, the worktrees under
  ais's `worktree.base`, and the repos under `$TSM_ROOTS` (colon-separated,
  default `~/git`, two levels deep).
- **worktree** is asked only for a profile in a repo. `new` passes `-w`
  (ais branches the repo and starts there, named after the session), `here`
  passes `--no-worktree`, so the answer holds whatever `worktree.auto` says.
  "Another agent on that repo" is a repo plus `new`; "another agent in that
  worktree" is the worktree plus `here` (or `d` on the agent).
- **place** puts it after the selection's window, beside the selection, or in
  a session of its own, named like the work (`ais name --of`).
- `tab` moves between fields, `←→` changes a choice, `↑↓` picks a directory,
  `⏎` starts from any field. What, place and worktree default to what you
  chose last (kept in `~/.cache/tsm/`), and the form opens on where — so
  `n`, a few letters of a repo, `⏎` is the common case.

## Moving things around

`m` picks up the selected pane (or a whole window, on a window row of several
panes) and turns the list into a plan:

```
 tsm  MOVE  3 staged
 ▾ main                        3 windows │ MOVE ──────────────────────────────
   1 ui                              zsh │ 3 staged
+  2 lab-agent ← work:0          2 panes │  1  lab/b → beside lab agent in work:0
     · lab agent                   sleep │  2  window work:0 lab-agent → main
+    · lab/b ← main:0.1              zsh │  3  window main:0 zsh → scratch (new)
+▾ scratch (new)               1 window │
▌  2 lab/a ← main:0                  zsh │ tmux
                                         │   join-pane -d -h -s %1 -t %2
                                         │   move-window -d -s %2 -t $0:
                                         │   new-session -d -s scratch
                                         │   move-window -d -s %0 -t scratch:
                                         │   kill-pane -t <placeholder in scratch>
```

| key | while moving |
|---|---|
| `m` | pick up the selection (again, to pick up something else) |
| `p` | drop it beside the selected pane, in that window |
| `P` | drop it into the selected session as a window of its own |
| `N` | drop it into a new session (asks for the name) |
| `u` | put down what you hold, else undo the last drop |
| `⏎` | apply the whole plan |
| `esc` | put down, else discard the plan (asks) |

Nothing happens to tmux until `⏎`. The list shows the server as the plan
will leave it — moved things marked `+` with where they came from, sessions
the plan empties gone, new ones marked `(new)` — and the panel shows the plan
in words and the tmux commands it comes to. Moves name panes by id, which
tmux keeps through every move, so the plan still means the same thing when
it runs; if something it names closes in the meantime, the plan is cut there
and you are told. Every command runs with `-d`, so nobody's view jumps.

A new session is created with a placeholder pane (tmux has no empty
sessions), which goes once the plan has run.

## Remote hosts

With tmux on your laptop, and in one of its panes an ssh or mosh connection to
a server running tmux too, tsm shows both: the server's sessions under a
heading of their own, its agents among "needs you", and every key working on
its rows as on local ones.

```toml
# ~/.config/tsm/config.toml
[[remote]]
name = "karif"       # also the ssh destination, unless ssh = "..." says otherwise
attach = "mosh"      # what "tsm attach karif" connects with: ssh (default) or mosh
session = "main"     # the tmux session it attaches to there
```

Install tsm (and ais) on the server too, then connect with

```
tsm attach karif
```

instead of `mosh karif -- tmux attach`. It marks the pane as the way to karif
and tags the tmux client it starts there, so a jump to a karif pane switches
this pane's view there and moves your client here to this pane. A connection
made by hand is found by its `ssh karif` / `mosh` command line; on the far end
tsm then moves the client used last.

The same tag tells tsm what you are looking at on karif: open the popup from
the pane connected to it and the cursor starts on the karif pane that client
shows. That comes from the cache (kept current by `tsm relay` when it runs),
and if the answer only arrives after the popup opens, the cursor moves there
unless you have already pressed a key.

How it stays fast at ~300ms round trips:

- tsm talks to the server over an ssh connection of its own, never your
  interactive one (mosh has no side channel). It is a ControlMaster that
  persists for 8 hours, so after the first request each costs one round trip.
- Every answer is cached in `~/.cache/tsm/`. The popup opens on the cache
  — the heading says `as of 3m ago` — and replaces it when the server answers
  (`● live`), then every few seconds while open. A server that does not
  answer is `offline`, still shown as last seen.
- One request fetches everything: tmux's picture, the server's ais state and
  the directories its `n` form offers. The selected pane's screen is fetched
  only for the selection.
- Actions are sent as a description (`tsm _op` reads JSON) and done there by
  the same code as here; staged moves go as one batch. Moves stay within a
  host: a process cannot change machines.

Both ends must run a tsm speaking the same protocol; a mismatch says so in
the heading. Measured against a real server: the first fetch (with the ssh
handshake) ~570ms, every later one ~190ms, a jump to a remote pane ~240ms
until both ends have switched — while the popup's first frame stays at ~50ms,
the remote part drawn from the cache.

tmux older than 3.8 replaces control and non-ASCII characters in its
output with `_` unless it runs in a UTF-8 locale, which commands over ssh
usually lack; tsm gives its tmux calls `LC_CTYPE=C.UTF-8` when needed.

## Notifications from anywhere: `tsm relay`

`ais notify` on a server you reach through tmux reaches the screen you are
looking at, however many hops away: each host runs a `tsm relay`, connected
to the hosts below it, and a notification climbs the chain of connections
until it reaches one with a screen.

```
karif:    ais notify → tsm notify ─┐
                                   └ relay --serve ══ssh══ agalloch: tsm relay ─┐
agalloch: ais notify → tsm notify ──────────────────────────────────────────────┴ relay --serve ══ssh══ Mac: tsm relay → banner
```

- **On every host with agents:** tell ais to offer its notifications to tsm
  first, in `~/.ais/config.toml`:
  ```toml
  [notify]
  sink = ["~/.local/bin/tsm", "notify", "--stdin"]
  ```
  With nobody connected from above, `tsm notify` declines and ais does what
  it always did, recording the notification as undelivered.
- **On every host that reaches others** (`[[remote]]` in its tsm config), run
  the relay. From tmux.conf it lives and dies with the server:
  ```
  run-shell -b 'tsm relay'
  ```
  On a Mac without tmux, run it as a LaunchAgent
  (`~/Library/LaunchAgents/tsm.relay.plist`, then `launchctl load` it):
  ```xml
  <plist version="1.0"><dict>
    <key>Label</key><string>tsm.relay</string>
    <key>ProgramArguments</key><array><string>/usr/local/bin/tsm</string><string>relay</string></array>
    <key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
  </dict></plist>
  ```
- `tsm relay status` says what is connected, below and above.

What happens to a notification:

- Each relay offers it to the relay above, if one is connected to its host;
  the top one shows it (`osascript` on a Mac, `notify-send` on a Linux
  desktop, or `$TSM_NOTIFY_CMD title message` for anything else).
- It is **suppressed** only if you are looking at it all the way up: its
  pane is on screen where it was raised, every host in between is showing
  the pane that holds the connection, and on a Mac the terminal is in front.
  The record on the server says so, which keeps "needs you" honest.
- If nobody is connected above a host, the notification is held there (50, a
  day at most) and replayed, with the server's own undelivered ones, when a
  relay connects — folded into one banner if there are several.
- The relay also pushes each remote's tmux picture up whenever it changes (at
  least every 5s), into the cache the popup opens on: with a relay running,
  a remote is `● live` the moment the popup appears. "Whenever it changes" is
  tmux's word for it, not a poll: the serving end sets hooks (slot `[77]` of
  `client-session-changed`, `window-pane-changed` and the like, each running
  `wait-for -S tsm-changed`) and waits on that channel, so a pane switch over
  there is in the cache here one trip later. The hooks stay set after the
  relay goes, signalling nobody; a `set-hook -g` of your own replaces them
  until the next heartbeat sets them again. Commands and directories have no
  hook and go up with the heartbeat.

Relay connections use ssh (never mosh, which has no side channel), so they
work alongside an interactive mosh session and reconnect after sleep or a
dropped network. Banners on a Mac cannot be clicked through to the pane:
`osascript` notifications do not report clicks.

A new shell beside an ais pane starts in the session's directory — its
worktree — with its profile's env and `$AIS_WORKTREE`/`$AIS_WORKTREE_BASE`
(from `ais env`), but not `$AIS_SESSION`: the shell is not the agent.

Anything tsm starts in a pane (`ais restart`, `ais run --fork`) is typed into
a fresh interactive shell rather than run as the pane's command, so it runs as
a job of that shell exactly as if you had typed it, and the shell is still
there when it ends.

## Speed

The popup draws its first frame from the tmux query alone, ~13ms after the
key; `ais state` (~90ms of Python) fills in the agent details just after.
`go.mod` pins `go-runewidth` at v0.0.31 for this: older releases build width
tables for every code point at startup, which alone cost ~40ms.
