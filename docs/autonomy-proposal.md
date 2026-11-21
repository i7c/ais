# Proposal: autonomy levels, ticket refinement and plan review

Status: **draft, round 2** (2026-11-21)

## 1. What this is for

`ais flow` starts sessions on its own now. Inside each one, `work-tickets`
still does what it did when I started it by hand: it surveys, proposes a batch
with a short preview, and waits for my go. I want every session to go **as far
as the ticket lets it** before it stops for me.

How far that is depends on the ticket, and a ticket rarely holds everything an
agent needs. So this proposal splits the work in two:

- **Refinement** makes the ticket good enough to plan from. It is about intent,
  prerequisites and requirements, not about how to build. It runs without me,
  writes what it found into the ticket, and hands the rest back to me as
  questions.
- **Work sessions** only get tickets that are good enough. They produce a plan
  as good as the ticket allows (L2 or L3), and I review it as a draft PR.

One rule holds everywhere: **nothing is changed and no execution tokens are
spent before I approve a plan.** Refinement writes only to the ticket.

## 2. Settled so far

| Question | Decision |
|---|---|
| Levels | L1 means the ticket needs refinement, and it never reaches a work session. L2 means a plan with its open questions, refined with me. L3 means a plan the session refined itself. |
| Level = ? | `min(ceiling, evidence)`. I set the ceiling and the ticket provides the evidence (§4). Default ceiling L2. |
| L1 handling | A separate, non-interactive refinement run writes evidence and questions into the ticket and moves it to `refinement`. I answer, then move it back to `ready` (§5). |
| Who starts refinement | flow's deterministic tick, not the planner (§5.1). |
| Loops | A round counter lives in the ticket, with a soft limit (default 3). Past it, the ticket goes to the planner flagged instead of back to refinement (§5.4). |
| WIP | A refinement run counts as a dispatched session. Its slot frees itself when the run exits. |
| Task layer | Agents never call `gh` for tickets. Everything goes through `ais flow ticket …`, backed by the workspace's source (§6). |
| Review surface | A draft plan PR (§8). Tooling, not instructions, removes the plan commits (§8.2). |
| Harnesses | Every profile that has a non-interactive template can run refinement. That is claude and pi built in (§7). |
| Planner-side questions, review nudge | Not in v1. |
| Risk (R7) | Caps the level at L2; it does not force L1. |

## 3. The lifecycle of a ticket

```
            I mark it ready
                  │
                  ▼
   ┌────────── ready ◄──────────────────────────────┐
   │              │ not assessed for this version   │
   │              ▼                                 │
   │      refine run (headless, holds the ticket)   │
   │        │ L2/L3                   │ L1          │
   │        ▼                         ▼             │
   │   assessed, stays ready     refinement ── I answer, move it to ready
   │        │                         (round n+1)
   │        ▼
   └──► planner proposes ─► scheduled ─► In Progress
                                          │ plan PR (L2/L3)
                                          │ I approve
                                          ▼
                                      implementation ─► Done
```

The board gets one new status, `refinement`. Nothing else changes in the
status contract, and the planner never sees a ticket that hasn't been assessed.

## 4. Choosing the level

### 4.1 The readiness check

Refinement runs this check, and so does a work session if the ticket changed
since it was last assessed. It runs on the ticket and on everything
`work-tickets` already reads up the chain: ancestors, blockers' hand-offs, the
plan, and the repo's `AGENTS.md`/`CLAUDE.md`. Each criterion is answered with
the evidence it rests on.

| # | Criterion | Met when | If not met |
|---|---|---|---|
| R1 | **Intent** | The ticket or an ancestor says why the work matters and what outcome it serves. | L1 |
| R2 | **Done when** | There are checkable acceptance criteria. | L1 |
| R3 | **Scope** | "Out of scope" is stated, or the boundary is obvious from the ancestors. | L1 if the work could reasonably go two very different sizes, else L2 |
| R4 | **Decisions** | Every fork is decided upstream or has a stated default. | L1 if it changes *what* is built; L2 if only *how* |
| R5 | **Verification** | A way to check the result exists or is named: tests, a command, a metric, a reviewer. | L2 at most |
| R6 | **Conventions** | The rules for this kind of work are written down: `AGENTS.md`, linters, a research template, a style. | L2 at most |
| R7 | **Risk** | Nothing irreversible or outward-facing is left open. | L2 at most |

