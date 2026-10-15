# Planning stages before the loop

- **Date:** 2026-10-14 (user)

New features start in a `#plan` channel. A product-persona agent works out the feature brief with
the user (who it is for, the problem, what they would actually want, success criteria, out of
scope); the user approves it. An engineering-persona agent then reads the brief and the code and
writes a feasibility note and a blueprint (approach, modules, interfaces, test plan, harness
verification plan, risks, a split into loop-sized pieces); the user approves it. `loopd` then
creates one item per piece, with dependencies between them and a link to the plan.

Briefs and blueprints are pages while being written (commentable from a phone) and a final copy is
committed to the target repo at `docs/plans/<feature>.md` when work starts.

Why: build agents implement better against a plan than a one-line ask, and the review and harness
gates can check the work against the blueprint's own verification plan. Built after rollout phase 4
(the runner), since the personas run as agents through it. Section "Planning, before the loop" in
docs/DESIGN.md.
