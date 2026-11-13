package fileops

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

// ReplayAction compares original client intent before taking new observations.
// Captured manifests, current usage, and current credential scope are not intent.
func (p *Postgres) ReplayAction(ctx context.Context, req ActionRequest) (Operation, bool, error) {
	var op Operation
	found := false
	if req.IdempotencyKey == "" {
		return op, false, nil
	}
	err := p.transaction(ctx, req.Root.OwnerID, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `select id::text from file_operations where owner_id=$1 and principal_id=$2 and action=$3 and idempotency_key=$4`, req.Root.OwnerID, req.Actor.ID, req.Kind, req.IdempotencyKey).Scan(&id)
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
		if err = authorizeOperation(op, req.Root, req.Actor, true); err != nil {
			return err
		}
		original, err := actionRequest(op)
		if err != nil || !actionClientEqual(original, req) {
			return E("idempotency_conflict", err)
		}
		found = true
		return nil
	})
	return op, found, err
}
func readActionBinding(ctx context.Context, tx pgx.Tx, op Operation) (actionBinding, bool, error) {
	var b actionBinding
	var raw []byte
	err := tx.QueryRow(ctx, `select binding from file_action_bindings where operation_id=$1 and owner_id=$2`, op.ID, op.Root.OwnerID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, false, nil
	}
	if err != nil {
		return b, false, err
	}
	err = json.Unmarshal(raw, &b)
	return b, true, err
}
func (p *Postgres) ActionBinding(ctx context.Context, op Operation) (actionBinding, error) {
	var b actionBinding
	err := p.transaction(ctx, op.Root.OwnerID, func(tx pgx.Tx) error {
		var found bool
		var err error
		b, found, err = readActionBinding(ctx, tx, op)
		if err == nil && !found {
			return E("manifest_not_frozen", nil)
		}
		return err
	})
	return b, err
}

