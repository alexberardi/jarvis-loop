# Dependencies are met at merge, not at QA (2026-10-18)

Item #193. An item waited until every item it depended on was **done**; an item in qa (merged,
waiting for the user's check) held its dependents back. With 12 items in qa that idled slots: #61,
#63 and #121 sat behind #94's check. The user's goal (2026-10-17): the QA column holds only what
needs the user's eyes, and nothing else waits on it.

## Decision

- A dependency is met when the item it names is **merged**: stage `qa` or `done`
  (`pipeline.DependencyMet`). `WaitingOn` (the claim, "waiting on #12" in Slack, `/loop show`, the
  orchestrator snapshot) and `WaitChains` (the board's "waiting on #12 → …") both use it, so a qa
  item is never a link of a wait chain. A closed dependency still moves its dependents to needs-you
  (`ApplyDependencyClosed`), unchanged.
- QA goes on in parallel. When it fails (`store.Reopen`: qa → done plus a new P2 item), loopd, in
  the same transaction, writes a `dependency_reopened` event (data `dependency`, `reopen_item`,
  `words`) on every **open** item (not done or closed) whose `depends_on` names it. Nothing else
  changes: no stage move, no lease ended, no new dependency on the reopen item. Running work keeps
  running.
- The Slack bot posts it in the dependent's thread: "This depends on #94, which was reopened: <words>
  (the fix is #201). The work here goes on; check whether it needs a change." No mention: the user
  is the one who just reopened it (or will be told by the reopen item's own thread).
- The board card shows "Depends on #94, which was reopened: <words> (the fix is #201)", and the
  orchestrator snapshot carries it as `reopened_deps`. The flag is derived from the events, with no
  table of its own: it shows while the dependent is open **and** the reopen item is not yet merged
  (not qa, done or closed). So it clears when the dependent is done or closed, or when the fix
  merges or is dropped.
- Hold rules are unchanged.

## Why

Most qa checks pass; making every dependent wait for the user's eyes costs a slot per qa item for
hours. When one fails, the dependent's work is usually still right (the reopen is a follow-up item,
not a revert), so a flag the user can act on beats stopping it. A merged-but-reopened dependency
stays met: the merged code is still on main.

## Not done

- A dependent that is done (or closed) gets no flag; it is past the point where the flag changes
  anything. A dependent in qa is flagged: the user is about to check it, so that is where the
  flag helps most.
- The flag does not make the dependent wait on the reopen item; add the dependency by hand if it
  should.
