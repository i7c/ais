# Proposal: autonomy levels for ticket sessions

Status: **draft for review** (2026-11-21)

## 1. What this is for

`ais flow` starts sessions on its own now. Inside each one, `work-tickets`
does what it did when I started it by hand: it surveys, proposes a batch with a
short preview, and waits for my go. When the session was dispatched at 3 a.m.,
a preview is the least useful thing to find in the morning. I want the session
to go **as far as the ticket lets it** before it stops for me.

How far that is depends on the ticket. A ticket rarely holds everything an
agent needs, so a session must judge how much it can decide alone and say why.

There is one fixed rule at every level: **nothing is changed and no execution
tokens are spent before I approve a plan.** Autonomy here means how good the
plan is when I first see it. It does not mean doing the work unasked.

This proposal answers two questions:

1. How a session knows which level to work at (§3–§5).
2. How I review and approve a plan somewhere better than a terminal (§6–§7).

## 2. The three levels

| Level | The session hands me | I do | It stops when |
|---|---|---|---|
| **L1 — Questions** | What it understood, and the questions it needs answered before it can propose anything. That may be 3 questions or 30, grouped and each with a default where one exists. | Answer. | The questions are posted. After my answers it re-assesses, usually to L2. |
| **L2 — Proposal** | A plan with the full set of sections (§4) and its open questions. | Refine it with the session: ask for research or evidence, change it, approve it. | The plan is posted. |
| **L3 — Refined proposal** | The same plan, after the session has researched, critiqued and revised it itself (§5). It also lists the standards it found missing and proposes how to add them. | Read and approve, or send it back. | It passes its own review, or it gives up and drops to L2. |

L1 is not a failure. A broken plan built on guesses costs me more than a good
list of questions.

## 3. Choosing the level: a ceiling from me, evidence from the ticket

Two inputs decide the level:

- **The ceiling.** This is the highest level I allow. I set it, or I accept the
  planner's suggestion (§3.2). It defaults to L2.
- **The evidence.** This is the level the ticket actually supports, from a
  readiness check the session runs on its context (§3.1).

**The session works at `min(ceiling, evidence)`.** It may always go lower than
the ceiling, but never higher, and it says why in the first line of what it
posts:

> Working at **L1** (ceiling L3): the ticket has no checkable "Done when", and
> two ancestors disagree about whether the old API stays.

This keeps experiments cheap: I can raise the ceiling on a whole workspace and
still trust that an under-specified ticket comes back as questions.

### 3.1 The readiness check

The session scores its context, which is the ticket and everything
`work-tickets` already reads up the chain: ancestors, blockers' hand-offs, the
plan, and the repo's `AGENTS.md`/`CLAUDE.md`. Each criterion is answered with
the evidence it rests on, not with a vague impression.

| # | Criterion | Met when | If not met |
|---|---|---|---|
| R1 | **Intent** | The ticket or an ancestor says why the work matters and what outcome it serves. | L1 |
| R2 | **Done when** | There are checkable acceptance criteria. "Works well" does not count. | L1 |
| R3 | **Scope** | "Out of scope" is stated, or the boundary is obvious from the ancestors. | L1 if the work could reasonably go two very different sizes, else L2 |
| R4 | **Decisions** | Every fork in the approach is either decided upstream or has a stated default ("Decide while working"). | L1 if a fork changes what gets built; L2 if it only changes how |
| R5 | **Verification** | A way to check the work exists or is named: tests, a command, a metric, an acceptance suite. | L2 at most |
| R6 | **Conventions** | The repo states its rules: `AGENTS.md`, linters, test layout, commit style. | L2 at most |
| R7 | **Risk** | Nothing irreversible or outward-facing that the ticket leaves open: prod data, deletes, cost, public APIs, security. | L2 at most, whatever else is met |

The evidence level is:

- **L1** if R1, R2 or a "changes what gets built" R4 fails;
- **L3** if everything passes;
- **L2** otherwise.

