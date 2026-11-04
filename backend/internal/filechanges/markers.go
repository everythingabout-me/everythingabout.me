package filechanges

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// MarkChanged updates the changed entry and captured old/new ancestors in the
// caller's settlement transaction, after Append. It never traverses live nodes,
// invents extra change events, or regresses either ordering or service time.
func MarkChanged(ctx context.Context, tx pgx.Tx, owner string, entryIDs []string, position int64, at time.Time) error {
	return markChanged(ctx, tx, owner, entryIDs, position, at, "")
}

// MarkChangedWithToken uses the settlement's preallocated event UUID so saved
// feed metadata and recursive equality markers agree in the same transaction.
func MarkChangedWithToken(ctx context.Context, tx pgx.Tx, owner string, entryIDs []string, position int64, at time.Time, token string) error {
	if !validUUID(token) {
		return ErrInvalidChange
	}
	return markChanged(ctx, tx, owner, entryIDs, position, at, token)
}

func markChanged(ctx context.Context, tx pgx.Tx, owner string, entryIDs []string, position int64, at time.Time, token string) error {
	if tx == nil || !validUUID(owner) || position <= 0 {
		return ErrInvalidChange
	}
	ids := slices.Clone(entryIDs)
	for _, id := range ids {
		if !validUUID(id) {
			return ErrInvalidChange
		}
	}
	if len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	var head int64
	if err := tx.QueryRow(ctx, `select head_position from file_change_heads where owner_id=$1::uuid for update`, owner).Scan(&head); err != nil {
		return err
	}
	if head < position {
		return ErrInvalidChange
	}
	var observed any
	if !at.IsZero() {
		observed = at.UTC()
	}
	_, err := tx.Exec(ctx, `insert into file_change_markers(owner_id,entry_id,position,last_changed_at,change_token)
 select $1::uuid,id,$3,coalesce($4::timestamptz,clock_timestamp()),coalesce(nullif($5,'')::uuid,gen_random_uuid()) from unnest($2::uuid[]) id order by id
 on conflict(owner_id,entry_id) do update set position=greatest(file_change_markers.position,excluded.position),
 last_changed_at=greatest(file_change_markers.last_changed_at,excluded.last_changed_at),change_token=excluded.change_token
 where excluded.position>file_change_markers.position`, owner, ids, position, observed, token)
	return err
}

// ReadMarker is an internal owner-isolated lookup. APIs must require full visible
// subtree access before exposing recursive markers and must encode Position as an
// opaque equality token rather than return the private stream position.
func ReadMarker(ctx context.Context, q Queryer, owner, entryID string) (Marker, error) {
	var marker Marker
	if !validUUID(owner) || !validUUID(entryID) {
		return marker, ErrInvalidChange
	}
	err := q.QueryRow(ctx, `select owner_id::text,entry_id::text,position,change_token::text,last_changed_at from file_change_markers
 where owner_id=$1::uuid and entry_id=$2::uuid`, owner, entryID).Scan(&marker.OwnerID, &marker.EntryID, &marker.Position, &marker.Token, &marker.LastChangedAt)
	marker.LastChangedAt = marker.LastChangedAt.UTC()
	return marker, err
}