The evidence level is **L1** if R1, R2 or a "what" R4 fails, **L3** if every
criterion passes, and **L2** otherwise.

The criteria are written for any kind of work. For a research or ordering
ticket, R5 is "how will we know the answer is right" and R6 is "which template
or standard applies".

The rubric lives once, in `~/.agents/shared/autonomy.md`. It is cited by
`refine-ticket`, `work-tickets` and `plan-sessions`.

### 4.2 The ceiling

First match wins:

1. **The session's prompt.** `ais flow queue add --autonomy L3` stores it on
   the proposal, and the template gets `{autonomy}`. By hand:
   `/work-tickets #42 --autonomy L3`.
2. **The ticket.** An `Autonomy` field on the board, or an `autonomy:L3` label,
   read through the source.
3. **The workspace.** `[workspace.<ws>.session] autonomy = "L2"`.
4. **Default:** L2.

The planner doesn't score tickets any more: the assessment is already stored.
`ais flow ready --json` reports each ticket's level, reasons and round, and the
planner's table shows them. When I accept a proposal I accept its level, or I
change it.

## 5. Refinement

### 5.1 What starts it

The dispatch tick (every 5 minutes) gets one new step before it picks work: a
**refinement sweep**, at most every `refine.sweep` (default 30 min) per
workspace.

1. Take every ticket in a ready ticket's `work` (`readyWork`, as the planner
   does). This covers "the ticket is ready" and "its epic is ready" alike.
2. Skip tickets that are held, or that already have an assessment for their
   current version. The **version** is a hash of the title, the body and the
   comment count, from the source.
3. Queue a **refine proposal** for each one left: `kind = refine`, one ticket,
   no human acceptance, priority above work proposals. It holds the ticket
   (`held_by`) like any proposal, so the planner skips it while it runs.

From there the normal dispatcher starts it, within WIP and the rate limits.

Why not the planner: the planner is interactive and waits for me, while
refinement must not. The planner also runs twice a day, which is too slow for
"I marked it ready, tell me what's missing". And starting runs is the
dispatcher's job; a planner that also launched agents would be a second
dispatcher.

**When a refine run makes a ticket plannable,** its exit counts as a finished
session. flow already opens a planner after a finished session
(`planAfterDone`), so an assessed L2/L3 ticket reaches me without waiting for
14:00.

### 5.2 What a refine run does — skill `refine-ticket`

It runs headless (§7), in the workspace's planner cwd, with read access to the
workspace's repos and no worktree, because it changes no code.

1. `ais flow ticket show <ref> --context --json` returns the ticket, its
   ancestors, blockers, hand-offs, the refinement block (§5.3) with my
   answers, and the repo paths.
2. **Gather evidence** for anything evidence can settle:
   - the code (`code-locator`, `Explore`);
   - the web (`web-research`, archived);
   - linked docs, ADRs and earlier tickets.

   Each finding is written as a fact plus where it came from.
3. **Fold in my answers.** Answered questions become *Decisions* in the block.
   They are never asked again (§5.4).
4. **Run the readiness check** (§4.1).
5. Write the outcome with **one** command, which also sets the status:
   - **L2/L3:** `ais flow ticket assess <ref> --level L3 --file -`. This updates
     the block (evidence, decisions, level and reasons), leaves the ticket
     `ready`, and records the assessment for this version.
   - **L1:** `ais flow ticket refine <ref> --file -`. This updates the block
     with the open questions, moves the ticket to `refinement`, and comments
     "round n: 4 questions, 2 asks to rewrite a section" so I get notified.
6. Exit. `ais flow start` sees the harness end, marks the run `done`, and frees
   the slot. No `ais flow done` is needed, and none is possible from a headless
   run anyway.

It never touches the plan or how to build, and it never edits text I wrote.

### 5.3 The refinement block in the ticket

flow owns one delimited block at the end of the ticket body. Agents don't edit
the body with their own tools: they hand flow a structured file, and flow
renders the block and swaps it in. Everything above it is mine and is never
rewritten.

