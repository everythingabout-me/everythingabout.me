package filechanges

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	fc "everythingabout.me/backend/internal/filecontract"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// EnsureHead allocates a stream epoch once; it does not advance the head. The
// caller owns commit/rollback. Establish index/quota locks before calling Append.
func EnsureHead(ctx context.Context, tx pgx.Tx, owner string) (Head, error) {
	if tx == nil || !validUUID(owner) {
		return Head{}, ErrInvalidChange
	}
	if _, err := tx.Exec(ctx, `insert into file_change_heads(owner_id) values($1::uuid) on conflict(owner_id) do nothing`, owner); err != nil {
		return Head{}, fmt.Errorf("ensure file change head: %w", err)
	}
	return ReadHead(ctx, tx, owner)
}

func ReadHead(ctx context.Context, q Queryer, owner string) (Head, error) {
	var head Head
	if !validUUID(owner) {
		return head, ErrInvalidChange
	}
	err := q.QueryRow(ctx, `select owner_id::text,root_epoch::text,head_position,retained_after,retained_since,root_namespace
 from file_change_heads where owner_id=$1::uuid`, owner).Scan(&head.OwnerID, &head.RootEpoch, &head.Position, &head.RetainedAfter, &head.RetainedSince, &head.RootNamespace)
	head.RetainedSince = head.RetainedSince.UTC()
	return head, err
}

// Append records one confirmed effect in the SAME transaction as index/result
// settlement. Lock order is index/quota -> owner head -> effect/audit records.
// Never acquire more index locks or perform provider/network I/O after Append.
// The owner head is held until commit: no reader can advance beyond an earlier
// uncommitted effect. A rollback also rolls back the allocation, avoiding gaps.
//
// Replays use the immutable effect key. ObservedAt and optional proposed event ID
// never replace the first accepted record. Different semantic payloads conflict.
func Append(ctx context.Context, tx pgx.Tx, change Change) (int64, error) {
	change, err := normalizeChange(change)
	if err != nil || tx == nil {
		return 0, ErrInvalidChange
	}
	fingerprint, err := changeFingerprint(change)
	if err != nil {
		return 0, err
	}
	lookup := func() (int64, bool, error) {
		var position int64
		var id, saved string
		err := tx.QueryRow(ctx, `select event_id::text,position,fingerprint from file_change_effects
 where owner_id=$1::uuid and effect_key=$2`, change.OwnerID, change.EffectKey).Scan(&id, &position, &saved)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, err
		}
		if saved != fingerprint || (change.ID != "" && change.ID != id) {
			return 0, true, ErrEffectConflict
		}
		return position, true, nil
	}
	savedPosition, savedEffect, err := lookup()
	if err != nil {
		return 0, err
	}
	if _, err := EnsureHead(ctx, tx, change.OwnerID); err != nil {
		return 0, err
	}
	var head int64
	if err := tx.QueryRow(ctx, `select head_position from file_change_heads where owner_id=$1::uuid for update`, change.OwnerID).Scan(&head); err != nil {
		return 0, err
	}
	// Replays retain the same lock order before callers settle markers/audit.
	if savedEffect {
		return savedPosition, nil
	}
	// A concurrent writer may have committed this effect while we waited.
	if position, found, err := lookup(); found || err != nil {
		return position, err
	}
	position := head + 1
	if position <= 0 {
		return 0, ErrInvalidChange
	}
	var id string
	if err := tx.QueryRow(ctx, `insert into file_change_effects(owner_id,effect_key,event_id,position,fingerprint)
 values($1::uuid,$2,coalesce(nullif($3,'')::uuid,gen_random_uuid()),$4,$5) returning event_id::text`,
		change.OwnerID, change.EffectKey, change.ID, position, fingerprint).Scan(&id); err != nil {
		return 0, fmt.Errorf("record file change receipt: %w", err)
	}
	before, err := metadataJSON(change.Before)
	if err != nil {
		return 0, err
	}
	after, err := metadataJSON(change.After)
	if err != nil {
		return 0, err
	}
	ancestorsBefore, _ := json.Marshal(change.BeforeAncestors)
	ancestorsAfter, _ := json.Marshal(change.AfterAncestors)
	impacts, _ := json.Marshal(change.ImpactPaths)
	folders, _ := json.Marshal(change.AffectedFolders)
	actor, _ := json.Marshal(change.Actor)
	var observed any
	if !change.ObservedAt.IsZero() {
		observed = change.ObservedAt.UTC()
	}
	_, err = tx.Exec(ctx, `insert into file_changes(owner_id,position,id,effect_key,kind,coverage,entry_id,
 before_snapshot,after_snapshot,before_ancestors,after_ancestors,impact_paths,affected_folders,actor,operation_id,request_id,origin,observed_at)
 values($1::uuid,$2,$3::uuid,$4,$5,$6,$7::uuid,$8::jsonb,$9::jsonb,$10::jsonb,$11::jsonb,$12::jsonb,$13::jsonb,$14::jsonb,$15,$16,$17,coalesce($18::timestamptz,clock_timestamp()))`,
		change.OwnerID, position, id, change.EffectKey, change.Kind, change.Coverage, change.EntryID,
		before, after, ancestorsBefore, ancestorsAfter, impacts, folders, actor, change.OperationID, change.RequestID, change.Origin, observed)
	if err != nil {
		return 0, fmt.Errorf("append file change: %w", err)
	}
	if _, err := tx.Exec(ctx, `update file_change_heads set head_position=$2,updated_at=clock_timestamp() where owner_id=$1::uuid`, change.OwnerID, position); err != nil {
		return 0, err
	}
	return position, nil
}

