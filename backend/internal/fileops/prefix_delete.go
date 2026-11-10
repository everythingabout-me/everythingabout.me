package fileops

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
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
		return op, err
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
	purge := op.Purge
	if purge == nil || purge.Path == "" {
		return op, E("invalid_intent", nil)
	}
	op, err := a.journal.Claim(ctx, root, actor, op.ID, a.options.LeaseTTL)
	if err != nil {
		return op, err
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
	// growing cannot hold the operation open forever.
	var deleted, adopted int64
	for pass := 0; pass < purgePasses; pass++ {
		entries, err := snapshotActionTree(work, native, purge.Path, request.Recursive)
		if err != nil {
			if storage.IsCode(err, "not_found") {
				// The prefix is gone: an earlier attempt finished it, or an outside tool did.
				return a.finishPurge(work, root, actor, op, Succeeded, nil)
			}
			return a.finishPurge(work, root, actor, op, Failed, err)
		}
		// Deepest first: a parent removed before its children would leave them unreachable.
		order := append([]storage.Entry(nil), entries...)
		sort.SliceStable(order, func(i, j int) bool {
			return pathDepth(order[i].Path) > pathDepth(order[j].Path)
		})
		waited := 0
		for start := 0; start < len(order); start += purgePageSize {
			end := min(start+purgePageSize, len(order))
			page := order[start:end]
			live, err := a.options.LivePaths(work, root, pagePaths(page))
			if err != nil {
				return a.finishPurge(work, root, actor, op, Failed, err)
			}
			if len(live) != len(page) {
				// The answer must cover the page it was asked about; a short answer would
				// silently mean "nothing is live" and delete a re-created path.
				return a.finishPurge(work, root, actor, op, Failed, E("invalid_backend_response", nil))
			}
			for i, entry := range page {
				if live[i] {
					// A user re-created this exact path after accepting the deletion. It is
					// their new content, not a leftover, and deleting it would be the one
					// failure a delete must never produce.
					adopted++
					continue
				}
				if !entry.ModifiedAt.IsZero() && entry.ModifiedAt.After(purge.AcceptedAt) {
					// Written after the user's decision: an in-flight publish or an outside
					// writer. Neither was ever visible under the deleted path.
					adopted++
					continue
				}
				done, countAdopted, err := a.purgeEntry(work, op, purge, native, entry)
				if errors.Is(err, errFolderNotReady) {
					waited++
					continue
				}
				if err != nil {
					if saveErr := a.savePurgeProgress(work, op, deleted, adopted); saveErr != nil {
						slog.Warn("file purge progress not saved", "operation", op.ID, "error", saveErr)
					}
					return a.finishPurge(work, root, actor, op, Failed, err)
				}
				if done {
					deleted++
				}
				if countAdopted {
					adopted++
				}
			}
			if err = a.savePurgeProgress(work, op, deleted, adopted); err != nil {
				return a.finishPurge(work, root, actor, op, Failed, err)
			}
		}
		if waited == 0 {
			break
		}
	}
	return a.finishPurge(work, root, actor, op, Succeeded, nil)
}

// errFolderNotReady is not a failure: it means the folder acquired a child after this
// pass observed it, so its marker has to wait for the pass that removes that child.
var errFolderNotReady = errors.New("folder gained a child during the purge")

// purgeEntry removes one native object. "Already gone" is success: the walk cannot
// distinguish an object an earlier attempt deleted from one an outside tool removed,
// and both mean the bytes the user wanted gone are gone.
func (a *Actions) purgeEntry(ctx context.Context, op Operation, purge *PurgeState, native NativeActions, entry storage.Entry) (deleted, adopted bool, err error) {
	revision := entry.Revision
	if len(revision) == 0 {
		// A folder that exists only as a common prefix has no revision of its own, and a
		// provider that cannot name a child from a listing leaves us one observation
		// short. Stat answers both without deciding anything.
		current, statErr := observe(ctx, native, entry.Path)
		if storage.IsCode(statErr, "not_found") {
			return true, false, nil
		}
		if statErr != nil {
			return false, false, statErr
		}
		revision = current.Revision
		if current.Kind != entry.Kind {
			// The path changed kind under us: the object the user deleted is already gone.
			return true, false, nil
		}
	}
	_, err = native.Remove(ctx, storage.Mutation{
		ID:         purgeEntryID(op.ID, entry.Path),
		Path:       entry.Path,
		Conditions: storage.Conditions{ExpectedRevision: revision},
	}, func(context.Context, []byte) error { return nil })
	switch {
	case err == nil:
		return true, false, nil
	case storage.IsCode(err, "not_found"):
		return true, false, nil
	case storage.IsCode(err, "changed"), storage.IsCode(err, "precondition_failed"):
		// Somebody replaced the object between our observation and the delete. Treat it
		// as their newer intent and leave it: a delete fails quiet, never destructive.
		return false, true, nil
	case storage.IsCode(err, "not_empty"), storage.IsCode(err, "directory_not_empty"):
		return false, false, errFolderNotReady
	}
	var none *storage.NoFinalEffect
	if errors.As(err, &none) {
		return false, false, err
	}
	return false, false, err
}

func (a *Actions) savePurgeProgress(ctx context.Context, op Operation, deleted, adopted int64) error {
	if op.Purge == nil {
		return nil
	}
	if deleted == op.Purge.Deleted && adopted == op.Purge.Adopted {
		return nil
	}
	updated, err := a.journal.SavePurgeProgress(ctx, op.Lease, deleted, adopted)
	if err != nil {
		return err
	}
	*op.Purge = *updated.Purge
	return nil
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

// purgePasses bounds how often the walk restarts for folders that gained children while
// a previous pass was deleting their siblings.
const purgePasses = 3

var _ = time.Time{}