```markdown
<!-- ais:refinement v1 — written by ais flow; answer inline -->
## Refinement · round 2 of 3 · L1 (2026-11-21)

**Why L1:** Done-when missing (R2); export format undecided and changes what
is built (R4).

**Evidence**
- The v1 export lives in `backend/export/csv.go:40`; it streams rows, no
  pagination. (code)
- Two customers asked for XLSX in #31 and #38. (tickets)

**Decisions** (from earlier rounds)
- D1 (round 1): exports stay synchronous below 10k rows. — you

**Questions**
1. Which formats must v2 support? *Default: CSV + XLSX.*
   > Answer:
2. Who must be able to run an export: admins, or every user?
   > Answer:

**Please rewrite**
- The "Why" section: it says what, not why. What breaks for users today?

**Rounds:** 1 · 2026-11-20 · L1 · 5 questions · 2 · 2026-11-21 · L1 · 2 questions
<!-- /ais:refinement -->
```

I answer after `> Answer:`, or I edit my own sections as asked. Then I move the
ticket to `ready`. That changes nothing in the text, but the edits I made do
change its version, so the sweep picks it up again.

If I move it back to `ready` without changing anything, the version is the
same, so the old L1 assessment stands and nothing runs. `ais flow ls` lists the
ticket as "ready, but still L1 with unanswered questions". If I want a round
anyway, `ais flow refine <ref>` forces one.

The block is plain markdown between two comment markers. Any source that has a
ticket body can hold it.

### 5.4 Keeping it from looping

Three rules keep refinement from going round forever:

1. **Every round must narrow.** A question that was answered becomes a decision
   and is never asked again. A new question must cite the answer or evidence
   that raised it. Detail-level questions (how, not what) are not L1 material:
   they go into the block as notes for the work session.
2. **The rounds are visible.** The round log stays in the block, and the
   heading says "round 2 of 3".
3. **The soft limit.** When a ticket is still L1 after `refine.max_rounds`
   (default 3), the run does not send it back to `refinement`. It records
   **L2 with open questions**, flagged `refinement limit`, and leaves it
   `ready`. The planner shows the flag, so I decide there:
   - plan it anyway, and the open questions become the plan's;
   - abandon it;
   - or run `ais flow refine <ref>` for another round.

   The limit stops the automation, not me.

### 5.5 Config

```toml
[workspace.soi.refine]
profile = "claude"          # must have a batch template (§7)
prompt = "/refine-ticket {ticket}"
max_rounds = 3
sweep = "30m"
```

## 6. The task layer: `ais flow ticket`

Agents reach tickets only through flow. The commands are named by intent, not
by GitHub:

| Command | What it does |
|---|---|
| `ticket show <ref> [--context] [--json]` | The ticket: title, body (without the block), status, level and round, the parsed refinement block. With `--context`: ancestors, sub-issues, blockers with their last hand-off, linked PRs. |
| `ticket comment <ref> --file -` | Post a comment, such as a hand-off. |
| `ticket assess <ref> --level L2\|L3 --file -` | Record the assessment and write the block (§5.2). |
| `ticket refine <ref> --file -` | Write the L1 block and move the ticket to `refinement`. |
| `ticket close <ref> --done\|--not-planned --comment …` | Close a ticket with its evidence, or abandon it. |
| `ticket new --parent <ref> --title … --file -` | Create a follow-up sub-issue, with optional `--blocked-by`. |
| `ticket plan <ref> --url <pr>` | Link the plan PR on the ticket (§8). |

Status changes stay internal to the commands that imply them: `queue add`,
`start`, `refine`, `close`. Agents never set a status by name.

**Sources.** `internal/flow` gets a `Source` interface with these operations.
`github-project` is the only source today, and it implements the interface
with `gh`, as `source.go` already does for items and status. Every later
source (an issue search, Linear, a local inbox) implements the same interface,
so the skills don't change.

**Outside a workspace.** `work-tickets` is also used by hand in repos with no
workspace. There, `ais flow ticket` works out the source from the reference
itself: an issue URL or `owner/repo#n` is GitHub. Board-only operations (the
status, the `Autonomy` field) are skipped and the command says so.

**What stays outside the task layer:** git and PRs. They are the code host,
not the task source, so `git` and `gh pr` stay usable. The one exception is
the plan PR's lifecycle, which flow owns (§8).

The skills `work-tickets`, `plan-sessions` and `plan-to-tickets` are rewritten
to use `ais flow ticket` instead of `gh issue`/`gh project`.
`plan-to-tickets` is the largest change, because it needs `ticket new` with
sub-issue and blocker links.