// FreezeAction is the only action path that can obtain negative source-size
// accounting authority. It requires an existing guard and captures exact native
// source versions; generic Begin remains unable to mint tree credit.
func (p *Postgres) FreezeAction(ctx context.Context, l Lease, b actionBinding, manifest []ManifestEntry, q ReservationRequest) error {
	return p.leased(ctx, l, func(tx pgx.Tx, op *Operation, _ time.Time) error {
		if op.Terminal() || op.Phase != Admitted || op.EntryCount > 0 || op.CancelRequested {
			return E("invalid_transition", nil)
		}
		if _, found, err := readActionBinding(ctx, tx, *op); err != nil {
			return err
		} else if found {
			return E("intent_conflict", nil)
		}
		if err := validateFrozenAction(*op, b, manifest, q); err != nil {
			return err
		}
		logical := q.NewBytes // moves reserve possible independent duplicate outputs
		u, err := budget(ctx, tx, op.Root)
		if err != nil {
			return err
		}
		if !budgetFits(u.LogicalBytes, u.LogicalReserved, logical, u.LogicalLimit) {
			return E("quota_exceeded", nil)
		}
		if !budgetFits(u.PhysicalBytes, u.PhysicalReserved, q.PhysicalBytes, u.PhysicalLimit) {
			return E("staging_full", nil)
		}
		for _, e := range manifest {
			if e.Action == "delete" && e.Size > 0 {
				// A deletion credit is a permanent receipt (credit.go). The row it inserts
				// may still be a settled replacement's, which adoption turns into this
				// operation's receipt when that replacement's revision is the captured source.
				if err := claimCredit(ctx, tx, op.Root.OwnerID, op.Root, e.Source, e.SourceRevision, op.ID); err != nil {
					return err
				}
			}
			op.Entries = append(op.Entries, EntryEffect{ManifestEntry: e, State: "pending"})
		}
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `insert into file_action_bindings(operation_id,owner_id,binding) values($1,$2,$3)`, op.ID, op.Root.OwnerID, raw)
		if err != nil {
			return err
		}
		op.Reservation = q
		raw, err = json.Marshal(q)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update file_reservations set request=$2,logical_reserved=$3,physical_reserved=$4 where operation_id=$1 and owner_id=$5 and not settled`, op.ID, raw, logical, q.PhysicalBytes, op.Root.OwnerID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update file_budgets set logical_reserved=logical_reserved+$2,physical_reserved=physical_reserved+$3 where owner_id=$1`, op.Root.OwnerID, logical, q.PhysicalBytes)
		if err != nil {
			return err
		}
		// Freeze is the one moment the manifest appears, so this is where the entry rows
		// and entry_count are written together with the frozen binding, in this
		// transaction and behind the parent row lock `leased` took. The document is
		// written by the same `leased` call after this function returns, without Entries
		// (documentOf), so the row's layout and its rows cannot disagree. The layout is
		// already 'table' (admission wrote it, and Claim upgrades a document-backed row
		// before returning), so this only records the count.
		if err = writeEntries(ctx, tx, *op, op.Entries); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update file_operations set entry_count=$3, entries_layout='table' where id=$1 and owner_id=$2`, op.ID, op.Root.OwnerID, len(op.Entries))
		return err
	})
}
func validateFrozenAction(op Operation, b actionBinding, entries []ManifestEntry, q ReservationRequest) error {
	req, err := actionRequest(op)
	if err != nil {
		return err
	}
	if q.CreditPath != "" || len(q.CreditVersion) > 0 || len(entries) > 2*maxActionEntries {
		return E("invalid_intent", nil)
	}
	sources := make(map[storage.Path]storage.Entry, len(b.Sources))
	for _, e := range b.Sources {
		if requireRevision(e) != nil || !within(comparePath(op.Root, e.Path), comparePath(op.Root, req.From)) || sources[e.Path].Kind != "" {
			return E("invalid_intent", nil)
		}
		sources[e.Path] = e
	}
	if req.Kind != "mkdir" && (len(b.Sources) == 0 || b.Sources[0].Path != req.From) {
		return E("invalid_intent", nil)
	}
	var expected []ManifestEntry
	add := func(action string, source, target storage.Path, v []byte, size int64) {
		entry := ManifestEntry{Number: len(expected), Action: action, Source: source, Target: target, SourceRevision: v, Size: size, TargetConditions: storage.Conditions{Absent: action != "delete"}}
		if source != "" {
			captured := cloneActionEntry(sources[source])
			entry.SourceEntry = &captured
		}
		expected = append(expected, entry)
	}
	if b.Strategy != "" && b.Strategy != nativeTreeMoveStrategy {
		return E("invalid_intent", nil)
	}
	if b.Strategy == nativeTreeMoveStrategy {
		if req.Kind != "move" || req.From == "" || req.To == "" || req.From == req.To || len(b.Sources) != 1 || b.Sources[0].Kind != storage.Folder || len(b.Sources[0].Revision) == 0 || b.Existing != nil {
			return E("invalid_intent", nil)
		}
		add("move", req.From, req.To, b.Sources[0].Revision, 0)
	} else if req.Kind == "mkdir" {
		if len(b.Sources) != 0 {
			return E("invalid_intent", nil)
		}
		if b.Existing != nil {
			if b.Existing.Path != req.To || b.Existing.Kind != storage.Folder || requireRevision(*b.Existing) != nil || len(entries) != 0 {
				return E("invalid_intent", nil)
			}
		} else {
			if len(entries) == 0 || entries[len(entries)-1].Target != req.To || !req.Parents && len(entries) != 1 {
				return E("invalid_intent", nil)
			}
			for i, e := range entries {
				if e.Action != "mkdir" || e.Source != "" || e.Size != 0 || len(e.SourceRevision) > 0 || !e.TargetConditions.Absent || len(e.TargetConditions.ExpectedRevision) > 0 || e.Number != i || i > 0 && parentPath(e.Target) != entries[i-1].Target {
					return E("invalid_intent", nil)
				}
			}
			expected = entries
		}
	} else {
		if b.Existing != nil {
			return E("invalid_intent", nil)
		}
		if req.Kind == "copy" || req.Kind == "move" {
			for _, e := range b.Sources {
				suffix := string(e.Path)[len(req.From):]
				target := req.To + storage.Path(suffix)
				if e.Kind == storage.Folder {
					add("mkdir", "", target, nil, 0)
				} else {
					add("copy", e.Path, target, e.Revision, e.Size)
				}
			}
		}
		if req.Kind == "delete" || req.Kind == "move" {
			for i := len(b.Sources) - 1; i >= 0; i-- {
				e := b.Sources[i]
				add("delete", e.Path, "", e.Revision, e.Size)
			}
		}
	}
	if len(expected) != len(entries) || len(entries) > 0 && !equalJSON(expected, entries) {
		return E("invalid_intent", nil)
	}
	var newBytes, oldBytes int64
	for _, e := range entries {
		if storage.ValidatePath(e.Source) != nil || storage.ValidatePath(e.Target) != nil {
			return E("invalid_intent", nil)
		}
		for _, path := range []storage.Path{e.Source, e.Target} {
			if path != "" && !covered(op.Root, op.Guards, path) {
				return E("invalid_intent", nil)
			}
		}
		var ok bool
		if e.Action == "copy" {
			newBytes, ok = addOK(newBytes, e.Size)
		} else if e.Action == "delete" {
			oldBytes, ok = addOK(oldBytes, e.Size)
		} else {
			ok = true
		}
		if !ok {
			return E("invalid_intent", nil)
		}
	}
	physical, err := PhysicalReservation(op.Root, newBytes)
	if err != nil {
		return err
	}
	if q.NewBytes != newBytes || q.OldBytes != oldBytes || q.PhysicalBytes != physical || newBytes < 0 || oldBytes < 0 || newBytes > 9007199254740991 || oldBytes > 9007199254740991 {
		return E("invalid_intent", nil)
	}
	return nil
}

// SaveActionApplied aggregates only durable applied entry results. It never uses
// the upload-specific staged write protocol, nor invents an effect for a failed
// zero-effect operation. An existing mkdir is a verified no-op native observation.
func (p *Postgres) SaveActionApplied(ctx context.Context, l Lease, observation *ActionObservation) error {
	return p.leased(ctx, l, func(tx pgx.Tx, op *Operation, _ time.Time) error {
		b, found, err := readActionBinding(ctx, tx, *op)
		if err != nil {
			return err
		}
		uncertain, err := operationEntriesUncertain(ctx, tx, op)
		if err != nil {
			return err
		}
		if !found || op.Terminal() || uncertain {
			return E("recovery_required", nil)
		}
		req, err := actionRequest(*op)
		if err != nil {
			return err
		}
		result := storage.MutationResult{}
		var evidence []json.RawMessage
		applied, err := appliedEntries(ctx, tx, *op)
		if err != nil {
			return err
		}
		for _, e := range applied {
			if _, err := recordedEntry(e); err != nil {
				return err
			}
			var native storage.MutationResult
			if json.Unmarshal(e.Result, &native) != nil {
				return E("invalid_native_result", nil)
			}
			evidence = append(evidence, e.Result)
			if native.Entry != nil && native.Entry.Path == req.To {
				result.Entry = native.Entry
			}
		}
		if b.Existing != nil {
			result.Entry = b.Existing
			evidence = append(evidence, json.RawMessage(`{"existing":true}`))
		}
		if len(evidence) == 0 {
			return E("invalid_transition", nil)
		}
		result.Evidence, err = json.Marshal(evidence)
		if err != nil {
			return err
		}
		if observation != nil {
			if result.Entry == nil || !sameNative(*result.Entry, observation.Entry) {
				return E("target_changed", nil)
			}
			result.Entry = &observation.Entry
			result.Evidence, err = json.Marshal(observation)
			if err != nil {
				return err
			}
		}
		if op.Phase == NativeApplied && op.NativeResult != nil {
			// Index retry retains the first complete public observation so a lost
			// database response cannot change an already saved result's revision.
			if (op.NativeResult.Entry == nil) != (result.Entry == nil) || result.Entry != nil && !sameNative(*op.NativeResult.Entry, *result.Entry) {
				return E("intent_conflict", nil)
			}
			result = *op.NativeResult
		}
		op.NativeResult = &result
		op.Phase = NativeApplied
		op.State = Running
		return nil
	})
}
func (p *Postgres) MarkActionObservationUnknown(ctx context.Context, l Lease) error {
	return p.leased(ctx, l, func(tx pgx.Tx, op *Operation, _ time.Time) error {
		_, found, err := readActionBinding(ctx, tx, *op)
		if err != nil {
			return err
		}
		uncertain, err := operationEntriesUncertain(ctx, tx, op)
		if err != nil {
			return err
		}
		if !found || op.Terminal() || uncertain {
			return E("recovery_required", nil)
		}
		if op.EntriesLayout == EntriesLayoutTable {
			unfinished, err := unfinishedEntryExists(ctx, tx, *op)
			if err != nil {
				return err
			}
			if unfinished {
				return E("recovery_required", nil)
			}
		} else {
			for _, entry := range op.Entries {
				if entry.State != "applied" {
					return E("recovery_required", nil)
				}
			}
		}
		op.State = Unknown
		return nil
	})
}

func validateActionSettlement(ctx context.Context, tx pgx.Tx, op Operation, b actionBinding, s Settlement) error {
	entries := make([]ManifestEntry, 0, len(op.Entries))
	for _, entry := range op.Entries {
		entries = append(entries, entry.ManifestEntry)
	}
	if err := validateFrozenAction(op, b, entries, op.Reservation); err != nil {
		return err
	}
	logical, physical, err := actionDeltas(op)
	if err != nil {
		return err
	}
	if s.LogicalDelta != logical || s.PhysicalDelta != physical || s.KeepLogical != 0 || s.KeepPhysical != 0 {
		return E("reservation_exceeded", nil)
	}
	if s.State == Succeeded {
		for _, entry := range op.Entries {
			if entry.State != "applied" {
				return E("invalid_transition", nil)
			}
		}
	}
	for _, entry := range op.Entries {
		if entry.Action == "delete" && entry.Size > 0 {
			// The read shim matches rows the old binary keyed with the legacy spelling.
			var exists bool
			err = tx.QueryRow(ctx, `select exists(select 1 from file_credit_claims where owner_id=$1 and operation_id=$2 and `+comparisonShim("comparison_path")+`=$3 and version=$4)`, op.Root.OwnerID, op.ID, comparePath(op.Root, entry.Source), entry.SourceRevision).Scan(&exists)
			if err != nil {
				return err
			}
			if !exists {
				return E("credit_conflict", nil)
			}
			// A deletion credit is a receipt, so only an applied entry keeps it. A cancelled
			// or not-applied delete must not prevent a future operation from deleting that
			// same source, and replacement credits never reach this loop (credit.go).
			if entry.State != "applied" {
				_, err = tx.Exec(ctx, `delete from file_credit_claims where owner_id=$1 and operation_id=$2 and `+comparisonShim("comparison_path")+`=$3 and version=$4`, op.Root.OwnerID, op.ID, comparePath(op.Root, entry.Source), entry.SourceRevision)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}
