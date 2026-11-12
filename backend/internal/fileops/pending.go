package fileops

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Queryer is the read side a pending-delete guard needs. pgxpool.Pool,
// pgxpool.Conn and pgx.Tx all satisfy it, so the same query can run on the pool
// a worker uses, the dedicated connection a scan holds, or its serializable
// transaction, without the caller constructing a second handle.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// PendingDeleteRoots reports the prefixes of deletions this owner accepted whose
// native byte purge has not settled. It is the current source of truth for the
// question every reader used to ask the retired fs_jobs table ("is a permanent
// deletion still running over this path?").
//
// Only an accepted delete qualifies: AcceptPrefixDelete sets Purge in the same
// transaction that removes the index rows, so a non-terminal operation carrying a
// Purge path is exactly "the user's answer is final, the bytes are still leaving".
// A queued delete that has not been accepted yet is deliberately excluded — its
// rows are still live, the scan/sync callers hold the owner write-admission lock
// that also fences its acceptance, and concealment during admission is a read
// question the API overlay answers from its guards.
//
// A terminal delete is excluded even when it failed with bytes left behind: the
// operation journal stops retrying then, so nothing will make further progress and
// blocking reads forever would strand the owner. Recovery of such an operation is
// operator work (docs/design/deletes.md), not a read-time fence.
//
// namespace scopes the answer to one native root (Root.Namespace()). An empty
// namespace asks across every generation, which is what the legacy readers did;
// callers that know the current binding pass it so a purge in another generation
// cannot block the current tree.
//
// The partial index file_operations_pending covers these states, so the read scans
// only the work in flight, not the journal that retains every terminal row.
func PendingDeleteRoots(ctx context.Context, q Queryer, ownerID, namespace string) ([]string, error) {
	if q == nil || ownerID == "" {
		return nil, nil
	}
	rows, err := q.Query(ctx, `select distinct o.snapshot->'Purge'->>'Path'
		from file_operations o
		where o.owner_id=$1::uuid
		  and o.action='delete'
		  and coalesce(o.snapshot->'Purge'->>'Path','') <> ''
		  and o.snapshot->>'State' in ('queued','running','outcome_unknown')
		  and ($2='' or concat_ws(':', o.snapshot->'Root'->>'OwnerID', o.snapshot->'Root'->>'ConnectionID',
		      o.snapshot->'Root'->>'Generation', o.snapshot->'Root'->>'ContentRoot',
		      o.snapshot->'Root'->>'SystemRoot')=$2)
		order by 1`, ownerID, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roots []string
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, err
		}
		roots = append(roots, root)
	}
	return roots, rows.Err()
}
