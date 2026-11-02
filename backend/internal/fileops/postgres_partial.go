package fileops

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var _ PartialJournal = (*Postgres)(nil)

// AccountEntry books only proven positive native liability. It consumes the same
// amount of reserved liability, preserving total budget exposure, and never releases
// a guard or changes native certainty for other entries. Deletes/credit corrections
// are conservatively deferred to the exact final settlement.
func (p *Postgres) AccountEntry(ctx context.Context, l Lease, charge EntryCharge) (Operation, error) {
	var out Operation
	if charge.Number < 0 || charge.LogicalDelta < 0 || charge.PhysicalDelta < 0 {
		return out, E("invalid_request", nil)
	}
	err := p.leased(ctx, l, func(tx pgx.Tx, op *Operation, _ time.Time) error {
		var prior []byte
		err := tx.QueryRow(ctx, `select charge from file_entry_charges where operation_id=$1 and entry_number=$2`, op.ID, charge.Number).Scan(&prior)
		if err == nil {
			var old EntryCharge
			if json.Unmarshal(prior, &old) != nil || old != charge {
				return E("intent_conflict", nil)
			}
			out = *op
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if op.Terminal() {
			return E("invalid_transition", nil)
		}
		found := false
		for _, entry := range op.Entries {
			if entry.Number == charge.Number {
				if entry.State != "applied" || len(entry.Evidence) == 0 {
					return E("recovery_required", nil)
				}
				if charge.LogicalDelta > entry.Size {
					return E("reservation_exceeded", nil)
				}
				if op.Root.RetainsVersions && (entry.Action != "copy" || charge.LogicalDelta != entry.Size || charge.PhysicalDelta != entry.Size) {
					return E("reservation_exceeded", nil)
				}
				found = true
				break
			}
		}
		if !found {
			return E("not_found", nil)
		}
		var logical, physical int64
		var settled bool
		err = tx.QueryRow(ctx, `select logical_reserved,physical_reserved,settled from file_reservations where operation_id=$1 for update`, op.ID).Scan(&logical, &physical, &settled)
		if err != nil {
			return err
		}
		if settled || charge.LogicalDelta > logical || charge.PhysicalDelta > physical {
			return E("reservation_exceeded", nil)
		}
		u, err := budget(ctx, tx, op.Root)
		if err != nil {
			return err
		}
		logicalUsed, ok := addOK(u.LogicalBytes, charge.LogicalDelta)
		if !ok {
			return E("reservation_exceeded", nil)
		}
		physicalUsed, ok := addOK(u.PhysicalBytes, charge.PhysicalDelta)
		if !ok {
			return E("reservation_exceeded", nil)
		}
		chargedLogical, ok := addOK(op.ChargedLogical, charge.LogicalDelta)
		if !ok {
			return E("reservation_exceeded", nil)
		}
		chargedPhysical, ok := addOK(op.ChargedPhysical, charge.PhysicalDelta)
		if !ok {
			return E("reservation_exceeded", nil)
		}
		b, _ := json.Marshal(charge)
		_, err = tx.Exec(ctx, `insert into file_entry_charges(operation_id,owner_id,entry_number,charge) values($1,$2,$3,$4)`, op.ID, op.Root.OwnerID, charge.Number, b)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update file_budgets set logical_bytes=$2,physical_bytes=$3,logical_reserved=logical_reserved-$4,physical_reserved=physical_reserved-$5 where owner_id=$1`, op.Root.OwnerID, logicalUsed, physicalUsed, charge.LogicalDelta, charge.PhysicalDelta)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update file_reservations set logical_reserved=logical_reserved-$2,physical_reserved=physical_reserved-$3 where operation_id=$1`, op.ID, charge.LogicalDelta, charge.PhysicalDelta)
		if err != nil {
			return err
		}
		op.ChargedLogical = chargedLogical
		op.ChargedPhysical = chargedPhysical
		out = *op
		return nil
	})
	return out, err
}
