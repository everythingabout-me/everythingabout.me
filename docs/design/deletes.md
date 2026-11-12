# Deleting a subtree: settle the index first, purge the bytes after

> Accepted 2026-11-10. Supersedes the frozen-manifest delete for the `/api/files` tree; move and copy keep
> their manifests. Implementation is staged — see [Status](#status).

## The costs we were conflating

A recursive delete has two costs with opposite requirements:

| cost | who waits | must be |
|---|---|---|
| the user's decision becoming true | the person who clicked | tens of milliseconds, independent of size |
| the bytes actually leaving the provider | nobody | complete, resumable, retryable for minutes if it has to be |

The current design puts both on the request path. Before it answers `202`, `Prepare` walks the subtree twice
(capture, then a consistency re-read) and freezes one manifest entry per object, because the deletion set must
be decided before it can be concealed and before it can be journaled.

Measured on one real folder of 48 subfolders: a single walk is 144 COS requests after the latest listing
fixes, 376 before them, and a delete paid three passes (two walks plus a per-entry `Stat`). Acceptance of that
folder took 20.0s (752 requests) before the listing fixes and 10.4s (288 requests) after them; the 103-entry
folder that started this investigation took 23.6s. A mainstream drive answers the same click in 100–300ms,
because the thing it deletes is its own metadata tree, not somebody else's objects.

## Decision

Three steps. Step 1 and 2 happen in the acceptance transaction; only step 3 is background.

1. **Resolve** the target: one or two native observations (the folder's parent listing, its marker `HEAD`,
   and the requested revision check). O(1).
2. **Settle the index** in one transaction: record the deletion facts for the subtree, refund the quota of
   the rows, delete the rows. O(rows), local, one statement. Then answer `202`.
3. **Purge the bytes** in the worker: walk the native prefix page by page and delete what step 2 says is
   gone. O(objects), network, retryable, parallelizable.

The client's confirmation is therefore final when `202` arrives: the subtree is gone from every read because
the rows are gone, and no other mechanism has to agree with them.

Cost after this change: acceptance is `O(1)` native requests plus one local transaction — about 10ms for a
small folder and roughly 0.5–2s for the 100k-entry ceiling, against 10–24s today.

## Why the rows go first, not "mark them later"

The obvious cheaper variant is: accept after step 1, hide by prefix, mark the rows in the background (step 2
runs late). It moves the same amount of work, and it costs a protocol:

- Between step 1 and step 2 the rows are still live, so "the path is free" cannot be answered by the index.
  A create in that window has to be classified as *adoption*: an exact-path release record, an exemption in
  the read filter, a `mkdir` that reuses an existing marker instead of `If-None-Match`, and a create that
  overwrites an object it cannot prove it may overwrite.
- Every one of those is a new state that recovery, idempotent replay and the guard machinery have to agree
  with. The alternative is: the create's `If-None-Match` fails until the purge reaches that object, so a
  folder deleted and re-created within seconds is refused for minutes.

Settling the rows inside the acceptance transaction removes the window instead of governing it: after `202`
there is no pending state to classify, because the index already says the subtree is gone. A create is an
ordinary create. The price is one transaction proportional to the subtree, which is the cheapest thing in the
whole operation.

This is also why the guard disappears from the delete path: it existed to make "concealed" and "in use" the
same fact for the length of the purge. Concealment is now carried by the rows themselves, and the namespace is
free the moment they commit.

## The purge invariant

One sentence, used by both the worker and (later) the index rebuild:

> Delete a native object when its path is under the accepted prefix, no live index row exists at that path,
> and — when the provider reports a clock we can compare — the object was observed before the acceptance time.

Consequences:

- **A re-created path is never deleted.** The user re-creates `/X`, uploads `/X/a.txt`, then the purge reaches
  them: both have live rows, both are skipped. The old children of `/X` have no rows and are removed, so the
  re-created folder ends up containing exactly what the user put there.
- **A publish that has not settled yet is never deleted.** An upload writes the object before its index row,
  so "no live row" alone would race it. A commit holds its guard from the freeze to its settlement, so a
  delete over it is not accepted until the row exists; a commit that starts after the acceptance can only
  target a folder the index holds again, and its object is newer than the acceptance, which the purge adopts.
  The conditional delete's `If-Match` is a further fence.
- **A failed conditional delete is not an operation failure.** `412` means the object changed after we
  observed it — the user got there first. The entry is reported as `adopted` and the purge moves on. Today the
  same event fails the whole operation.
- **Two purges of the same prefix are harmless.** Both skip live rows; the rows were removed once, at the
  first acceptance, so the quota is refunded once.

## What this gives up, and why it is acceptable

The rule "never rescan and remove new external additions" is replaced by the invariant above. Concretely, an
outside writer (rclone, Finder, a mount) that writes into the folder *after* the `202`:

- before this change: the file survives and the folder reappears holding it;
- after this change: the file is deleted with the folder.

The reason the new answer is the better one: at `202` the subtree stopped existing for the user, so a file
written there afterwards was never visible to them and cannot be missed. The old rule protected a file the
user could not see from a deletion they had already confirmed. The one case that must keep working — the same
user re-creating the path through this service — is protected by the live-row rule, not by refusing the
delete.

## An open upload inside the subtree does not fence the delete

An upload session takes its path guard when it commits, not when it is created
([api.md §7](../api/api.md#7-upload-sessions-and-transfer-classes)). A session that is still `open` is staged
parts and a reservation, with no native effect behind it, so a delete over it is an ordinary delete: nothing
to wait for, nothing to cancel, and the delete does not know the session exists.

What happens to the session is decided when it commits. Its commit runs in one transaction with the guard it
takes and a check of the index: the folder it was opened for must still be indexed at the same path under the
same identity. After this delete's acceptance the rows are gone, so the commit is refused (`parent_missing`,
or `target_changed` when the folder was created again) and the session ends without publishing. Both
transactions take the owner lock, so there is no interleaving in which the commit sees the folder and the
publication lands after the acceptance.

A session whose commit already started holds a guard, so the delete is refused with its `operationId` until
that publication settles — seconds, not a session lifetime.

This replaced two compensations for a guard held from creation. First the browser cancelled the session a
`409` named and retried, bounded at 32 attempts; then the acceptance cancelled every open session inside the
subtree itself. Both treated the symptom: the same guard still refused a rename of the folder, a delete of a
single file with a pending replacement, any client that did not know to cancel first, and it made the delete
abort uploads as a side effect. Measured case that started it: a 3,200-file import left 188 sessions behind
when it stopped between allocating and committing, and the folder holding them answered
`operation_in_progress` for every delete attempt until the sessions expired the next day.

## Races

| moment of a create | result |
|---|---|
| after `202`, folder re-created | ordinary `mkdir`; the purge skips it (`X/` has a live row); old children are still purged |
| after `202`, file re-uploaded at an old path | ordinary create; the purge skips it; the old object is overwritten or deleted-then-rewritten by the upload itself |
| during the purge, same path | first writer wins natively; the loser gets `412`/`409` and is retried by its own client; the purge treats `412` as `adopted`, never as an error |
| upload session opened inside the folder before the delete, committed after `202` | the commit is refused before any native effect (`409 parent_missing`; `412 target_changed` if the folder was created again) and the session ends `failed`; nothing is published under the deleted prefix |
| second delete of the same prefix | both walks proceed, both skip live rows; no double refund, because the rows left the index once |
| DB lost during the purge | the folder resurrects with whatever bytes were not yet purged. Closed by the tombstone below; without it this is the accepted cost of a disposable index |

## Rejected alternatives

- **Freeze the manifest before accepting (today).** Bounded by pages and entries, not by a clock, so a large
  folder simply takes minutes to answer; measured 10–24s for 48–103 entries. It buys "delete exactly what the
  user saw", which the live-row invariant buys for the only case that matters.
- **Accept, then mark the rows later.** Correct only with an adoption/release protocol (see above): an
  exact-path release table, a `mkdir` that reuses markers, overwrite semantics on create, and an exemption in
  the read filter. That is a new state machine to avoid a 5ms transaction.
- **Quarantine by renaming the subtree** into a private prefix, then purge there. In an object store a folder
  rename is a copy per object, so it turns one O(N) network pass into two. It is attractive only for a
  filesystem driver with a native rename.
- **Prefix purge without the live-row test.** Deletes whatever the user re-created while the purge runs: data
  loss of content the user can see, which is the one thing a delete must never do.

## What changes for clients

- `202` now means: the subtree is gone from every read, permanently. There is no longer a window where a
  folder is deleted but still listed.
- The operation no longer fails because the native purge failed. It stays `running` with `completed/total` in
  provider objects while the walk runs, then `succeeded`; a purge that cannot make progress stays `running`
  with its retry state and is visible as such. There is no "partially deleted" outcome on the index side,
  because the index was settled atomically.
- `result` appears when the operation is terminal, not at acceptance. A delete's answer is
  `{deleted,purged,adopted}`; while it runs, `result` is null. `total` is null only until the first walk has
  counted the prefix, and a `succeeded` purge ends on `completed == total`.
- `409 operation_in_progress` no longer appears for a path that was just deleted: the name is free as soon as
  the acceptance commits.

## Status

- [x] Listing/resolve cost fixes that made a pass cost one request per folder instead of one per entry and per
      path segment (already on `main`).
- [x] Acceptance stops walking: it resolves the target and answers `202`, and the worker collects the manifest
      before the first native deletion. The confirmation is now `O(1)` in the subtree; at that step the guard,
      the frozen manifest and the per-entry settlement were unchanged, which step 2 then replaced.
- [x] Acceptance transaction (step 2) and prefix purge (step 3) for `delete`: the acceptance removes the
      rows, records one subtree change fact and refunds the quota in its own transaction; the purge walks the
      prefix and deletes what no live row and no later write claims, re-walking on resume.
- [x] Purge liveness: a path with a live index row or an observation after `AcceptedAt` is adopted, a delete
      that comes back `changed`/`precondition_failed` is adopted rather than fatal, and a folder that gained a
      child during a pass is retried (bounded).
- [x] Purge cost: one conditional request per object and eight deletes in flight. A settled deletion does not
      need the journalled removal — nothing has to be attributed later, `If-Match` carries the identity, and a
      lost reply is answered by the same re-walk that resumes a crash — so the driver gets a `Purge` that skips
      the receipt and the two re-observations `Remove` makes. Folders go after their children, and a folder is
      kept when anything under it survived the pass, which the walk already knows without another request.
      Measured on 80 objects: 94s → 2.5s.
- [x] Operation resource: `purged`/`adopted` move while the purge runs, per page of 200 objects, and the
      total is published from the walk instead of staying null. The result moved to the terminal state in the
      same change: it used to be written by the acceptance transaction, so a running operation already
      answered "deleted" — the inverse of the gap this item described.
- [ ] Fence re-created folders against the folder-marker delete. The live-row test runs before the conditional
      delete, and a folder re-created over a marker the purge has not reached yet adopts that marker without a
      native write. A create whose row commits between the check and the delete therefore has a live row and
      an unchanged native revision, so the purge deletes the folder the user just re-created (observed as a
      `200` mkdir followed by `409 parent_missing` on the next upload). Closing it needs one of: the
      re-creation reasserting the marker so `If-Match` refuses the delete, or the purge restoring a folder it
      deleted after a live row appears. Until then the write path is not concealed, but this ordering is not
      fenced.
- [ ] Tombstone object (`__eam_system__/<owner>/tombstones/<operation>`) so a rebuild after index loss does not
      resurrect a half-purged delete. Same predicate as the worker; the tombstone is the durable copy.
- [ ] `move`/`copy` keep the frozen manifest: their effects are conditional on both endpoints and a re-walk
      cannot reconstruct "what the user meant" the way a prefix can.

Acceptance and the prefix purge are implemented, so the delete no longer outlives acceptance with a guard: the
name is free at once, a re-created path is an ordinary create, and only reads still conceal the subtree until
the purge is terminal. [api.md](../api/api.md) states that rule in the mutation section; the unchecked item
above names the one ordering the purge does not yet fence.
