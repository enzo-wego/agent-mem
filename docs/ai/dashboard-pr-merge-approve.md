# Request: PR merge + approve actions in the dashboard

Asked 2026-08-02 by Enzo. Round deferred at step 0 — context was ~153%.
Nothing has been investigated yet; treat everything below as the raw ask.

## The ask, verbatim

> if mergeable, could we add a menu like merge and it will squash merge
> automatically? and could we build a menu like approve for team's pr?

Two actions, presumably on whatever PR surface the dashboard already has:

1. **Merge** — visible only when the PR is actually mergeable, and it performs
   a **squash** merge, not a merge commit.
2. **Approve** — submit an approving review on a teammate's PR.

## Open questions to settle before planning

Do not guess these; ask.

- **Which surface?** The dashboard on `:34567` (`/live`, or another route)?
  There is an existing issue `agent-mem-6it` about ingesting wego/payments PRs
  into the graph — check whether a PR view already exists or whether this
  implies building one.
- **Which repos?** `wego/payments` only, or any PR in the graph?
- **Auth**: acting on GitHub as Enzo needs a token with write scope in the
  worker. Where does it live, and is the dashboard already authenticated well
  enough to be allowed to merge? This is the risky part — a mis-click merges
  someone's production PR.
- **Guardrails**: confirmation step? Only PRs where Enzo is a reviewer? Block
  merge when CI is red even if GitHub reports mergeable?
- **Approve semantics**: bare approval, or approval with a comment body? If a
  comment is involved, the `write-as-enzo` rules apply to the text.

## Constraints already known

- team-mode: plan → approval → `/team 1:omp`. No implementation by the lead.
- New settings must be editable in the dashboard GUI, not just config/DB.
- Dashboard changes need the embedded dashboard rebuilt before `make deploy`;
  never build on the VPS.
- Outbound text under Enzo's name (PR review comments) must be drafted against
  `~/.claude/skills/write-as-enzo/SKILL.md`.