func validUUID(value string) bool {
	var id pgtype.UUID
	return len(value) == 36 && id.Scan(value) == nil && id.Valid && value != "00000000-0000-0000-0000-000000000000"
}

func normalizeChange(c Change) (Change, error) {
	if !validUUID(c.OwnerID) || c.EffectKey == "" || len(c.EffectKey) > 1024 || (c.ID != "" && !validUUID(c.ID)) {
		return c, ErrInvalidChange
	}
	if c.Coverage == "" {
		c.Coverage = "entry"
	}
	if c.Coverage != "entry" && c.Coverage != "subtree" {
		return c, ErrInvalidChange
	}
	if c.Origin == "" {
		c.Origin = "unknown"
	}
	if c.Actor.Kind == "" {
		c.Actor.Kind = "unknown"
	}
	for _, snapshot := range []*fc.Metadata{c.Before, c.After} {
		if snapshot == nil {
			continue
		}
		if !validUUID(snapshot.ID) || fc.ValidatePath(string(snapshot.Path)) != nil || (snapshot.Kind != "file" && snapshot.Kind != "folder") ||
			(snapshot.ParentID != nil && !validUUID(*snapshot.ParentID)) || (snapshot.ParentPath != "" && fc.ValidatePath(string(snapshot.ParentPath)) != nil) {
			return c, ErrInvalidChange
		}
		if c.EntryID == nil {
			id := snapshot.ID
			c.EntryID = &id
		} else if *c.EntryID != snapshot.ID {
			return c, ErrInvalidChange
		}
	}
	if c.EntryID != nil && !validUUID(*c.EntryID) {
		return c, ErrInvalidChange
	}
	switch c.Kind {
	case "created":
		if c.Before != nil || c.After == nil {
			return c, ErrInvalidChange
		}
	case "deleted":
		if c.Before == nil || c.After != nil {
			return c, ErrInvalidChange
		}
	case "updated", "moved":
		if c.Before == nil || c.After == nil {
			return c, ErrInvalidChange
		}
		before, _ := json.Marshal(c.Before)
		after, _ := json.Marshal(c.After)
		if bytes.Equal(before, after) {
			return c, ErrInvalidChange
		}
	case "invalidated":
		if c.Before == nil && c.After == nil && len(c.ImpactPaths) == 0 {
			return c, ErrInvalidChange
		}
	default:
		return c, ErrInvalidChange
	}
	c.AffectedFolders = slices.Clone(c.AffectedFolders)
	if c.AffectedFolders == nil {
		c.AffectedFolders = []fc.Metadata{}
	}
	seenFolders := make(map[string]bool, len(c.AffectedFolders))
	for _, folder := range c.AffectedFolders {
		if folder.Kind != "folder" || !validUUID(folder.ID) || fc.ValidatePath(string(folder.Path)) != nil ||
			(folder.ParentID != nil && !validUUID(*folder.ParentID)) || (folder.ParentPath != "" && fc.ValidatePath(string(folder.ParentPath)) != nil) || seenFolders[folder.ID] {
			return c, ErrInvalidChange
		}
		seenFolders[folder.ID] = true
	}
	slices.SortFunc(c.AffectedFolders, func(a, b fc.Metadata) int {
		if order := strings.Compare(string(a.Path), string(b.Path)); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	for _, ancestors := range [][]Ancestor{c.BeforeAncestors, c.AfterAncestors} {
		for _, ancestor := range ancestors {
			if !validUUID(ancestor.ID) || fc.ValidatePath(string(ancestor.Path)) != nil {
				return c, ErrInvalidChange
			}
		}
	}
	for _, path := range c.ImpactPaths {
		if fc.ValidatePath(string(path)) != nil {
			return c, ErrInvalidChange
		}
	}
	if c.BeforeAncestors == nil {
		c.BeforeAncestors = []Ancestor{}
	}
	if c.AfterAncestors == nil {
		c.AfterAncestors = []Ancestor{}
	}
	c.ImpactPaths = slices.Clone(c.ImpactPaths)
	if c.ImpactPaths == nil {
		c.ImpactPaths = []fc.Path{}
	}
	slices.Sort(c.ImpactPaths)
	c.ImpactPaths = slices.Compact(c.ImpactPaths)
	if err := validateActor(c.Actor); err != nil {
		return c, err
	}
	return c, nil
}

func validateActor(actor Actor) error {
	if strings.TrimSpace(actor.Kind) == "" || len(actor.Kind) > 64 || (actor.Scope != "" && fc.ValidatePath(string(actor.Scope)) != nil) {
		return ErrInvalidChange
	}
	return nil
}

func changeFingerprint(c Change) (string, error) {
	values := []any{c.Kind, c.Coverage, c.EntryID, c.Before, c.After, c.BeforeAncestors, c.AfterAncestors,
		c.ImpactPaths, c.Actor, c.OperationID, c.RequestID, c.Origin}
	// Empty patches retain fingerprints issued before this additive field existed.
	if len(c.AffectedFolders) != 0 {
		values = append(values, c.AffectedFolders)
	}
	return fingerprint(values)
}

func fingerprint(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func metadataJSON(metadata *fc.Metadata) (any, error) {
	if metadata == nil {
		return nil, nil
	}
	data, err := json.Marshal(metadata)
	return json.RawMessage(data), err
}
