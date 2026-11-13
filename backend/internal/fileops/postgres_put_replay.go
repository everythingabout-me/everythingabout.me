package fileops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

// ReplayWrite compares the original public intent before observing a destination
// which may have changed or disappeared after commit. Native bindings are captured
// admission evidence, not a reason to create a second operation after a lost ACK.
func (p *Postgres) ReplayWrite(ctx context.Context, root Root, actor Actor, key string, intent storage.WriteIntent) (Operation, bool, error) {
	var op Operation
	found := false
	if key == "" {
		return op, false, nil
	}
	if err := validateRoot(root); err != nil {
		return op, false, err
	}
	err := p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `select id::text from file_operations where owner_id=$1 and principal_id=$2 and action='upload_commit' and idempotency_key=$3`, root.OwnerID, actor.ID, key).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		op, _, err = readOperation(ctx, tx, id)
		if err != nil {
			return err
		}
		if err = authorizeOperation(op, root, actor, true); err != nil {
			return err
		}
		var original storage.WriteIntent
		if json.Unmarshal(op.Intent, &original) != nil {
			return E("invalid_intent", nil)
		}
		original, intent = clientWriteIdentity(original), clientWriteIdentity(intent)
		equal := original.Target == intent.Target && original.Size == intent.Size && original.MediaType == intent.MediaType && original.ClientCondition == intent.ClientCondition && original.Selection == intent.Selection && original.EntryID == intent.EntryID && original.ParentID == intent.ParentID
		a, _ := json.Marshal(original.SourceModifiedAt)
		b, _ := json.Marshal(intent.SourceModifiedAt)
		equal = equal && bytes.Equal(a, b)
		a, _ = json.Marshal(original.Digest)
		b, _ = json.Marshal(intent.Digest)
		equal = equal && bytes.Equal(a, b)
		if !equal {
			return E("idempotency_conflict", nil)
		}
		found = true
		return nil
	})
	return op, found, err
}
