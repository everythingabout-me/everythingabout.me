package fileops

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"everythingabout.me/backend/internal/storage"
)

// acceptDelete makes a recursive deletion final before answering: the target's
// identity is observed, then one transaction removes the subtree from the index,
// records the change facts, refunds the quota and releases the guard that fenced the
// acceptance. Nothing native is deleted here, so nothing can fail in a way the user
// has to understand — a missing path or a stale revision still answers synchronously,
// because the observation happens first.
func (a *Actions) acceptDelete(ctx context.Context, req ActionRequest, op Operation, native NativeActions) (Operation, error) {
	if a.options.PrefixCommit == nil || a.options.LivePaths == nil {
		return op, E("unsupported", nil)
	}
	target, err := observe(ctx, native, req.From)
	if err != nil {
		if !storage.IsCode(err, "not_found") {
			return op, err
		}
		// The bytes are already gone: removed outside the service, or a folder that only
		// ever existed as the prefix of its children. The index is a cache of the tree, so
		// the deletion settles the rows the caller can still see instead of answering 404
		// for an entry nothing can remove any other way. The index still decides whether
		// there is anything to delete — a path it never held is not_found, exactly as
		// before — and the tree still decided the recursion question above, when the
		// target existed.
		live, liveErr := a.options.LivePaths(ctx, req.Root, []storage.Path{req.From})
		if liveErr != nil {
			return op, liveErr
		}
		if len(live) != 1 || !live[0] {
			return op, storage.NewError("not_found", req.From, nil)
		}
		accepted, acceptErr := a.journal.AcceptPrefixDelete(ctx, op.Lease, req.From, a.options.PrefixCommit)
		if acceptErr != nil {
			return op, acceptErr
		}
		return accepted, nil
	}
	if target.Kind == storage.Folder && !req.Recursive {
		// A non-recursive folder deletion is an empty-folder deletion. The purge walks a
		// prefix, so this decision cannot be deferred: accepting it here would remove a
		// subtree the client never asked to recurse into.
		page, listErr := native.List(ctx, req.From, storage.PageRequest{Limit: 1})
		if listErr != nil {
			return op, listErr
		}
		if len(page.Entries) > 0 || page.NextCursor != "" {
			return op, E("directory_not_empty", nil)
		}
	}
	accepted, err := a.journal.AcceptPrefixDelete(ctx, op.Lease, req.From, a.options.PrefixCommit)
	if err != nil {
		return op, err
	}
	return accepted, nil
}

// purgePrefix deletes the bytes of an accepted deletion. It is not a manifest replay:
// the index already forgot the subtree, so the purge lists the prefix, deletes what
// no newer intent claims, and records how far it got. Re-running it is how recovery
// works, and every step tolerates finding the object already gone.
func (a *Actions) purgePrefix(ctx context.Context, root Root, actor Actor, op Operation, native NativeActions) (Operation, error) {
	if op.Purge == nil || op.Purge.Path == "" {
		return op, E("invalid_intent", nil)
	}
	op, err := a.journal.Claim(ctx, root, actor, op.ID, a.options.LeaseTTL)
	if err != nil {
		return op, err
	}
	// Read the intent after claiming: the journal hands back a fresh snapshot, and a
	// pointer taken before the claim would be updated by nobody.
	purge := op.Purge
	if purge == nil || purge.Path == "" {
		return op, E("invalid_intent", nil)
	}
	work, stop := a.service.keepLease(ctx, op.Lease)
	defer stop()
	// Deliberately no Authorize here. The decision was authorized and committed when it was
	// accepted, and the path it named has stopped existing: re-asking would either fail on
	// the deleted path or, worse, decide a new question ("may you delete what is already
	// gone") whose only answers are wrong. The purge completes a deletion; it never grants
	// one, and it cannot expose data.
	var request ActionRequest
	if err = json.Unmarshal(op.Intent, &request); err != nil {
		return a.finishPurge(work, root, actor, op, Failed, err)
	}
	// Up to three passes: a folder that gained a child while this pass was running cannot
	// be removed before that child, and the child was written into a folder that had
	// already stopped existing for every read. The bound exists so a folder that keeps
	// growing cannot hold this attempt open forever.
	//
	// The counters resume from the journal rather than from zero: a retry re-walks the
	// same prefix and has already deleted part of it, and reporting "0 of N" again after
	// a throttled attempt is indistinguishable from an operation that never moved.
	deleted, adopted := purge.Deleted, purge.Adopted
	for pass := 0; pass < purgePasses; pass++ {
		entries, err := snapshotActionTree(work, native, purge.Path, request.Recursive)
		if err != nil {
			if storage.IsCode(err, "not_found") {
				// The prefix is gone: an earlier attempt finished it, or an outside tool did.
				return a.finishPurge(work, root, actor, op, Succeeded, nil)
			}
			return a.retryPurge(work, op, err)
		}
		// Publish the denominator before the first deletion of the pass, not after the
		// last one: the walk has just counted the objects it is about to settle, and a
		// total that only becomes known once the work is done can never be used to render
		// it. From here on `completed/total` is a real fraction for the whole pass.
		if err = a.savePurgeProgress(work, op, deleted, adopted, deleted+adopted+int64(len(entries))); err != nil {
			return a.retryPurge(work, op, err)
		}
		waited, err := a.purgeLevels(work, root, op, native, entries, &deleted, &adopted)
		if err != nil {
			if saveErr := a.savePurgeProgress(work, op, deleted, adopted, op.Purge.Total); saveErr != nil {
				slog.Warn("file purge progress not saved", "operation", op.ID, "error", saveErr)
			}
			return a.retryPurge(work, op, err)
		}
		if waited == 0 {
			break
		}
	}
	// Folders can survive all three passes when what is under them belongs to a newer
	// intent — a path the user re-created, or a write that arrived after the acceptance —
	// and those objects are never this purge's to delete. Re-walking cannot help, so the
	// purge ends here either way; what it may not touch is reported as adopted, not as
	// work still owed.
	return a.finishPurge(work, root, actor, op, Succeeded, nil)
}