R5–R7 cap the level at L2 rather than forcing L1. Missing tests or rules make
a plan less certain, but they don't make one impossible.

The rubric lives in one place: a reference file shared by `work-tickets` and
`plan-sessions`. Both use the same words for the same gaps.

### 3.2 Where the ceiling comes from

First match wins:

1. **The session's prompt.** `ais flow queue add --autonomy L3` stores it on the
   proposal. The session template gets an `{autonomy}` placeholder:
   `/work-tickets {tickets} --autonomy {autonomy}`. By hand: `/work-tickets #42
   --autonomy L3`.
2. **The ticket.** An `Autonomy` single-select field on the project (`L1`/`L2`/
   `L3`), so I can set it on the board while I triage. A label `autonomy:L3`
   is the fallback for search sources.
3. **The workspace.** `[workspace.<ws>.session] autonomy = "L2"`.
4. **Default:** L2.

**The planner suggests a ceiling.** `plan-sessions` reads every ticket in
full anyway, so it can run the same readiness check. Its table gains a column:

| # | title | tickets | repos | after | readiness | suggested level | why this grouping |
|---|---|---|---|---|---|---|---|
| 1 | Retry webhooks | #41 | backend | — | R1–R7 ✓ | L3 | … |
| 2 | Export v2 | #44 | backend, frontend | 1 | R2 ✗, R5 ✗ | L1 | … |

When I accept a proposal I accept its level too, or change it ("2 at L2"). This
needs no new habit from me, because I already read that table.

**Optional: asking questions before queuing.** If the planner finds an L1
ticket, it could post the questions on the ticket instead of proposing a
session. The ticket then stays `ready`, and a session starts once the answers
are there. That saves a session slot, but it puts work into the planner that
it doesn't do today. I'd leave it out of the first version (§9, Q4).

## 4. The plan document

L2 and L3 produce the same document, and L1 produces its first two sections.
It has a fixed shape so I learn where to look:

```markdown
# Plan: <title>   ·  Level L2 (ceiling L3)  ·  <tickets>

## Why this level
<the readiness check: each criterion, met or not, with its evidence>

## Goal and requirements
<intent in my words, quoted from where it came from; the Done-when list,
 restated as checks; what is out of scope>

## Approach
<what will change, where, in what order; which repo conventions, tools and
 tests it follows, citing AGENTS.md / existing code by path:line>

## Assumptions
<numbered; each says what it rests on and what breaks if it is wrong>

## Consequences
<what changes for users, other code, ops, cost; what becomes harder later;
 what can't be undone>

## Verification
<how each Done-when item will be shown to hold: the command, the test, the
 metric>

## Open questions
<numbered, each with the default the session will take if I don't answer,
 and how much the answer matters: blocks the plan / changes a step / detail>

## Standards gaps            (L3 always, L2 when found)
<missing tests, rules or tools that forced an assumption, each with a
 proposed fix: a sub-issue, an AGENTS.md line, a check script>
```

L1 posts **Why this level**, then **What I understood**, then **Questions**
(grouped by topic, with defaults where possible). It also says what each answer
unlocks.

