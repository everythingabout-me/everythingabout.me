package filechanges

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"github.com/jackc/pgx/v5"
)

// RecordAudit is transactional and independent of Append: a denied read is an
// audit fact, never a successful change. Call it after Append when one transaction
// records both. The typed snapshot has no fields for file bodies or credentials.
// A worker effect key deduplicates retries; ordinary requests may omit the key.
func RecordAudit(ctx context.Context, tx pgx.Tx, audit *Audit) error {
	if tx == nil || audit == nil {
		return ErrInvalidAudit
	}
	a := *audit
	if !validUUID(a.OwnerID) || (a.ID != "" && !validUUID(a.ID)) || a.Action == "" || a.Outcome == "" || len(a.EffectKey) > 1024 || a.Status < 0 || a.Status > 599 ||
		(a.BytesWritten != nil && *a.BytesWritten < 0) || (a.EntryID != nil && !validUUID(*a.EntryID)) || (a.Path != nil && fc.ValidatePath(string(*a.Path)) != nil) {
		return ErrInvalidAudit
	}
	if a.Actor.Kind == "" {
		a.Actor.Kind = "unknown"
	}
	if a.Origin == "" {
		a.Origin = "unknown"
	}
	if validateActor(a.Actor) != nil {
		return ErrInvalidAudit
	}
	fingerprint, err := fingerprint([]any{a.Action, a.Outcome, a.Allowed, a.Status, a.BytesWritten, a.ReasonCode, a.EntryID, a.Path, a.Actor, a.OperationID, a.RequestID, a.Origin})
	if err != nil {
		return err
	}
	actor, err := json.Marshal(a.Actor)
	if err != nil {
		return err
	}
	var key, observed any
	if a.EffectKey != "" {
		key = a.EffectKey
	}
	if !a.ObservedAt.IsZero() {
		observed = a.ObservedAt.UTC()
	}
	err = tx.QueryRow(ctx, `insert into file_audit(id,owner_id,effect_key,action,outcome,allowed,status,bytes_written,reason_code,entry_id,path,
 actor,operation_id,request_id,origin,observed_at,fingerprint)
 values(coalesce(nullif($1,'')::uuid,gen_random_uuid()),$2::uuid,$3,$4,$5,$6,$7,$8,$9,$10::uuid,$11,$12::jsonb,$13,$14,$15,
 coalesce($16::timestamptz,clock_timestamp()),$17)
 on conflict(owner_id,effect_key) do update set effect_key=excluded.effect_key
 where file_audit.fingerprint=excluded.fingerprint and ($1='' or file_audit.id=nullif($1,'')::uuid)
 returning id::text,observed_at`, a.ID, a.OwnerID, key, a.Action, a.Outcome, a.Allowed, a.Status, a.BytesWritten, a.ReasonCode, a.EntryID, a.Path, actor,
		a.OperationID, a.RequestID, a.Origin, observed, fingerprint).Scan(&a.ID, &a.ObservedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrEffectConflict
	}
	if err != nil {
		return fmt.Errorf("record file audit: %w", err)
	}
	a.ObservedAt = a.ObservedAt.UTC()
	*audit = a
	return nil
}

// ReadAudit is an owner-management view, not an app feed. Callers must authorize
// owner audit access; a shared-owner app token does not imply that permission.
// It retains tombstones and actor snapshots independently from live credential or
// node rows. This bounded recent view does not promise replay cursor semantics.
func ReadAudit(ctx context.Context, q Queryer, owner string, limit int) ([]Audit, error) {
	return ReadAuditPage(ctx, q, owner, limit, time.Time{}, "")
}

// ReadAuditPage uses observed-time/UUID keysets, never an offset. These are
// internal boundaries; API cursors must bind the owner and management principal.
func ReadAuditPage(ctx context.Context, q Queryer, owner string, limit int, beforeObservedAt time.Time, beforeID string) ([]Audit, error) {
	if !validUUID(owner) || limit < 1 || limit > 1001 || beforeObservedAt.IsZero() != (beforeID == "") || (beforeID != "" && !validUUID(beforeID)) {
		return nil, ErrInvalidAudit
	}
	var before any
	if !beforeObservedAt.IsZero() {
		before = beforeObservedAt.UTC()
	}
	rows, err := q.Query(ctx, `select id::text,owner_id::text,coalesce(effect_key,''),action,outcome,allowed,status,bytes_written,reason_code,
 entry_id::text,path,actor,operation_id,request_id,origin,observed_at from file_audit
 where owner_id=$1::uuid and ($3::timestamptz is null or (observed_at,id)<($3::timestamptz,nullif($4,'')::uuid))
 order by observed_at desc,id desc limit $2`, owner, limit, before, beforeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Audit{}
	for rows.Next() {
		var a Audit
		var actor []byte
		if err := rows.Scan(&a.ID, &a.OwnerID, &a.EffectKey, &a.Action, &a.Outcome, &a.Allowed, &a.Status, &a.BytesWritten, &a.ReasonCode, &a.EntryID,
			&a.Path, &actor, &a.OperationID, &a.RequestID, &a.Origin, &a.ObservedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(actor, &a.Actor); err != nil {
			return nil, err
		}
		a.ObservedAt = a.ObservedAt.UTC()
		items = append(items, a)
	}
	return items, rows.Err()
}
