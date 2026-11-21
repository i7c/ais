<!-- ais-prompt: rubric v1 -->
## Autonomy levels and the readiness check

There are three levels of autonomy:

- **L1, refinement.** The ticket doesn't say enough to plan from. Its author
  gets questions, and nothing is planned.
- **L2, proposal.** There is enough for a plan. The plan's open questions are
  refined together with the user.
- **L3, refined proposal.** The same plan, but researched, critiqued and
  revised before the user sees it.

At every level, nothing is changed and no execution starts before the user
approves a plan.

**The level is `min(ceiling, evidence)`.** The user sets the ceiling. The
evidence is the readiness check below, run on the ticket and everything it
hangs off: its ancestors to the top, its blockers' hand-offs, the plan or ADR
it links, and the repo's `AGENTS.md` / `CLAUDE.md`. Answer each criterion with
the evidence it rests on (a quote, `path:line`, a URL), never an impression:

| # | Criterion | Met when | If not met |
|---|---|---|---|
| R1 | Intent | The ticket or an ancestor says why the work matters and what outcome it serves. | L1 |
| R2 | Done when | There are checkable acceptance criteria. "Works well" does not count. | L1 |
| R3 | Scope | "Out of scope" is stated, or the boundary is obvious from the ancestors. | L1 if the work could reasonably go two very different sizes, else L2 |
| R4 | Decisions | Every fork in the approach is decided upstream or has a stated default. | L1 if it changes *what* gets built; L2 if only *how* |
| R5 | Verification | A way to check the result exists or is named: tests, a command, a metric, a reviewer. | L2 at most |
| R6 | Conventions | The rules for this kind of work are written down: `AGENTS.md`, linters, test layout, a template or style. | L2 at most |
| R7 | Risk | Nothing irreversible or outward-facing is left open: prod data, deletes, cost, public APIs, security. | L2 at most |

The criteria apply to any kind of work. For research or ordering work, R5 is
"how will we know the answer is right", and R6 is "which template or standard
applies".

The evidence level is:

- **L1** if R1 fails, R2 fails, or R4 fails on *what* gets built;
- **L3** if every criterion is met;
- **L2** otherwise.
