package fileops

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

func appendCheckpoint(ctx context.Context, tx pgx.Tx, op *Operation, entry any, b []byte) error {
	if b == nil {
		b = []byte{}
	}
	_, err := tx.Exec(ctx, `insert into file_operation_checkpoints(operation_id,owner_id,entry_number,lease_epoch,checkpoint) values($1,$2,$3,$4,$5)`, op.ID, op.Root.OwnerID, entry, op.Lease.Epoch, b)
	return err
}
func (p *Postgres) SaveCheckpoint(ctx context.Context, l Lease, b []byte) error {
	return p.leased(ctx, l, func(tx pgx.Tx, op *Operation, _ time.Time) error {
		if op.Terminal() {
			return E("invalid_transition", nil)
		}
		if err := appendCheckpoint(ctx, tx, op, nil, b); err != nil {
			return err
		}
		op.Checkpoint = append([]byte(nil), b...)
		if op.Phase == Admitted {
			op.Phase = Receiving
		}
		return nil
	})
}
func equalJSON(a, b any) bool {
	x, err := json.Marshal(a)
	if err != nil {
		return false
	}
	x, err = canonicalJSON(x)
	if err != nil {
		return false
	}
	y, err := json.Marshal(b)
	if err != nil {
		return false
	}
	y, err = canonicalJSON(y)
	return err == nil && bytes.Equal(x, y)
}
func (p *Postgres) SaveStage(ctx context.Context, l Lease, s storage.Staged) error {
	return p.leased(ctx, l, func(_ pgx.Tx, op *Operation, _ time.Time) error {
		if op.Kind != "upload_commit" {
			return E("invalid_request", nil)
		}
		if op.Terminal() || op.Phase == EffectStarted || op.Phase == NativeApplied || op.Phase == Settled {
			return E("invalid_transition", nil)
		}
		var intent storage.WriteIntent
		if json.Unmarshal(op.Intent, &intent) != nil || intent.ID != "" {
			return E("invalid_request", nil)
		}
		intent.ID = op.ID
		if !equalJSON(intent, s.Intent) || s.Size != intent.Size || s.Size < 0 || len(s.Handle) == 0 || !covered(op.Root, op.Guards, intent.Target) {
			return E("intent_conflict", nil)
		}
		if intent.Digest != nil && !equalJSON(intent.Digest, s.Digest) {
			return E("intent_conflict", nil)
		}
		if op.Stage != nil {
			if !equalJSON(op.Stage, s) {
				return E("intent_conflict", nil)
			}
			return nil
		}
		op.Stage = &s
		op.Phase = StagedReady
		return nil
	})
}
func uncertainEntries(op *Operation) bool {
	for _, e := range op.Entries {
		if e.State == "started" || e.State == "outcome_unknown" {
			return true
		}
	}
	return false
}
func anyApplied(op *Operation) bool {
	for _, e := range op.Entries {
		if e.State == "applied" {
			return true
		}
	}
	return false
}
func (p *Postgres) StartEffect(ctx context.Context, l Lease) error {
	return p.leased(ctx, l, func(tx pgx.Tx, op *Operation, _ time.Time) error {
		uncertain, err := operationEntriesUncertain(ctx, tx, op)
		if err != nil {
			return err
		}
		if op.Phase == EffectStarted || op.Phase == NativeApplied || op.State == Unknown || uncertain {
			return E("recovery_required", nil)
		}
		if op.Terminal() || op.CancelRequested {
			return E("invalid_transition", nil)
		}
		if op.Kind == "upload_commit" && op.Stage == nil {
			return E("invalid_transition", nil)
		}
		op.Phase = EffectStarted
		return nil
	})
}
func (p *Postgres) SaveApplied(ctx context.Context, l Lease, result storage.MutationResult) error {
	return p.leased(ctx, l, func(tx pgx.Tx, op *Operation, _ time.Time) error {
		if op.Phase == NativeApplied {
			if !equalJSON(op.NativeResult, result) {
				return E("intent_conflict", nil)
			}
			return nil
		}
		uncertain, err := operationEntriesUncertain(ctx, tx, op)
		if err != nil {
			return err
		}
		if op.Terminal() || op.Phase != EffectStarted || uncertain {
			return E("invalid_transition", nil)
		}
		if result.Entry != nil && (result.Entry.Size < 0 || storage.ValidatePath(result.Entry.Path) != nil || !covered(op.Root, op.Guards, result.Entry.Path)) {
			return E("invalid_request", nil)
		}
		if op.Stage != nil && (result.Entry == nil || result.Entry.Path != op.Stage.Intent.Target || result.Entry.Size != op.Stage.Size) {
			return E("intent_conflict", nil)
		}
		op.NativeResult = &result
		op.Phase = NativeApplied
		op.State = Running
		return nil
	})
}
func (p *Postgres) MarkUnknown(ctx context.Context, l Lease, evidence []byte) error {
	return p.leased(ctx, l, func(tx pgx.Tx, op *Operation, _ time.Time) error {
		if op.Terminal() || op.Phase != EffectStarted {
			return E("invalid_transition", nil)
		}
		if err := appendCheckpoint(ctx, tx, op, nil, evidence); err != nil {
			return err
		}
		if len(evidence) > 0 {
			op.RecoveryEvidence = append([]byte(nil), evidence...)
		}
		op.State = Unknown
		return nil
	})
}
func (p *Postgres) BeginEntry(ctx context.Context, l Lease, n int) error {
	return p.leased(ctx, l, func(tx pgx.Tx, op *Operation, _ time.Time) error {
		uncertain, err := operationEntriesUncertain(ctx, tx, op)
		if err != nil {
			return err
		}
		if op.Terminal() || op.CancelRequested || op.Phase == NativeApplied || op.State == Unknown || uncertain {
			return E("recovery_required", nil)
		}
		if op.EntriesLayout == EntriesLayoutTable {
			// One conditional row update. 0 rows means the entry is not pending — another
			// attempt claimed it or it is terminal — and the answer is recovery_required,
			// never a second native effect (§2.2).
			tag, err := tx.Exec(ctx, beginEntrySQL, op.ID, op.Root.OwnerID, n)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return E("recovery_required", nil)
			}
			op.Phase = EffectStarted
			return nil
		}
		for i := range op.Entries {
			e := &op.Entries[i]
			if e.Number == n {
				if e.State != "pending" {
					return E("recovery_required", nil)
				}
				e.State = "started"
				op.Phase = EffectStarted
				return nil
			}
		}
		return E("not_found", nil)
	})
}
func (p *Postgres) SaveEntry(ctx context.Context, l Lease, n int, state string, checkpoint, evidence []byte, result json.RawMessage) error {
	return p.leased(ctx, l, func(tx pgx.Tx, op *Operation, _ time.Time) error {
		if op.Terminal() || op.Phase == NativeApplied {
			return E("invalid_transition", nil)
		}
		if len(result) > 0 {
			var err error
			result, err = canonicalJSON(result)
			if err != nil {
				return err
			}
		}
		// The transition validates against the entry's current durable value. For a table
		// row that is one row read under the parent lock this transaction already holds;
		// for a document row it is the list the document carried.
		entry, position, err := operationEntry(ctx, op, tx, n)
		if err != nil {
			return err
		}
		if entry.State == "applied" || entry.State == "not_applied" {
			old := entry
			old.Checkpoint = checkpoint
			old.Evidence = evidence
			old.Result = result
			old.State = state
			// A replayed transition already matches the durable value; nothing is written.
			if !equalJSON(old, entry) {
				return E("intent_conflict", nil)
			}
			if op.EntriesLayout != EntriesLayoutTable {
				op.Entries[position] = entry
			}
			return nil
		}
		switch state {
		case "started":
			if entry.State != "started" {
				return E("invalid_transition", nil)
			}
			if len(evidence) == 0 {
				evidence = entry.Evidence
			} else if len(entry.Evidence) > 0 && !bytes.Equal(entry.Evidence, evidence) {
				return E("intent_conflict", nil)
			}
			if len(result) == 0 {
				result = entry.Result
			} else if len(entry.Result) > 0 && !equalJSON(entry.Result, result) {
				return E("intent_conflict", nil)
			}
		case "pending":
			if entry.State != "pending" {
				return E("invalid_transition", nil)
			}
		case "applied", "not_applied", "outcome_unknown":
			if state != "outcome_unknown" && len(evidence) == 0 {
				return E("invalid_request", nil)
			}
			if entry.State != "started" && entry.State != "outcome_unknown" {
				return E("invalid_transition", nil)
			}
		default:
			return E("invalid_request", nil)
		}
		if err := appendCheckpoint(ctx, tx, op, n, checkpoint); err != nil {
			return err
		}
		entry.State = state
		entry.Checkpoint = append([]byte(nil), checkpoint...)
		entry.Evidence = append([]byte(nil), evidence...)
		entry.Result = result
		if op.EntriesLayout == EntriesLayoutTable {
			// One row update, ordered behind the parent row lock this transaction holds.
			var value any
			if len(result) > 0 {
				value = string(result)
			}
			if _, err := tx.Exec(ctx, saveEntrySQL, op.ID, op.Root.OwnerID, n, entry.State, entry.Checkpoint, entry.Evidence, value); err != nil {
				return err
			}
			uncertain, err := uncertainEntriesExist(ctx, tx, *op)
			if err != nil {
				return err
			}
			if state == "outcome_unknown" {
				op.State = Unknown
			} else if op.State == Unknown && !uncertain {
				op.State = Running
			}
			return nil
		}
		op.Entries[position] = entry
		if state == "outcome_unknown" {
			op.State = Unknown
		} else if op.State == Unknown && !uncertainEntries(op) {
			op.State = Running
		}
		return nil
	})
}