**Answers improve the ticket, not just this session.** Once I've answered,
the session writes the answers into the ticket body (a "Decisions" or "Decide
while working" section), so the next session or a re-run starts higher. This
is what makes L3 more common over time without depending on me again and again.

## 5. L3: how the session refines on its own

At L3 the session drafts the L2 plan, then runs a bounded loop before it posts
anything:

1. **Research.** For each assumption or open question that evidence could
   settle, it sends a subagent:
   - `code-locator` or `Explore` for "where / how is this done here";
   - `web-research` for "how is this usually done", archived as usual.

   It cites the result in the plan and moves the item from *Assumptions* to
   *Approach*.
2. **Critique.** A fresh subagent gets the ticket context and the plan, but not
   the drafting conversation. It reviews against the rubric and the plan's own
   Done-when. It looks for:
   - unstated assumptions;
   - steps with no verification;
   - scope creep;
   - conventions ignored;
   - irreversible steps.
3. **Revise,** and repeat 2–3 at most **three rounds**.
4. **Exit check.** The plan is posted at L3 only if:
   - the critic raises no blocking finding;
   - every remaining open question has a default and is marked
     "detail" or "changes a step", not "blocks the plan".

   Otherwise it is posted as **L2**, with the unresolved findings listed as
   open questions. "Dropped from L3: the critic's finding 2 needs your call."

**Standards gaps are the L3 deliverable that compounds.** Whenever research or
the critic had to assume something a repo rule or test would have told it, the
gap goes in *Standards gaps* with a concrete fix. I approve those like the rest
of the plan. Small ones (an `AGENTS.md` line) can ride along in the PR. Larger
ones (a test harness, a check script) become new sub-issues of the same parent,
as `work-tickets` already does for found work.

**Cost.** An L3 plan spends maybe 2–5× the tokens of an L2 plan. That is still
small next to execution, and it is spent while I'm away instead of while I wait.
Flow's rate limits still apply only to starts, which is fine for now.

## 6. The approval gate

Today "approval" is a message I type in the session. I keep that, and add a
recorded state so flow and I can see which sessions are waiting on me:

| Session phase | Set by | Seen as |
|---|---|---|
| `assessing` | session start | tsm/`ais flow ls`: working |
| `questions` (L1) | `ais flow phase questions` | waiting on me, with a link |
| `plan` (L2/L3) | `ais flow phase plan --level L3 --url …` | waiting on me, with a link |
| `approved` | the session, when I approve | working |
| `executing` → `done` | as today | |

`ais flow phase` is one small command that writes the phase and the plan's
link on the proposal, logs an event, and calls `ais notify`:

```
soi: "Retry webhooks" plan ready (L3, 2 questions) — <link>
```

`ais flow ls` then shows a **waiting for review** group: phase, level, age,
link. The skill instructions forbid any edit or execution before the
`approved` phase. The phase field makes it auditable: the hand-off can say
"approved 09:14 after 2 rounds".

## 7. Reviewing somewhere better than a terminal

What I need from a review surface:

- a rendered document;
- comments **on a specific line**;
- reachable from my phone;
- durable, attached to the ticket;
- the session can read my comments back without me copy-pasting.

The candidates:

| Option | Rendered | Line comments | Phone | Durable / on the ticket | Back to the session | Cost to build |
|---|---|---|---|---|---|---|
| **A. Issue comment** with the plan | ✓ | ✗ (reply quotes it) | ✓ GitHub app | ✓ | `gh issue view --comments` | none |
| **B. Draft "plan PR"**: `plans/<ticket>.md` on the session branch | ✓ | ✓ native review | ✓ | ✓ linked to the ticket | `gh api …/pulls/<n>/comments`; Approve = the gate | small |
| **C. Claude artifact / Claude Docs** page | ✓ richest (diagrams, collapsible evidence) | ✓ | ✓ | ✗ lives on claude.ai, not on the ticket | comments reach the session only while it runs, and only for Claude Code | small, Claude only |
| **D. Local file** in the session dir + `glow`/editor, or a tsm pane | ✓ | ✗ | ✗ | ✗ | trivial | none |
| **E. Own web UI** served by ais | ✓ | ✓ | needs a tunnel | depends | ✓ | large; §11 of the flow proposal rules out a web UI |

**Recommendation: B, with A as the record and D as the offline path.**

- **The plan is a draft PR.** The session commits `plans/<ticket>.md` on its
  own branch and opens a draft PR titled `Plan: <title>` with `Part of <ref>`.
  - I review in GitHub's PR view: line comments, suggestions, "Request
    changes" to send it back, **"Approve" to approve**.
  - The session reads the review with `gh` when I tell it "reviewed" in the
    terminal. Later, a dispatch-tick check can notice a new review and nudge
    the session for me (§9, Q3).
- **After approval** the same PR becomes the implementation PR. The session
  deletes `plans/<ticket>.md`, so the plan never lands on main. It copies the
  final plan into the PR description, and the commit history keeps the plan
  and every revision. For multi-repo sessions, the plan PR lives in the repo
  the ticket's `Repo:` line names, and it links the other repos' PRs.
- **The ticket gets a short comment** (option A): level, a one-paragraph
  summary, the link, and the open-question count. Anyone reading the ticket
  sees there is a plan waiting, without the full text duplicated.
- **L1 questions go on the ticket itself** as a comment, not in a PR. There is
  no branch-worthy content yet, and the answers belong on the ticket (§4).
  I answer in a reply comment, numbered to match.

Why not C as the default: it is the nicest to read, but it ties the workflow to
Claude Code and a claude.ai account. pi sessions couldn't use it. The plan
would also live away from the ticket. It is a good optional extra: a
`work-tickets` flag that also publishes the plan as an artifact for big plans
with diagrams.

## 8. Changes this needs

**Skills** (shared, in `~/.agents/skills/`, via `agents-maintenance`):

- `work-tickets/references/autonomy.md`: the levels, the rubric, the plan
  template, the L3 loop. This is the single source both skills cite.
- `work-tickets`:
  - read `--autonomy` from the arguments and the ticket's field;
  - run the readiness check;
  - in step 2, produce L1 questions or an L2/L3 plan instead of the batch
    preview;
  - write answers back into the ticket;
  - never move to step 3 before `approved`.

  Hand-started sessions keep working as today, because the default is L2 and
  an L2 plan is a superset of the current preview.
- `plan-sessions`: add the readiness column and suggested level, and
  `--autonomy` on `queue add`.

**ais flow:**

- `proposals.autonomy` column, `queue add/edit --autonomy`, `{autonomy}` in
  the session template.
- `proposals.phase`, `plan_url`, `phase_at`; `ais flow phase`; a
  "waiting for review" group in `ais flow ls`.
- Source: read an `Autonomy` field if the board has one (optional, like
  `Status`).

**Not changed:** the dispatcher, limits, the ticket status contract. Phases are
inside `In Progress`; they don't touch the board's Status.

## 9. Open questions for me

1. **The ceiling's default:** L2 (no change in behaviour until I raise it), or
   L3 for flow-dispatched sessions only? *Default: L2 everywhere; raise per
   workspace once I've seen a few L3 plans.*
2. **Review surface:** draft plan PR (B) as recommended, or issue comment only
   (A) to start with less? *Default: B.*
3. **Getting my review back to the session:** do I tell the session "reviewed"
   myself, or should the dispatch tick watch plan PRs and type a nudge into the
   waiting pane? *Default: by hand first; the nudge is a later step.*
4. **L1 in the planner:** should the planner post L1 questions on tickets
   instead of queuing a session for them (§3.2)? *Default: no, not in v1.*
5. **Rubric thresholds:** is R7 (risk) capping at L2 right, or should risky
   work force L1? *Default: cap at L2, since the plan is reviewed anyway.*

## 10. Build plan

1. **Rubric and template.** Write `references/autonomy.md`. Teach
   `work-tickets` L1/L2 with the plan as a local file plus a ticket comment
   (A + D). Usable by hand at once: `/work-tickets #n --autonomy L2`.
2. **Plan PR.** Add review surface B to `work-tickets`, with Approve as the
   gate and the cleanup after approval.
3. **flow plumbing.** `autonomy` and `phase` on proposals, `ais flow phase`,
   the review group in `ais flow ls`, the readiness column in `plan-sessions`.
4. **L3.** Add the research/critique loop and standards gaps to `work-tickets`.
   Try it on two or three soi tickets that pass R1–R7, and compare against
   L2 runs of similar tickets.
5. **Later:** the review nudge (Q3), planner-side L1 (Q4), the artifact as an
   extra surface, and a per-workspace report of how often each level happened
   and why. The "why" column shows which ticket-writing habits to fix.
