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
  already seen; the popup opens on the first of them, so `^b a ⏎` takes you
  to whatever is waiting. Otherwise it opens on the pane you came from (`◆`).
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
