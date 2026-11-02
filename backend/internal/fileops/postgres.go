package fileops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres persists metadata, guards and quota; native contents never enter this store.
// Owner advisory locks order all transitions before row locks and serialize admission.
// P3 deliberately forbids changing the budget root binding, including after settlement.
type Postgres struct{ pool *pgxpool.Pool }

var _ Journal = (*Postgres)(nil)

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}
func ownerLock(ctx context.Context, tx pgx.Tx, owner string) error {
	_, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1::uuid::text, 1739))`, owner)
	return err
}
func dbNow(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var t time.Time
	err := tx.QueryRow(ctx, `select clock_timestamp()`).Scan(&t)
	return t, err
}
func readOperation(ctx context.Context, tx pgx.Tx, id string) (Operation, error) {
	var op Operation
	var b []byte
	err := tx.QueryRow(ctx, `select snapshot from file_operations where id=$1 for update`, id).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return op, E("not_found", nil)
	}
	if err != nil {
		return op, err
	}
	err = json.Unmarshal(b, &op)
	if err == nil {
		err = canonicalOperation(&op)
	}
	return op, err
}
func saveOperation(ctx context.Context, tx pgx.Tx, op Operation) error {
	b, err := json.Marshal(op)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `update file_operations set snapshot=$2 where id=$1`, op.ID, b)
	return err
}
func (p *Postgres) transaction(ctx context.Context, owner string, fn func(pgx.Tx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = ownerLock(ctx, tx, owner); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func budget(ctx context.Context, tx pgx.Tx, root Root) (Usage, error) {
	var u Usage
	var b []byte
	err := tx.QueryRow(ctx, `select root_binding,logical_bytes,logical_reserved,physical_bytes,physical_reserved,logical_limit,physical_limit from file_budgets where owner_id=$1 for update`, root.OwnerID).Scan(&b, &u.LogicalBytes, &u.LogicalReserved, &u.PhysicalBytes, &u.PhysicalReserved, &u.LogicalLimit, &u.PhysicalLimit)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, E("baseline_required", nil)
	}
	if err != nil {
		return u, err
	}
	var r Root
	if err = json.Unmarshal(b, &r); err != nil {
		return u, err
	}
	if r != root {
		return u, E("root_conflict", nil)
	}
	return u, nil
}
func (p *Postgres) Initialize(ctx context.Context, root Root, base Baseline) error {
	if err := validateRoot(root); err != nil {
		return err
	}
	if !base.NativeComplete || !base.IndexCompared || !base.LegacyWritersDrained || !nonemptyEvidence(base.Evidence) || base.LogicalBytes < 0 || base.PhysicalBytes < 0 || base.LogicalLimit < 0 || base.PhysicalLimit < 0 {
		return E("baseline_required", nil)
	}
	var err error
	base.Evidence, err = canonicalJSON(base.Evidence)
	if err != nil {
		return err
	}
	return p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error {
		var r, b []byte
		err := tx.QueryRow(ctx, `select root_binding,baseline from file_budgets where owner_id=$1 for update`, root.OwnerID).Scan(&r, &b)
		rb, _ := json.Marshal(root)
		bb, _ := json.Marshal(base)
		if err == nil {
			var storedRoot Root
			var storedBase Baseline
			if err = json.Unmarshal(r, &storedRoot); err != nil {
				return err
			}
			if err = json.Unmarshal(b, &storedBase); err != nil {
				return err
			}
			storedBase.Evidence, _ = canonicalJSON(storedBase.Evidence)
			canonicalStored, _ := json.Marshal(storedBase)
			if storedRoot != root {
				return E("root_conflict", nil)
			}
			if string(canonicalStored) != string(bb) {
				return E("baseline_conflict", nil)
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, err = tx.Exec(ctx, `insert into file_budgets(owner_id,root_binding,baseline,logical_bytes,physical_bytes,logical_limit,physical_limit) values($1,$2,$3,$4,$5,$6,$7)`, root.OwnerID, rb, bb, base.LogicalBytes, base.PhysicalBytes, base.LogicalLimit, base.PhysicalLimit)
		return err
	})
}
func (p *Postgres) Usage(ctx context.Context, root Root, a Actor) (Usage, error) {
	var u Usage
	if err := validateRoot(root); err != nil {
		return u, err
	}
	if a.ID == "" || (!a.CanRead && !a.CanWrite) || a.Scope != "" {
		return u, E("forbidden", nil)
	}
	err := p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error { var err error; u, err = budget(ctx, tx, root); return err })
	return u, err
}
func (p *Postgres) Begin(ctx context.Context, input BeginRequest) (Operation, bool, error) {
	req, fingerprint, reserve, err := validateBegin(input)
	if err != nil {
		return Operation{}, false, err
	}
	var op Operation
	replayed := false
	err = p.transaction(ctx, req.Root.OwnerID, func(tx pgx.Tx) error {
		// Replay precedes quota/conflict checks and does not require live native ancestry.
		if req.IdempotencyKey != "" {
			var id string
			err := tx.QueryRow(ctx, `select id::text from file_operations where owner_id=$1 and principal_id=$2 and action=$3 and idempotency_key=$4`, req.Root.OwnerID, req.Actor.ID, req.Kind, req.IdempotencyKey).Scan(&id)
			if err == nil {
				op, err = readOperation(ctx, tx, id)
				if err != nil {
					return err
				}
				if op.Fingerprint != fingerprint {
					return E("idempotency_conflict", nil)
				}
				if err = authorizeOperation(op, req.Root, req.Actor, true); err != nil {
					return err
				}
				replayed = true
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		u, err := budget(ctx, tx, req.Root)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `select g.native_path,g.subtree,o.action from file_operation_guards g join file_operations o on o.id=g.operation_id and o.owner_id=g.owner_id where g.owner_id=$1`, req.Root.OwnerID)
		if err != nil {
			return err
		}
		conflict := false
		for rows.Next() {
			var g Guard
			var kind string
			if err = rows.Scan(&g.Path, &g.Subtree, &kind); err != nil {
				rows.Close()
				return err
			}
			if kind == "move" {
				g.Subtree = true
			}
			for _, want := range req.Guards {
				if req.Kind == "move" {
					want.Subtree = true
				}
				if guardConflict(req.Root, g, want) {
					conflict = true
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if conflict {
			return E("operation_in_progress", nil)
		}
		if !budgetFits(u.LogicalBytes, u.LogicalReserved, reserve, u.LogicalLimit) {
			return E("quota_exceeded", nil)
		}
		if !budgetFits(u.PhysicalBytes, u.PhysicalReserved, req.Reservation.PhysicalBytes, u.PhysicalLimit) {
			return E("staging_full", nil)
		}
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		retention := max(req.RetainFor, 24*time.Hour)
		op = Operation{ID: newID(), Root: req.Root, PrincipalID: req.Actor.ID, Kind: req.Kind, State: Queued, Phase: Admitted, Intent: req.Intent, Fingerprint: fingerprint, Guards: req.Guards, CreatedAt: now, RetainUntil: now.Add(retention), Reservation: req.Reservation}
		for _, e := range req.Manifest {
			op.Entries = append(op.Entries, EntryEffect{ManifestEntry: e, State: "pending"})
		}
		b, err := json.Marshal(op)
		if err != nil {
			return err
		}
		var key any
		if req.IdempotencyKey != "" {
			key = req.IdempotencyKey
		}
		_, err = tx.Exec(ctx, `insert into file_operations(id,owner_id,principal_id,action,idempotency_key,fingerprint,snapshot) values($1,$2,$3,$4,$5,$6,$7)`, op.ID, req.Root.OwnerID, req.Actor.ID, req.Kind, key, fingerprint, b)
		if err != nil {
			return err
		}
		for i, g := range req.Guards {
			_, err = tx.Exec(ctx, `insert into file_operation_guards(operation_id,owner_id,ordinal,native_path,comparison_path,subtree) values($1,$2,$3,$4,$5,$6)`, op.ID, req.Root.OwnerID, i, string(g.Path), comparePath(req.Root, g.Path), g.Subtree)
			if err != nil {
				return err
			}
		}
		if req.Reservation.OldBytes > 0 {
			tag, err := tx.Exec(ctx, `insert into file_credit_claims(owner_id,comparison_path,version,operation_id) values($1,$2,$3,$4) on conflict do nothing`, req.Root.OwnerID, comparePath(req.Root, req.Reservation.CreditPath), req.Reservation.CreditVersion, op.ID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return E("credit_conflict", nil)
			}
		}
		rb, _ := json.Marshal(req.Reservation)
		_, err = tx.Exec(ctx, `insert into file_reservations(operation_id,owner_id,request,logical_reserved,physical_reserved) values($1,$2,$3,$4,$5)`, op.ID, req.Root.OwnerID, rb, reserve, req.Reservation.PhysicalBytes)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update file_budgets set logical_reserved=logical_reserved+$2,physical_reserved=physical_reserved+$3 where owner_id=$1`, req.Root.OwnerID, reserve, req.Reservation.PhysicalBytes)
		return err
	})
	return op, replayed, err
}
func (p *Postgres) Get(ctx context.Context, root Root, a Actor, id string) (Operation, error) {
	var op Operation
	if err := validateRoot(root); err != nil {
		return op, err
	}
	if !validID(id) {
		return op, E("not_found", nil)
	}
	err := p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error {
		var err error
		op, err = readOperation(ctx, tx, id)
		if err != nil {
			return err
		}
		return authorizeOperation(op, root, a, false)
	})
	return op, err
}
func validID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
func (p *Postgres) Claim(ctx context.Context, root Root, a Actor, id string, ttl time.Duration) (Operation, error) {
	var op Operation
	if err := validateRoot(root); err != nil {
		return op, err
	}
	if !validID(id) || ttl <= 0 || ttl > 24*time.Hour {
		return op, E("invalid_request", nil)
	}
	err := p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error {
		var err error
		op, err = readOperation(ctx, tx, id)
		if err != nil {
			return err
		}
		if err = authorizeOperation(op, root, a, true); err != nil {
			return err
		}
		if op.Terminal() {
			return E("invalid_transition", nil)
		}
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		if op.Lease.Token != "" && op.Lease.ExpiresAt.After(now) {
			return E("operation_in_progress", nil)
		}
		op.Lease = Lease{OperationID: op.ID, Token: newID(), Epoch: op.Lease.Epoch + 1, ExpiresAt: now.Add(ttl)}
		if op.Phase == EffectStarted && (len(op.Entries) == 0 || uncertainEntries(&op)) {
			op.State = Unknown
		} else if op.State != Unknown {
			op.State = Running
		}
		return saveOperation(ctx, tx, op)
	})
	return op, err
}