// purgeLevels deletes one walk's entries deepest-first, several at a time. Depth decides
// the order and never the parallelism: a folder's marker may only be removed after its
// children, and every entry at one depth is independent of the others — which is what
// makes a purge of a thousand files a batch of requests instead of a queue of them.
func (a *Actions) purgeLevels(ctx context.Context, root Root, op Operation, native NativeActions, entries []storage.Entry, deleted, adopted *int64) (int, error) {
	byDepth := map[int][]storage.Entry{}
	deepest := 0
	for _, entry := range entries {
		depth := pathDepth(entry.Path)
		byDepth[depth] = append(byDepth[depth], entry)
		if depth > deepest {
			deepest = depth
		}
	}
	// A folder's marker may only go once nothing under it survived this pass: the walk saw
	// every child, so "did any child stay" is bookkeeping, not another request. It is also
	// what keeps an adopted child from being orphaned under a marker that disappeared.
	survivors := map[storage.Path]int{}
	waited := 0
	for depth := deepest; depth >= 0; depth-- {
		group := byDepth[depth]
		for start := 0; start < len(group); start += purgePageSize {
			page := group[start:min(start+purgePageSize, len(group))]
			live, err := a.options.LivePaths(ctx, root, pagePaths(page))
			if err != nil {
				return waited, err
			}
			if len(live) != len(page) {
				// The answer must cover the page it was asked about; a short answer would
				// silently mean "nothing is live" and delete a re-created path.
				return waited, E("invalid_backend_response", nil)
			}
			candidates := make([]storage.Entry, 0, len(page))
			for i, entry := range page {
				if live[i] {
					// A user re-created this exact path after accepting the deletion. It is
					// their new content, not a leftover, and deleting it would be the one
					// failure a delete must never produce.
					*adopted++
					continue
				}
				if !entry.ModifiedAt.IsZero() && entry.ModifiedAt.After(op.Purge.AcceptedAt) {
					// Written after the user's decision: an in-flight publish or an outside
					// writer. Neither was ever visible under the deleted path.
					*adopted++
					continue
				}
				candidates = append(candidates, entry)
			}
			results := a.purgeBatch(ctx, op, native, candidates)
			// Count the whole page even when one object refuses: the batch already deleted
			// the others, and a counter that skips them makes the retry look like it lost
			// ground it never lost.
			var refused error
			for i, result := range results {
				entry := candidates[i]
				if result.err != nil {
					if refused == nil {
						refused = result.err
					}
					continue
				}
				if entry.Kind == storage.Folder && result.deleted && survivors[entry.Path] > 0 {
					// Children of this folder were left behind, so the folder stays with them
					// and the next pass tries again once they are gone.
					survivors[parentPath(entry.Path)]++
					waited++
					*adopted++
					continue
				}
				switch {
				case result.notReady:
					survivors[parentPath(entry.Path)]++
					waited++
				case result.deleted:
					*deleted++
				case result.adopted:
					survivors[parentPath(entry.Path)]++
					*adopted++
				}
			}
			if refused != nil {
				return waited, refused
			}
			// One durable step per page, so the counters move while a large tree is being
			// deleted instead of jumping once when the whole walk ends. The cost is one
			// journal write per purgePageSize provider deletions — an owner-head lock held
			// for the write, not for the deletes.
			if err = a.savePurgeProgress(ctx, op, *deleted, *adopted, op.Purge.Total); err != nil {
				return waited, err
			}
		}
	}
	return waited, nil
}

