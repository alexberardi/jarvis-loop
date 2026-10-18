# Correcting a filed item from the CLI (2026-10-18)

Item #202. On 2026-10-17 each mistake in a filed item (its text, a missing dependency, a wrong
resource tag) meant dropping it and filing it again; #94's resource tag took a database edit.

## Decision

Four user-only routes (a runner or orchestrator token gets 403; not on the orchestrator's
on-behalf list), each through `store.human`, so the change and a refusal are both events:

- **`POST /items/{id}/edit`** `{"title", "ask"}` (`loop edit`): only before build starts, that is
  in triage or on triage's question (`pipeline.BeforeBuild`); a build already works from the text.
  Kind `edited`, `data.title`/`data.ask` with `from` and `to`. `PATCH /items/{id}` keeps its title
  and priority as they were (runner tokens use it for priority).
- **`POST /items/{id}/link`** `{"after": [12, 13]}` (`loop link`): `OrchLink`'s checks, factored
  into `linkAfter` (open, not itself, the other open, not already, no cycle); all or none. Kind
  `linked`, `data.after` the list (the orchestrator's has one id).
- **`POST /items/{id}/unlink`** `{"after": 12}` (`loop unlink`): kind `unlinked`. With no
  dependency left the item is claimable at once: claims read `depends_on` as it stands.
- **`POST /items/{id}/resources`** `{"resources": ["macos"]}` (`loop resources`, `none` for none):
  only after triage (triage's outcome sets them), from the known set (`pipeline.Resources`). It
  applies from the next claim; a run in progress keeps its runner. The unservable check runs again
  in the same transaction, so a flag clears (or is raised) at once. Kind `resources`, `from`/`to`.

`loop rebuild` wraps the existing `POST /items/{id}/rebuild`; notes stay required. `loop add`
prints the item as JSON with `--json`, only its number with `-q`. The CLI's flags go anywhere on
the line; a single-dash word that is not a known short flag is refused instead of becoming a
positional (text that starts with `-` goes after `--`).