## 7. Headless runs on every harness

Profiles gain a second template, next to `prompt`:

```toml
[profile.claude]
prompt = ["--", "{prompt}"]          # interactive, as today
batch  = ["-p", "--", "{prompt}"]    # run to the end, then exit
[profile.pi]
batch  = ["-p", "--", "{prompt}"]
```

Both are built in for claude and pi, since both have `-p`/`--print`. A profile
without `batch` can't be the `refine.profile`, and config loading says so.
Permissions come from the profile's own flags, as for any session.

`ais flow start` runs a refine proposal like any other: a tmux window, an ais
session record, `ais run … --batch --prompt …`. I can watch it, it shows in
tsm, and `ais notify` works. The one difference is at the end: **when the
harness exits, `start` marks the proposal `done` itself.** If the run exits
without calling `ticket assess` or `ticket refine`, flow records a failed
round. The ticket stays held for `refine.retry` (1 h) and the event is logged,
so a broken run can't spin.

Still to verify in a spike:

- whether `claude -p "/refine-ticket …"` invokes the skill;
- the same for pi's `-p` with a skill. If either doesn't work, the template
  says "Use the refine-ticket skill on …" instead;
- whether subagents work in `-p`.

## 8. The plan PR

### 8.1 How it works

At L2/L3, the work session writes its plan (shape in §9) and runs
`ais flow plan open`. That command:

1. writes `plans/<ticket-number>.md` in the worktree of the ticket's `Repo:`;
2. commits it alone, with the trailer `Ais-Plan: <ticket url>`;
3. pushes, and opens a draft PR titled `Plan: <title>` with `Part of <ref>`;
4. links the PR on the ticket (`ticket plan`), records phase `plan` on the
   proposal, and notifies me.

Revisions go through `ais flow plan update`, which makes another plan-only
commit with the same trailer, so I see the diff between revisions in the PR.

I review in GitHub: line comments, suggestions, "Request changes" to send it
back. When I'm done, I tell the session "reviewed" and it reads the review.

### 8.2 Approval removes the plan — in tooling

`ais flow plan approve` is the gate. It refuses unless the PR has an approving
review from me, or I run it myself with `--by-hand`. Then it:

1. copies the final plan into the PR description, under "Plan (approved
   <date>)";
2. **rewrites the branch without every commit carrying `Ais-Plan`.** Those
   commits touch only the plan file (`plan open`/`update` guarantee it), so
   dropping them can't conflict. Then it force-pushes with lease;
3. marks the PR ready for review;
4. records phase `approved` on the proposal.

Implementation commits come after this, on the same branch and the same PR.

**`ais flow plan check`** is the safety net. It fails if, in any of the
session's worktrees, `base..HEAD` contains a commit with `Ais-Plan`, or the
tree contains `plans/<n>.md` for one of the session's tickets. It runs:

- in `ais flow done` (which refuses to mark the session done while it fails);
- as a `pre-push` hook that `ais flow start` installs in the session's
  worktrees, but only after `approve`. Before approval, pushing plan commits
  is the point.

`work-tickets` also runs it explicitly in close-out, so a failure shows up
while I'm still there.

## 9. The plan document

This is what goes into the plan PR at L2 and L3:

```markdown
# Plan: <title>  ·  L2 (ceiling L3)  ·  <tickets>

## Why this level        — the readiness check, with evidence
## Goal and requirements — intent, Done-when as checks, out of scope
## Approach              — what changes, where, in what order; the
                           conventions and tools it follows (path:line)
## Assumptions           — numbered; what each rests on, what breaks if wrong
## Consequences          — for users, other code, ops, cost; what can't be undone
## Verification          — how each Done-when item will be shown to hold
## Open questions        — each with a default and its weight:
                           blocks the plan / changes a step / detail
## Standards gaps        — missing tests, rules or tools, each with a fix
                           (L3 always, L2 when found)
```

**L3 refines before posting.** It does up to three rounds of:

1. research, with subagents settling assumptions;
2. a fresh critic subagent reviewing against the rubric;
3. a revision.