// operationEntry returns the entry with this number and, for a document-backed
// operation, its position in the list.
func operationEntry(ctx context.Context, op *Operation, tx pgx.Tx, number int) (EntryEffect, int, error) {
	if op.EntriesLayout == EntriesLayoutTable {
		entry, err := loadEntry(ctx, tx, *op, number)
		return entry, -1, err
	}
	for i := range op.Entries {
		if op.Entries[i].Number == number {
			return op.Entries[i], i, nil
		}
	}
	return EntryEffect{}, -1, E("not_found", nil)
}

// operationEntriesUncertain answers "may an entry still land?" from the durable layout:
// a row predicate for a table-backed operation, the loaded list for a document-backed
// one. It must run in a transaction that holds the parent row lock.
func operationEntriesUncertain(ctx context.Context, tx pgx.Tx, op *Operation) (bool, error) {
	if op.EntriesLayout == EntriesLayoutTable {
		return uncertainEntriesExist(ctx, tx, *op)
	}
	return uncertainEntries(op), nil
}

// operationEntriesApplied is the same for "did any entry apply?".
func operationEntriesApplied(ctx context.Context, tx pgx.Tx, op *Operation) (bool, error) {
	if op.EntriesLayout == EntriesLayoutTable {
		return anyAppliedEntry(ctx, tx, *op)
	}
	return anyApplied(op), nil
}