// purgeBatch deletes these entries with a bounded number in flight. The first failure stops
// new work and is returned: the pass re-runs from a walk, so finishing the batch would only
// do work the retry repeats.
func (a *Actions) purgeBatch(ctx context.Context, op Operation, native NativeActions, entries []storage.Entry) []purgeResult {
	results := make([]purgeResult, len(entries))
	if len(entries) == 0 {
		return results
	}
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	slots := make(chan struct{}, purgeConcurrency)
	var wg sync.WaitGroup
	var failed atomic.Bool
	for i, entry := range entries {
		if failed.Load() {
			break
		}
		slots <- struct{}{}
		wg.Add(1)
		go func(i int, entry storage.Entry) {
			defer wg.Done()
			defer func() { <-slots }()
			if work.Err() != nil {
				return
			}
			results[i] = a.purgeEntry(work, op, entry, native)
			if results[i].err != nil && work.Err() == nil {
				failed.Store(true)
			}
		}(i, entry)
	}
	wg.Wait()
	return results
}

// purgeResult is what one object's deletion decided: it went, it was adopted, it has to
// wait for a child, or it failed and the pass retries.
type purgeResult struct {
	deleted, adopted, notReady bool
	err                        error
}

// errFolderNotReady is not a failure: it means the folder acquired a child after this
// pass observed it, so its marker has to wait for the pass that removes that child.
var errFolderNotReady = errors.New("folder gained a child during the purge")

// purgeEntry removes one native object. "Already gone" is success: the walk cannot
// distinguish an object an earlier attempt deleted from one an outside tool removed,
// and both mean the bytes the user wanted gone are gone.
func (a *Actions) purgeEntry(ctx context.Context, op Operation, entry storage.Entry, native NativeActions) purgeResult {
	revision := entry.Revision
	if len(revision) == 0 {
		// A folder that exists only as a common prefix has no revision of its own, and a
		// provider that cannot name a child from a listing leaves us one observation short.
		// Stat answers both without deciding anything.
		current, statErr := observe(ctx, native, entry.Path)
		if storage.IsCode(statErr, "not_found") {
			return purgeResult{deleted: true}
		}
		if statErr != nil {
			return purgeResult{err: statErr}
		}
		if current.Kind != entry.Kind {
			// The path changed kind under us: the object the user deleted is already gone.
			return purgeResult{deleted: true}
		}
		revision = current.Revision
		if len(revision) == 0 {
			// Still no revision after an observation: this is a marker-less prefix folder.
			// Its children are the native objects and this pass removes them, so there is
			// nothing native to delete here. Issuing the unconditional delete this branch
			// would otherwise reach could remove a marker that appeared after capture.
			return purgeResult{deleted: true}
		}
	}
	// One conditional request where the driver can do it, the journalled removal otherwise:
	// both refuse to delete an object whose revision moved, and both are safe to repeat.
	// A driver that journals (Local) always lands on the removal beside this branch; the
	// intent is named by the accepted operation below, never by the path alone, because a
	// path can be deleted, re-created and deleted again as two different operations.
	if purger, ok := native.(storage.Purger); ok {
		err := purger.Purge(ctx, entry.Path, entry.Kind, revision)
		switch {
		case err == nil:
			return purgeResult{deleted: true}
		case storage.IsCode(err, "not_found"):
			return purgeResult{deleted: true}
		case storage.IsCode(err, "changed"), storage.IsCode(err, "precondition_failed"):
			// Somebody replaced the object between our observation and the delete. Treat it
			// as their newer intent and leave it: a delete fails quiet, never destructive.
			return purgeResult{adopted: true}
		case storage.IsCode(err, "not_empty"), storage.IsCode(err, "directory_not_empty"):
			return purgeResult{notReady: true}
		}
		return purgeResult{err: err}
	}
	_, err := native.Remove(ctx, storage.Mutation{
		ID:         purgeEntryID(op.ID, entry.Path),
		Path:       entry.Path,
		Conditions: storage.Conditions{ExpectedRevision: revision},
	}, func(context.Context, []byte) error { return nil })
	switch {
	case err == nil, storage.IsCode(err, "not_found"):
		return purgeResult{deleted: true}
	case storage.IsCode(err, "changed"), storage.IsCode(err, "precondition_failed"):
		return purgeResult{adopted: true}
	case storage.IsCode(err, "not_empty"), storage.IsCode(err, "directory_not_empty"):
		return purgeResult{notReady: true}
	}
	return purgeResult{err: err}
}

