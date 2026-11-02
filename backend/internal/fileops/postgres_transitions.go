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
	return p.leased(ctx, l, func(_ pgx.Tx, op *Operation, _ time.Time) error {
		if op.Phase == EffectStarted || op.Phase == NativeApplied || op.State == Unknown || uncertainEntries(op) {
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
	return p.leased(ctx, l, func(_ pgx.Tx, op *Operation, _ time.Time) error {
		if op.Phase == NativeApplied {
			if !equalJSON(op.NativeResult, result) {
				return E("intent_conflict", nil)
			}
			return nil
		}
		if op.Terminal() || op.Phase != EffectStarted || uncertainEntries(op) {
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
	return p.leased(ctx, l, func(_ pgx.Tx, op *Operation, _ time.Time) error {
		if op.Terminal() || op.CancelRequested || op.Phase == NativeApplied || op.State == Unknown || uncertainEntries(op) {
			return E("recovery_required", nil)
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
		for i := range op.Entries {
			e := &op.Entries[i]
			if e.Number != n {
				continue
			}
			if e.State == "applied" || e.State == "not_applied" {
				old := *e
				old.Checkpoint = checkpoint
				old.Evidence = evidence
				old.Result = result
				old.State = state
				if !equalJSON(old, *e) {
					return E("intent_conflict", nil)
				}
				return nil
			}
			switch state {
			case "started":
				if e.State != "started" {
					return E("invalid_transition", nil)
				}
				if len(evidence) == 0 {
					evidence = e.Evidence
				} else if len(e.Evidence) > 0 && !bytes.Equal(e.Evidence, evidence) {
					return E("intent_conflict", nil)
				}
				if len(result) == 0 {
					result = e.Result
				} else if len(e.Result) > 0 && !equalJSON(e.Result, result) {
					return E("intent_conflict", nil)
				}
			case "pending":
				if e.State != "pending" {
					return E("invalid_transition", nil)
				}
			case "applied", "not_applied", "outcome_unknown":
				if state != "outcome_unknown" && len(evidence) == 0 {
					return E("invalid_request", nil)
				}
				if e.State != "started" && e.State != "outcome_unknown" {
					return E("invalid_transition", nil)
				}
			default:
				return E("invalid_request", nil)
			}
			if err := appendCheckpoint(ctx, tx, op, n, checkpoint); err != nil {
				return err
			}
			e.State = state
			e.Checkpoint = append([]byte(nil), checkpoint...)
			e.Evidence = append([]byte(nil), evidence...)
			e.Result = result
			if state == "outcome_unknown" {
				op.State = Unknown
			} else if op.State == Unknown && !uncertainEntries(op) {
				op.State = Running
			}
			return nil
		}
		return E("not_found", nil)
	})
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
		if op.Phase == EffectStarted || op.Phase == NativeApplied || op.NativeResult != nil || uncertainEntries(op) || anyApplied(op) {
			return E("recovery_required", nil)
		}
		// The caller has proved all staging allocations unused/cleaned before invoking.
		var logical, physical int64
		err := tx.QueryRow(ctx, `select logical_reserved,physical_reserved from file_reservations where operation_id=$1 for update`, op.ID).Scan(&logical, &physical)
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