It posts at L3 only if the critic raises no blocking finding and no open
question "blocks the plan". Otherwise it posts as L2 and lists what is
unresolved. Standards gaps become `AGENTS.md` lines in the PR, or new
sub-issues (`ticket new`).

## 10. Changes this needs

**ais / flow (Go):**

- **Profiles:** a `batch` template (built in for claude and pi) and
  `ais run --batch`.
- **Refinement:**
  - `proposals.kind` (`work`|`refine`);
  - a `refine` table (ticket, version, level, round, outcome, at);
  - the sweep in dispatch, and self-`done` on exit for refine runs;
  - `[workspace.<ws>.refine]` and the `refinement` status in `source`.
- **The task layer:**
  - a `Source` interface;
  - `ais flow ticket show|comment|assess|refine|close|new|plan`;
  - rendering and parsing of the refinement block;
  - a version hash;
  - level, reasons and round in `ais flow ready --json`.
- **The plan PR:**
  - `ais flow plan open|update|approve|check`, which can't reuse the planner's
    `plan` verb, so a new name is needed (§12, Q1);
  - the check inside `ais flow done`;
  - the pre-push hook.
- **Phases:**
  - `phase` and `plan_url` on proposals;
  - "waiting for review" and "still L1" groups in `ais flow ls`.

**Skills** (shared, via `agents-maintenance`):

- `~/.agents/shared/autonomy.md`: the levels, the rubric, the plan template,
  the L3 loop.
- **`refine-ticket`** (new, headless): §5.2. Its rules are to never ask
  interactively, never touch code, never edit my text, and to always end with
  `assess` or `refine`.
- **`work-tickets`:**
  - take `--autonomy`;
  - trust a current assessment, or re-check;
  - hand an L1 ticket back to refinement (`ticket refine`) and end there,
    rather than plan from it;
  - plan PR at L2/L3;
  - `ais flow ticket` instead of `gh issue`.
- **`plan-sessions`:** show level, round and the limit flag; take
  `--autonomy` on `queue add`; use `ais flow ticket` for reading tickets.
- **`plan-to-tickets`:** `ais flow ticket new`.

## 11. Build plan

1. **Spike.**
   - Does `claude -p` / `pi -p` run a skill from the first prompt, with
     subagents?
   - Can a refinement block survive real edits on gpwsoi/3?
   - Does a branch rewrite on a draft PR keep its review comments readable?
     They'd be marked "outdated", which is fine.
2. **The task layer.** The `Source` interface, `ais flow ticket show|comment|
   close|new`, and the move of `work-tickets` and `plan-sessions` off `gh`.
   This is useful on its own and makes everything after it source-agnostic.
3. **Plan PR.** `plan open|update|approve|check`, the check in `done`, the
   pre-push hook, and `work-tickets` at L2 with the plan PR. Usable by hand
   at once.
4. **Refinement.** `batch` templates, `kind=refine`, the sweep, self-done,
   `ticket assess|refine`, the block, the `refinement` status, and the
   `refine-ticket` skill. Live on soi with `max_rounds = 3`.
5. **L3.** The research/critic loop in `work-tickets`, tried on tickets that
   refinement scored L3.
6. **Later:**
   - the review nudge;
   - a weekly count of levels and their reasons per workspace, which shows
     which ticket-writing habits to fix;
   - an artifact view of big plans.

## 12. Open questions

1. **Naming.** `ais flow plan` already means the planner. The plan PR commands
   need another verb: `ais flow proposal open|update|approve|check`? Or
   `ais flow review …`? *Default: `ais flow review`.*
2. **Approval gate.** Require a GitHub "Approve" review, with `--by-hand` as
   the escape? On your own repos you can't approve your own PR. The session
   pushes as you, so GitHub won't let you approve a PR "you" opened.
   *Default: `--by-hand` is the normal path: you say "approved" in the session
   and it runs `approve`. A GitHub approval also counts where one is possible.*
3. **Refinement status name.** Does gpwsoi/3 get a new `refinement` option, or
   do we reuse an existing one (e.g. `Todo`)? A new one is clearer, and
   `ais flow install` can check that it exists. *Default: new option
   `refinement`.*
4. **Refine runs on the WIP.** Should they share the workspace's `wip`, or have
   their own small cap (`refine.wip = 1`), so a burst of ready tickets can't
   starve work sessions? *Default: their own cap of 1, inside the global
   `wip`.*