func terminalState(s State) bool { return s == Succeeded || s == Failed || s == Cancelled }
func finish(op *Operation, s State, now time.Time) {
	op.State = s
	op.Phase = Settled
	op.FinishedAt = &now
	// The requested retention applies after termination as well as during admission.
	duration := max(op.RetainUntil.Sub(op.CreatedAt), 24*time.Hour)
	op.RetainUntil = now.Add(duration)
}
func releaseGuards(ctx context.Context, tx pgx.Tx, op *Operation) error {
	_, err := tx.Exec(ctx, `delete from file_operation_guards where operation_id=$1 and owner_id=$2`, op.ID, op.Root.OwnerID)
	return err
}
func (p *Postgres) FinishWithoutEffect(ctx context.Context, l Lease, s State, problem json.RawMessage) error {
	if s != Failed && s != Cancelled {
		return E("invalid_request", nil)
	}
	if len(problem) > 0 {
		var err error
		problem, err = canonicalJSON(problem)
		if err != nil {
			return err
		}
	}
	return p.leased(ctx, l, func(tx pgx.Tx, op *Operation, now time.Time) error {
		if op.Terminal() {
			if op.State == s && equalJSON(op.Problem, problem) {
				return nil
			}
			return E("invalid_transition", nil)
		}
		uncertain, err := operationEntriesUncertain(ctx, tx, op)
		if err != nil {
			return err
		}
		applied, err := operationEntriesApplied(ctx, tx, op)
		if err != nil {
			return err
		}
		if op.Phase == EffectStarted || op.Phase == NativeApplied || op.NativeResult != nil || uncertain || applied {
			return E("recovery_required", nil)
		}
		// The caller has proved all staging allocations unused/cleaned before invoking.
		var logical, physical int64
		err = tx.QueryRow(ctx, `select logical_reserved,physical_reserved from file_reservations where operation_id=$1 for update`, op.ID).Scan(&logical, &physical)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update file_budgets set logical_reserved=logical_reserved-$2,physical_reserved=physical_reserved-$3 where owner_id=$1`, op.Root.OwnerID, logical, physical)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update file_reservations set logical_reserved=0,physical_reserved=0,settled=true where operation_id=$1`, op.ID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `delete from file_credit_claims where operation_id=$1 and owner_id=$2`, op.ID, op.Root.OwnerID)
		if err != nil {
			return err
		}
		op.Problem = problem
		finish(op, s, now)
		return releaseGuards(ctx, tx, op)
	})
}
func (p *Postgres) Settle(ctx context.Context, l Lease, s Settlement, commit IndexCommit) (Operation, error) {
	var out Operation
	if !terminalState(s.State) || s.KeepLogical < 0 || s.KeepPhysical < 0 {
		return out, E("invalid_request", nil)
	}
	var err error
	if len(s.Result) > 0 {
		s.Result, err = canonicalJSON(s.Result)
		if err != nil {
			return out, err
		}
	}
	if len(s.Problem) > 0 {
		s.Problem, err = canonicalJSON(s.Problem)
		if err != nil {
			return out, err
		}
	}
	err = p.leased(ctx, l, func(tx pgx.Tx, op *Operation, now time.Time) error {
		var request, prior []byte
		var logical, physical int64
		var settled bool
		err := tx.QueryRow(ctx, `select request,logical_reserved,physical_reserved,settled,settlement from file_reservations where operation_id=$1 for update`, op.ID).Scan(&request, &logical, &physical, &settled, &prior)
		if err != nil {
			return err
		}
		if settled {
			var old Settlement
			if len(prior) == 0 || json.Unmarshal(prior, &old) != nil || !equalJSON(old, s) {
				return E("intent_conflict", nil)
			}
			out = *op
			return nil
		}
		// The settlement's one ordered load. It must stay here: this transaction holds the
		// parent row lock from the readOperation above, so no transition can change an
		// entry between this read and the index commit that validates it (§2.3, §2.7).
		// Hoisting it into Get would reopen the window where an index row commits for an
		// entry whose evidence was not read.
		if op.EntriesLayout == EntriesLayoutTable {
			entries, loadErr := loadEntries(ctx, tx, *op)
			if loadErr != nil {
				return loadErr
			}
			op.Entries = entries
		}
		binding, action, err := readActionBinding(ctx, tx, *op)
		if err != nil {
			return err
		}
		if op.Terminal() || uncertainEntries(op) || (!action && (op.Phase != NativeApplied || op.NativeResult == nil)) || (action && anyApplied(op) && (op.Phase != NativeApplied || op.NativeResult == nil)) {
			return E("recovery_required", nil)
		}
		if action {
			if err = validateActionSettlement(ctx, tx, *op, binding, s); err != nil {
				return err
			}
		}
		pending := false
		for _, e := range op.Entries {
			if e.State == "pending" {
				pending = true
			}
			if e.State != "pending" && e.State != "applied" && e.State != "not_applied" {
				return E("recovery_required", nil)
			}
		}
		if pending && (s.State == Succeeded || !explicitRemaining(s.Result)) {
			return E("invalid_transition", nil)
		}
		var req ReservationRequest
		if err = json.Unmarshal(request, &req); err != nil {
			return err
		}
		upper := max(req.NewBytes-req.OldBytes, 0)
		if action {
			upper = req.NewBytes
		}
		if s.LogicalDelta < -req.OldBytes || s.LogicalDelta > upper || s.KeepLogical > logical || s.KeepPhysical > physical {
			return E("reservation_exceeded", nil)
		}
		if !action && (op.ChargedLogical > 0 && s.LogicalDelta < op.ChargedLogical || op.ChargedPhysical > 0 && s.PhysicalDelta < op.ChargedPhysical) {
			return E("reservation_exceeded", nil)
		}
		logicalDelta := s.LogicalDelta - op.ChargedLogical
		physicalDelta := s.PhysicalDelta - op.ChargedPhysical
		if len(op.Entries) == 0 && op.Stage != nil && s.LogicalDelta != op.Stage.Size-req.OldBytes {
			return E("reservation_exceeded", nil)
		}
		if !action && op.Root.RetainsVersions && op.Stage != nil && s.PhysicalDelta != PhysicalDelta(op.Root, op.Stage.Size, req.OldBytes) {
			return E("reservation_exceeded", nil)
		}
		// Settled reservations retain a conservative liability, but cannot be spent again.
		if max(logicalDelta, 0) > logical-s.KeepLogical || max(physicalDelta, 0) > physical-s.KeepPhysical {
			return E("reservation_exceeded", nil)
		}
		u, err := budget(ctx, tx, op.Root)
		if err != nil {
			return err
		}
		used, ok := addOK(u.LogicalBytes, logicalDelta)
		if !ok || used < 0 {
			return E("reservation_exceeded", nil)
		}
		phys, ok := addOK(u.PhysicalBytes, physicalDelta)
		if !ok || phys < 0 {
			return E("reservation_exceeded", nil)
		}
		op.Result = s.Result
		if action {
			op.State = s.State
			op.Problem = s.Problem
		}
		if commit != nil {
			result, err := commit(ctx, tx, *op, op.NativeResult)
			if err != nil {
				return err
			}
			if len(result) > 0 {
				result, err = canonicalJSON(result)
				if err != nil {
					return err
				}
				op.Result = result
			}
		}
		if len(op.Result) == 0 {
			op.Result = s.Result
		}
		op.Problem = s.Problem
		_, err = tx.Exec(ctx, `update file_budgets set logical_bytes=$2,physical_bytes=$3,logical_reserved=logical_reserved-$4+$5,physical_reserved=physical_reserved-$6+$7 where owner_id=$1`, op.Root.OwnerID, used, phys, logical, s.KeepLogical, physical, s.KeepPhysical)
		if err != nil {
			return err
		}
		sb, _ := json.Marshal(s)
		_, err = tx.Exec(ctx, `update file_reservations set logical_reserved=$2,physical_reserved=$3,settled=true,settlement=$4 where operation_id=$1`, op.ID, s.KeepLogical, s.KeepPhysical, sb)
		if err != nil {
			return err
		}
		// A replacement's credit is a fence, and this commit is the end of the window in
		// which it can be spent: the operation is terminal, its delta is in the budget,
		// and it can never publish again. Keeping the row would make identical content
		// unreplaceable, because identical bytes carry the identical native revision and
		// the next replacement must be allowed to claim that revision (it credits bytes
		// that still exist, so it is not a second refund). The release is in this
		// transaction precisely so a crash between the native write and this settlement
		// cannot open that window: until this COMMIT the claim and the operation's phase
		// stay as they were, and recovery settles through this same statement.
		//
		// Only a succeeded settlement may release it. A non-action settlement always
		// follows a recorded native publication, so a failed or cancelled one means the
		// old revision was consumed while the accounting is not the ordinary replacement
		// delta; that claim stays (fail closed), and deletion claims are never touched
		// here — validateActionSettlement owns them and keeps the applied ones.
		if !action && s.State == Succeeded && req.OldBytes > 0 {
			if _, err = tx.Exec(ctx, `delete from file_credit_claims where owner_id=$1 and operation_id=$2 and `+comparisonShim("comparison_path")+`=$3 and version=$4`, op.Root.OwnerID, op.ID, comparePath(op.Root, req.CreditPath), req.CreditVersion); err != nil {
				return err
			}
		}
		op.ChargedLogical = s.LogicalDelta
		op.ChargedPhysical = s.PhysicalDelta
		finish(op, s.State, now)
		// Retained liabilities keep their guards, even though the known part is settled.
		if s.KeepLogical == 0 && s.KeepPhysical == 0 {
			if err = releaseGuards(ctx, tx, op); err != nil {
				return err
			}
		}
		out = *op
		return nil
	})
	return out, err
}
