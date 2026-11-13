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
// claimCredit takes the credit for (path, version) for one operation and refuses the
// request when another operation holds it. The caller must already hold the owner lock
// and have written its operation row, which is the row a claim points at.
func claimCredit(ctx context.Context, tx pgx.Tx, owner string, root Root, path storage.Path, version []byte, operationID string) error {
	comparison := comparePath(root, path)
	tag, err := tx.Exec(ctx, `insert into file_credit_claims(owner_id,comparison_path,version,operation_id) values($1,$2,$3,$4) on conflict do nothing`, owner, comparison, version, operationID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return E("credit_conflict", nil)
	}
	return nil
}
