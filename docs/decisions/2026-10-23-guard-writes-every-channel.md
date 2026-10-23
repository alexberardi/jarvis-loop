# write_protected binds every write channel the guard can see

2026-10-23 · item #537. Seen on #512: before that build agent understood the boundary, two
bash-channel writes landed under jarvis-server's protected `tools/pipeline/` (both deleted by the
agent, nothing committed) while the same paths were refused on the Write/Edit channel. The
protection was tool-channel-dependent: `checkWrite`'s prefixes (`.git`, `.claude`, `.mcp.json`,
`.loop/brief.md` and the repo's `deny_write`) ran only for the file tools, and a shell redirect or
a write-shaped command slipped past. Enforced by `internal/guard` unit tests and
`scenarios/05-guard.json`.

## What was chosen

- **Redirection targets carry a direction.** The parser records which targets a segment writes
  (`>`, `>>`, `>|`, `>&`, `<>`; here-strings and `<` are reads), and `checkScript` refuses a write
  target that resolves under a protected prefix, with the same `write_protected` rule the file
  tools get.
- **Write-shaped commands, as a short explicit list** (like `never` and `checkRm`, not a
  heuristic): `tee`, `touch`, `mkdir`, `truncate`, `cp`/`mv`/`install`/`ln` (the destination: the
  last path argument, or `-t`'s), `dd of=…`, `sed -i` (its file arguments). Sources are left
  alone on purpose: protected files are legitimately read, run and copied out
  (`cat scripts/sandbox/run`, `go run ./tools/pipeline/x`), so only write-shaped positions may
  refuse — the guard never mention-matches `deny_write` paths across a whole command the way it
  does `Protected` ones.
- **Unresolvable targets stay usable.** A target with an unexpanded variable or substitution is
  refused only when its own text names a protected prefix on path-component boundaries
  (`$WT/.git/config` is refused, `$TMPDIR/f` and `.gitignore` are not). A relative target after
  `cd` cannot be resolved at all and is refused, as in `checkRm`; the reason says to use an
  absolute path.
- **The trusted-files flow keeps its gate, not its shell path.** An agent can no longer make a
  `deny_write` change with a redirect or a write-shaped command; what remains are the channels no
  PreToolUse hook can see (a build script, `go generate`, a git operation such as rebase, merge or
  apply), and for those the runner's verify still refuses the commit and holds it for the user's
  OK ([2026-10-16-trusted-files.md](2026-10-16-trusted-files.md)). The fake claude's trusted plays
  now model exactly that: a raw write beside a committed `git add`, not a redirect.

## Not chosen

- Mention-matching `deny_write` paths across the whole command (the `checkProtected` treatment):
  it would refuse reading and running the very files build agents must read and run.
- Refusing every unresolvable target: it would break legitimate `> $TMPDIR/f` and `tee $LOG`. The
  guard is a second line of defence — the OS sandbox fences the filesystem and verify holds
  `deny_write` diffs — so conservative-but-usable is the bar.
- Inspecting interpreter bodies (`python3 <<PY …`) for protected prefixes: without parsing the
  language there is no telling a write from the legitimate reads above; verify remains the gate
  for what gets committed.
