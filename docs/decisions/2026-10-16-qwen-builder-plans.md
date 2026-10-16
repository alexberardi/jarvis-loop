# Procedural piece plans, and a builder per piece

2026-10-16 · item #31. A trial: three dummy features in jarvis-loop (`loop status --repo/--open`,
the bounce count on board cards, a 120-character title limit), each written up as a step-by-step
plan and built by Qwen3.8-27B through OpenCode. Claude's review approved two on the first try and
the third after one mechanical round. The plans are kept as `skills/engineering/examples/F1.md`,
`F2.md` and `F3.md`.

- **Pieces are procedural plans** (`skills/engineering/SKILL.md`): besides what it changes, each
  blueprint piece carries a goal, numbered steps in order naming the exact files and
  functions/types (with line hints or what to search for), the code shape where it matters, the
  tests to add case by case, the commands that prove it (`go vet ./... && go test ./...`, a
  scenario, a golden and its diff) and its scope. The section still becomes the item's ask, so
  the steps reach the build agent with no other change.
- **`builder: qwen|claude`** is a new piece line next to `repo:`, `size:` and `depends on:`.
  `qwen` only when every step is mechanical: no design decision left, at most about 8 files
  touched, the tests specified. Otherwise `claude`.
- **Validation**: `pipeline.ParseBlueprint` reads the line (case-insensitive, bold or backticked
  like the others) and refuses any other value (`piece N: builder must be qwen or claude`). The
  runner checks a turn's blueprint with it, so a bad builder fails the turn like a bad size.
- **Missing builder → `claude`**: a piece without the line is filed with `claude`, the safe
  builder, so blueprints written before this change still hand off.
- **Stored, not acted on**: `items.builder` (migration 0011; `''` for items not from a plan) is set
  when the hand-off files the item and never changes. Nothing routes on it yet: it shows on
  `/loop show` (`· builder qwen`) and on the board card (`builder: qwen`). Routing `qwen` pieces to
  the Qwen builder is a later item (#32).
