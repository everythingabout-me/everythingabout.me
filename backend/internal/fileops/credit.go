package fileops

import (
	"context"

	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

// A row in file_credit_claims records that one native revision's size credit was
// spent. Two operations spending the same revision's credit would subtract the same
// bytes twice — a replacement of 100 B by 200 B credits 100 B, and a concurrent
// delete of those same 100 B refunds 100 B — so the primary key (owner, path,
// revision) is the exclusion. How long the row must live depends on what the credit
// is, and the two kinds differ:
//
//   - A replacement (upload_commit with a credit) is a fence. The credit it spends
//     is the revision it overwrote, and it can spend that credit only while it can
//     still run. Settlement is the end of that window: after it commits the
//     operation is terminal, its delta is in the budget, and it can never touch the
//     revision again, so Settle releases the row. The release is required for
//     correctness, not tidiness: identical bytes produce the identical native
//     revision, so re-importing an unchanged file replaces revision R with R and the
//     next replacement must be able to claim R again (it credits bytes that are
//     still there, so it is not a second refund). It is also what makes a settled
//     replacement prunable (see prune.go).
//
//   - A deletion (a delete action, or the removal half of a move) is a receipt. It
//     records that revision R was removed, and that refund must never be earned
//     twice, so it stays after settlement — and its operation stays with it, because
//     prune refuses to delete an operation a claim points at.
//
// The fence is per (path, revision) and not per size: an identical-size replacement
// of different bytes credits the same 100 B and must not run beside the first.
//
// Rows written before the release rule are replacement claims whose operation has
// already settled. They fence nothing — no operation can spend their credit again —
// but the revision they name may still be current, so the next operation adopts the
// row instead of failing (claimCredit below). Adoption is limited to a holder that is
// a succeeded upload_commit: a deletion receipt is never adopted, and an outcome that
// is not proven (failed, cancelled, unknown) stays a conflict instead of a guess.
//
// Either kind describes a revision that is *gone* from its path, and that is why "is
// this revision present?" cannot be the release test: two concurrent deletions of one
// present revision both see it present, which is the race the claim exists to exclude.
// The opposite event is the one that must release: when an operation settles after
// putting that same revision back at that path — a copy entry of a move or copy, a
// published upload, a native tree move's target — the removal the row records has been
// undone. Those bytes were counted into the budget again, so removing them again is a
// new removal and has to be refundable. Leaving the row would refuse every later
// delete, move or replacement of those bytes with no operator command to clear it, and
// coming back to a path is ordinary: unchanged content carries the identical native
// revision, so a folder moved out and imported back names the same (path, revision)
// pairs it left. releaseCredit / releaseCreditSubtree, called from the settlement
// transaction that established the presence, are that release.
//
// claimCredit takes the credit for (path, version) for one operation, adopting a
// predecessor's row when that predecessor is a settled replacement.
//
// The caller must already hold the owner lock and have written the operation row
// (the adopted row's foreign key points at it), and must pass the path and revision
// that operation's own reservation names. Adopting is the same statement as the
// settlement release: a settled replacement no longer spends its revision's credit,
// so the row is residue, and reusing it grants exactly the exclusion a fresh insert
// would have granted. It cannot double-credit: the credit is bounded by the
// operation's own reservation and the old revision has to still be the target when
// the native effect runs.
func claimCredit(ctx context.Context, tx pgx.Tx, owner string, root Root, path storage.Path, version []byte, operationID string) error {
	comparison := comparePath(root, path)
	tag, err := tx.Exec(ctx, `insert into file_credit_claims(owner_id,comparison_path,version,operation_id) values($1,$2,$3,$4) on conflict do nothing`, owner, comparison, version, operationID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// The key is taken. The insert conflicted on the exact stored spelling, so the
	// holder row this update sees carries the canonical comparison_path.
	tag, err = tx.Exec(ctx, `update file_credit_claims cc set operation_id=$4
		from file_operations holder
		where cc.owner_id=$1 and cc.comparison_path=$2 and cc.version=$3
		  and holder.owner_id=cc.owner_id and holder.id=cc.operation_id
		  and holder.action='upload_commit' and holder.state='succeeded'`, owner, comparison, version, operationID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return E("credit_conflict", nil)
	}
	return nil
}

// releaseCredit drops the claim on one (path, revision) pair after an operation
// settled having established that revision at that path. The caller must already hold
// the owner lock and run inside the settlement transaction, so no concurrent operation
// can be claiming the row this statement removes: anything admitted after the commit
// observes the new presence and its removal credits the new removal.
//
// The statement is unconditional on the holder because the presence being written is
// the reason the row is residue: a live holder of this key could only be a replacement
// fence for a revision that this write is replacing (it would have had to be admitted
// against the same current revision, which the path guard excludes) or a deletion whose
// bytes had not been removed yet (and the destination would then not have been free).
func releaseCredit(ctx context.Context, tx pgx.Tx, owner string, root Root, path storage.Path, version []byte) error {
	if len(version) == 0 {
		// A driver that reports no revision cannot hold a claim either: claimCredit is
		// always called with the revision the reservation captured, and a receipt is
		// keyed by it.
		return nil
	}
	_, err := tx.Exec(ctx, `delete from file_credit_claims where owner_id=$1 and comparison_path=$2 and version=$3`, owner, comparePath(root, path), version)
	return err
}

// releaseCreditSubtree drops every claim under a folder that a native tree move just
// put in place. The move has no per-file manifest and no revisions of its own, so it
// cannot name the (path, revision) pairs it re-established.
//
// Clearing the whole prefix is still exact enough: a claim under the target can only
// describe bytes that were removed from that path before, and the subtree now occupies
// it. A row whose revision is not in the moved-in content is dead weight rather than a
// protection — removing that revision from the path again is possible only after some
// later write put it there, and that write counts its bytes. The target's subtree guard
// covers the same paths for the whole operation, so no concurrent removal can be holding
// one of these rows while the move runs.
func releaseCreditSubtree(ctx context.Context, tx pgx.Tx, owner string, root Root, path storage.Path) error {
	comparison := comparePath(root, path)
	_, err := tx.Exec(ctx, `delete from file_credit_claims where owner_id=$1 and (comparison_path=$2 or starts_with(comparison_path,$3))`, owner, comparison, comparison+"/")
	return err
}
