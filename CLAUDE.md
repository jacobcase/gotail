# gotail — Claude project notes

## Design plan

The v2 design plan lives at `docs/v2-plan.md`. The plan is a snapshot of
the current desired design state — it describes what the code does now,
not its history. When a change lands that alters a documented design
choice, edit the relevant section in place; do not keep "previously X,
now Y" notes or maintain a separate deviations log.

## Reviews

Code, performance, security, and other reviews live in `docs/reviews/`:

- `docs/reviews/perf-review-<YYYY-MM-DD>.md` — performance & simplicity review.
- `docs/reviews/code-review-<YYYY-MM-DD>.md` — end-to-end code review.
- `docs/reviews/audit-context.md` — shared mental model future audits
  reason against; *not* a findings doc.

When running Trail of Bits security skills, write all findings reports to ./docs/reviews/<skill-name>-<YYYY-MM-DD>.md unless the skill specifies otherwise.

Each review starts with a **Conducted** date and (when closed) a
**Closed** date in the header. New reviews go in the same directory
following the same header convention. When a review's findings change
the design described in `docs/v2-plan.md`, edit the plan in place to
match the new design.
