# gotail — Claude project notes

## Design plan

The v2 design plan lives at `docs/V2_PLAN.md`. The shipped code drifts
from the plan in places; track every divergence in the
`## 11. Deviations` section at the end of the plan.

Whenever a change lands that takes the code further from (or back toward)
the plan, add or update an entry under §11.1–§11.5 with:

- the plan reference being deviated from (e.g. `§4 L3 Options`),
- what ships now versus what the plan promised, and
- a *Driver:* line linking to the review section or other source that
  motivated the change. If no review drove it, say *Pre-review
  (design-time choice)* or similar — never fabricate a back-reference.

## Reviews

Code, performance, security, and other reviews live in `docs/reviews/`:

- `docs/reviews/PERF_REVIEW.md` — performance & simplicity review.
- `docs/reviews/CODE_REVIEW.md` — end-to-end code review.
- `docs/reviews/AUDIT_CONTEXT.md` — shared mental model future audits
  reason against; *not* a findings doc.

When running Trail of Bits security skills, write all findings reports to ./docs/reviews/<skill-name>-<YYYY-MM-DD>.md unless the skill specifies otherwise.

Each review starts with a **Conducted** date and (when closed) a
**Closed** date in the header. New reviews go in the same directory
following the same header convention. When a review's findings translate
into deviations from `docs/V2_PLAN.md`, also record them in the plan's
Deviations section with a link back to the review.