// leased acquires the owner lock before operation rows, then checks the fence using
// the database clock. Expiry blocks every mutation, including checkpoints and results.
func (p *Postgres) leased(ctx context.Context, l Lease, fn func(pgx.Tx, *Operation, time.Time) error) error {
	if !validID(l.OperationID) || l.Token == "" || l.Epoch < 1 {
		return E("lease_lost", nil)
	}
	var owner string
	err := p.pool.QueryRow(ctx, `select owner_id::text from file_operations where id=$1`, l.OperationID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return E("not_found", nil)
	}
	if err != nil {
		return err
	}
	return p.transaction(ctx, owner, func(tx pgx.Tx) error {
		op, err := readOperation(ctx, tx, l.OperationID)
		if err != nil {
			return err
		}
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		if op.Lease.Token != l.Token || op.Lease.Epoch != l.Epoch || !op.Lease.ExpiresAt.After(now) {
			return E("lease_lost", nil)
		}
		if err = fn(tx, &op, now); err != nil {
			return err
		}
		return saveOperation(ctx, tx, op)
	})
}
func (p *Postgres) Renew(ctx context.Context, l Lease, ttl time.Duration) (Lease, error) {
	var out Lease
	if ttl <= 0 || ttl > 24*time.Hour {
		return out, E("invalid_request", nil)
	}
	err := p.leased(ctx, l, func(_ pgx.Tx, op *Operation, now time.Time) error {
		if op.Terminal() {
			return E("invalid_transition", nil)
		}
		op.Lease.ExpiresAt = now.Add(ttl)
		out = op.Lease
		return nil
	})
	return out, err
}
func (p *Postgres) RequestCancel(ctx context.Context, root Root, a Actor, id string) (Operation, error) {
	var op Operation
	if err := validateRoot(root); err != nil {
		return op, err
	}
	if !validID(id) {
		return op, E("not_found", nil)
	}
	err := p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error {
		var err error
		op, err = readOperation(ctx, tx, id)
		if err != nil {
			return err
		}
		if err = authorizeOperation(op, root, a, true); err != nil {
			return err
		}
		op.CancelRequested = true
		return saveOperation(ctx, tx, op)
	})
	return op, err
}