func (a *Actions) savePurgeProgress(ctx context.Context, op Operation, deleted, adopted, total int64) error {
	if op.Purge == nil {
		return nil
	}
	if deleted == op.Purge.Deleted && adopted == op.Purge.Adopted && total == op.Purge.Total {
		return nil
	}
	updated, err := a.journal.SavePurgeProgress(ctx, op.Lease, deleted, adopted, total)
	if err != nil {
		return err
	}
	*op.Purge = *updated.Purge
	return nil
}

// retryPurge leaves the operation running and lets the worker try again. A provider that
// refuses a delete is a cleanup backlog, not a failed deletion: the index already told the
// user the subtree is gone, and the retry costs nothing but the walk.
func (a *Actions) retryPurge(ctx context.Context, op Operation, cause error) (Operation, error) {
	persist, cancel := durableContext(ctx)
	defer cancel()
	releaseLease(persist, a.journal, op.Lease)
	return op, cause
}

// finishPurge settles the operation. A purge that cannot finish stays non-terminal and
// is retried: the index already recorded the deletion, so a provider outage is a
// cleanup backlog, never a failed deletion.
func (a *Actions) finishPurge(ctx context.Context, root Root, actor Actor, op Operation, state State, cause error) (Operation, error) {
	persist, cancel := durableContext(ctx)
	defer cancel()
	result, _ := json.Marshal(map[string]any{"deleted": state == Succeeded, "purged": op.Purge.Deleted, "adopted": op.Purge.Adopted})
	var failure json.RawMessage
	if state != Succeeded {
		failure = problem("purge_failed", cause, &op)
	}
	return a.journal.FinishPurge(persist, op.Lease, state, result, failure)
}

// purgeEntryID names one object's removal inside one accepted deletion. The operation ID
// comes first because the path alone is not an intent: deleting a re-created path is a
// second accepted operation and must get a new identity, or the journaled driver refuses
// it as a conflict and leaves the bytes behind. The path stays in the name because one
// purge deletes many objects, and each removal is its own intent; the same operation
// retried over the same object reuses this exact id, which is what makes the retry
// idempotent instead of a second intent.
func purgeEntryID(operationID string, path storage.Path) string {
	return operationID + "/purge/" + string(path)
}

func pagePaths(entries []storage.Entry) []storage.Path {
	paths := make([]storage.Path, len(entries))
	for i, entry := range entries {
		paths[i] = entry.Path
	}
	return paths
}

func pathDepth(p storage.Path) int {
	depth := 0
	for _, r := range string(p) {
		if r == '/' {
			depth++
		}
	}
	return depth
}

// purgePageSize bounds one batch of index lookups and one saved progress update. It is
// the work unit, not a deadline: the purge of a large tree is many pages, each of which
// can be retried on its own.
const purgePageSize = 200

// purgeConcurrency is how many independent deletions one pass keeps in flight. Deletes are
// network-bound and per-object, so this is the whole difference between a purge measured in
// minutes and one measured in seconds; it stays modest because a burst of deletes is also
// what a provider throttles.
const purgeConcurrency = 8

// purgePasses bounds how often the walk restarts for folders that gained children while
// a previous pass was deleting their siblings.
const purgePasses = 3

var _ = time.Time{}
