package fileops

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"everythingabout.me/backend/internal/storage"
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

// OwnerLockKey is the advisory-lock key expression that admits one owner's writes.
// It is exported because a scan must take the same key: two lock spaces cannot
// exclude each other, and a scan on a different key would prune an upload or
// resurrect a deletion that settled after its native walk.
const OwnerLockKey = `hashtextextended($1::uuid::text, 1739)`

func ownerLock(ctx context.Context, tx pgx.Tx, owner string) error {
	_, err := tx.Exec(ctx, `select pg_advisory_xact_lock(`+OwnerLockKey+`)`, owner)
	return err
}
func dbNow(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var t time.Time
	err := tx.QueryRow(ctx, `select clock_timestamp()`).Scan(&t)
	return t, err
}

// journalColumns is the promoted half of one operation row: exactly the fields a SQL
// predicate is allowed to read. It is derived from the operation document, never stored
// independently, so the only way the two can disagree is a row written by a binary from
// before sql/047 (or corruption) — see divergence.
//
// The reason the split exists: while these fields lived only inside the whole-document
// `snapshot`, a predicate compared a JSON field name to a literal. A Go field rename then
// compiled, passed every test, and stopped recovering every suspended operation without an
// error anywhere. A column is named in one place (journalColumnList) and its Go value in
// another (promotedColumns); a rename breaks the writer instead of silently matching nothing.
type journalColumns struct {
	State               string
	Phase               string
	CreatedAt           time.Time
	FinishedAt          *time.Time
	RetainUntil         time.Time
	CleanupAttemptedAt  *time.Time
	CleanedAt           *time.Time
	LeaseToken          string
	LeaseExpiresAt      *time.Time
	RootConnectionID    string
	RootGeneration      int64
	RootContentRoot     string
	RootSystemRoot      string
	RootCaseSensitive   bool
	RootRetainsVersions bool
	PurgePath           *string
	IntentTarget        *string
	StageSize           *int64
	StageHandle         []byte
	// EntryCount and EntriesLayout are immutable facts about the manifest, not
	// derived from the document, so divergence ignores them: they are written at
	// freeze and by the Claim-time upgrade, never by saveOperation.
	EntryCount    int
	EntriesLayout string
}

// journalColumnList names every promoted column once, in scan order. The migration that
// added them and the queries below refer to the same names.
const journalColumnList = `state,phase,created_at,finished_at,retain_until,cleanup_attempted_at,cleaned_at,lease_token,lease_expires_at,root_connection_id,root_generation,root_content_root,root_system_root,root_case_sensitive,root_retains_versions,purge_path,intent_target,stage_size,stage_handle,entry_count,entries_layout`

func journalColumnTargets(c *journalColumns) []any {
	return []any{
		&c.State, &c.Phase, &c.CreatedAt, &c.FinishedAt, &c.RetainUntil, &c.CleanupAttemptedAt,
		&c.CleanedAt, &c.LeaseToken, &c.LeaseExpiresAt, &c.RootConnectionID, &c.RootGeneration,
		&c.RootContentRoot, &c.RootSystemRoot, &c.RootCaseSensitive, &c.RootRetainsVersions,
		&c.PurgePath, &c.IntentTarget, &c.StageSize, &c.StageHandle,
		&c.EntryCount, &c.EntriesLayout,
	}
}

func scanJournalColumns(row pgx.Row, snapshot *[]byte, c *journalColumns) error {
	return row.Scan(append([]any{snapshot}, journalColumnTargets(c)...)...)
}

// intentTarget extracts the publication target an upload commit carries in its own typed
// intent. Only that kind has one: the older predicate was exactly `Intent.Target` and the
// only reader (the publication-reclaim query) filters action='upload_commit'. A move, copy or
// delete intent is an ActionRequest with From/To, which is not this field — storing one would
// invent a value no query asked for and make every pre-047 action row disagree with its
// document on upgrade.
func intentTarget(op Operation) *string {
	if op.Kind != "upload_commit" {
		return nil
	}
	var w storage.WriteIntent
	if json.Unmarshal(op.Intent, &w) != nil || w.Target == "" {
		return nil
	}
	s := string(w.Target)
	return &s
}

// promotedColumns derives the columns from the document. Both the INSERT and the UPDATE go
// through it, so the two write paths cannot drift from each other or from the struct above.
func promotedColumns(op Operation) journalColumns {
	c := journalColumns{
		State:               string(op.State),
		Phase:               string(op.Phase),
		CreatedAt:           op.CreatedAt,
		FinishedAt:          op.FinishedAt,
		RetainUntil:         op.RetainUntil,
		CleanupAttemptedAt:  op.CleanupAttemptedAt,
		CleanedAt:           op.CleanedAt,
		LeaseToken:          op.Lease.Token,
		RootConnectionID:    op.Root.ConnectionID,
		RootGeneration:      op.Root.Generation,
		RootContentRoot:     op.Root.ContentRoot,
		RootSystemRoot:      op.Root.SystemRoot,
		RootCaseSensitive:   op.Root.CaseSensitive,
		RootRetainsVersions: op.Root.RetainsVersions,
		IntentTarget:        intentTarget(op),
	}
	if op.Lease.Token != "" {
		expires := op.Lease.ExpiresAt
		c.LeaseExpiresAt = &expires
	}
	if op.Purge != nil {
		p := string(op.Purge.Path)
		c.PurgePath = &p
	}
	if op.Stage != nil {
		size := op.Stage.Size
		c.StageSize = &size
		c.StageHandle = op.Stage.Handle
	}
	return c
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func sameString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameInt(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// divergence reports which promoted columns disagree with the operation document. It is the
// detection for the one row shape this design cannot prevent: a row written by a pre-split
// binary (the window between `eam migrate` and the restart) or by corruption. The document
// is the operation; the columns are its index. A write path repairs the columns from the
// document, and a prune refuses to delete a disagreeing candidate, so the direction that
// could destroy work — "the column says terminal while the operation still runs" — is never
// acted on.
func divergence(op Operation, got journalColumns) []string {
	want := promotedColumns(op)
	var out []string
	add := func(name string, equal bool) {
		if !equal {
			out = append(out, name)
		}
	}
	add("state", got.State == want.State)
	add("phase", got.Phase == want.Phase)
	add("created_at", got.CreatedAt.Equal(want.CreatedAt))
	add("finished_at", sameTime(got.FinishedAt, want.FinishedAt))
	add("retain_until", got.RetainUntil.Equal(want.RetainUntil))
	add("cleanup_attempted_at", sameTime(got.CleanupAttemptedAt, want.CleanupAttemptedAt))
	add("cleaned_at", sameTime(got.CleanedAt, want.CleanedAt))
	add("lease_token", got.LeaseToken == want.LeaseToken)
	add("lease_expires_at", sameTime(got.LeaseExpiresAt, want.LeaseExpiresAt))
	add("root_connection_id", got.RootConnectionID == want.RootConnectionID)
	add("root_generation", got.RootGeneration == want.RootGeneration)
	add("root_content_root", got.RootContentRoot == want.RootContentRoot)
	add("root_system_root", got.RootSystemRoot == want.RootSystemRoot)
	add("root_case_sensitive", got.RootCaseSensitive == want.RootCaseSensitive)
	add("root_retains_versions", got.RootRetainsVersions == want.RootRetainsVersions)
	add("purge_path", sameString(got.PurgePath, want.PurgePath))
	add("intent_target", sameString(got.IntentTarget, want.IntentTarget))
	add("stage_size", sameInt(got.StageSize, want.StageSize))
	add("stage_handle", bytes.Equal(got.StageHandle, want.StageHandle))
	return out
}

// readOperationSnapshot reads one operation without locking its row. A read-only transaction
// cannot take `for update`, and a report of what the journal holds does not need to: the row it
// reads is the answer it returns. The disagreeing columns are returned rather than repaired
// because a ReadOnly transaction must not write; the caller reports them.
func readOperationSnapshot(ctx context.Context, q pgx.Tx, id string) (Operation, []string, error) {
	var op Operation
	var b []byte
	var columns journalColumns
	err := scanJournalColumns(q.QueryRow(ctx, `select snapshot,`+journalColumnList+` from file_operations where id=$1`, id), &b, &columns)
	if errors.Is(err, pgx.ErrNoRows) {
		return op, nil, E("not_found", nil)
	}
	if err != nil {
		return op, nil, err
	}
	if err = json.Unmarshal(b, &op); err != nil {
		return op, nil, err
	}
	applyEntryLayout(&op, columns)
	if err = canonicalOperation(&op); err != nil {
		return op, nil, err
	}
	diverged := divergence(op, columns)
	warnOperationDivergence(id, diverged)
	return op, diverged, nil
}

func readOperation(ctx context.Context, tx pgx.Tx, id string) (Operation, []string, error) {
	var op Operation
	var b []byte
	var columns journalColumns
	err := scanJournalColumns(tx.QueryRow(ctx, `select snapshot,`+journalColumnList+` from file_operations where id=$1 for update`, id), &b, &columns)
	if errors.Is(err, pgx.ErrNoRows) {
		return op, nil, E("not_found", nil)
	}
	if err != nil {
		return op, nil, err
	}
	err = json.Unmarshal(b, &op)
	if err == nil {
		applyEntryLayout(&op, columns)
		err = canonicalOperation(&op)
	}
	if err != nil {
		return op, nil, err
	}
	diverged := divergence(op, columns)
	warnOperationDivergence(id, diverged)
	return op, diverged, nil
}

// applyEntryLayout attaches the immutable layout facts and enforces the one rule that
// makes the column authoritative: a table-backed row's document carries no entries,
// so whatever a stale document spelled is discarded. Readers branch on the column,
// never on whether a document happens to have Entries (§2.1).
func applyEntryLayout(op *Operation, c journalColumns) {
	op.EntryCount = c.EntryCount
	op.EntriesLayout = EntriesLayout(c.EntriesLayout)
	if op.EntriesLayout == EntriesLayoutTable {
		op.Entries = nil
	}
}

func warnOperationDivergence(id string, diverged []string) {
	if len(diverged) == 0 {
		return
	}
	slog.Warn("operation journal columns disagree with the document; the next write repairs them",
		"operation", id, "columns", diverged)
}

// operationArgs is the single place where a Go operation becomes the promoted columns. The
// column names are named once here per statement, through NamedArgs, so a column inserted in
// the middle of the list cannot silently shift every value after it.
func operationArgs(op Operation, snapshot []byte) pgx.NamedArgs {
	c := promotedColumns(op)
	return pgx.NamedArgs{
		"id":                    op.ID,
		"snapshot":              snapshot,
		"state":                 c.State,
		"phase":                 c.Phase,
		"created_at":            c.CreatedAt,
		"finished_at":           c.FinishedAt,
		"retain_until":          c.RetainUntil,
		"cleanup_attempted_at":  c.CleanupAttemptedAt,
		"cleaned_at":            c.CleanedAt,
		"lease_token":           c.LeaseToken,
		"lease_expires_at":      c.LeaseExpiresAt,
		"root_connection_id":    c.RootConnectionID,
		"root_generation":       c.RootGeneration,
		"root_content_root":     c.RootContentRoot,
		"root_system_root":      c.RootSystemRoot,
		"root_case_sensitive":   c.RootCaseSensitive,
		"root_retains_versions": c.RootRetainsVersions,
		"purge_path":            c.PurgePath,
		"intent_target":         c.IntentTarget,
		"stage_size":            c.StageSize,
		"stage_handle":          c.StageHandle,
	}
}

// documentOf is the document a table-backed row must keep: its entries live in
// file_operation_entries, so marshalling the in-memory list (which settlement carries so
// the index commit can validate it) must never put it back. A document-backed row keeps
// its list exactly as before.
func documentOf(op Operation) Operation {
	if op.EntriesLayout == EntriesLayoutTable {
		op.Entries = nil
	}
	return op
}

// saveOperation writes the document and the promoted columns in one statement. One statement
// is the whole reason they cannot disagree for a row this binary wrote: there is no window in
// which a reader sees the new state in one place and the old state in the other.
func saveOperation(ctx context.Context, tx pgx.Tx, op Operation) error {
	b, err := json.Marshal(documentOf(op))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `update file_operations set
		snapshot=@snapshot,
		state=@state,
		phase=@phase,
		created_at=@created_at,
		finished_at=@finished_at,
		retain_until=@retain_until,
		cleanup_attempted_at=@cleanup_attempted_at,
		cleaned_at=@cleaned_at,
		lease_token=@lease_token,
		lease_expires_at=@lease_expires_at,
		root_connection_id=@root_connection_id,
		root_generation=@root_generation,
		root_content_root=@root_content_root,
		root_system_root=@root_system_root,
		root_case_sensitive=@root_case_sensitive,
		root_retains_versions=@root_retains_versions,
		purge_path=@purge_path,
		intent_target=@intent_target,
		stage_size=@stage_size,
		stage_handle=@stage_handle
		where id=@id`, operationArgs(op, b))
	return err
}

// readOnly runs fn in a read-only transaction that does not take the owner lock. `transaction`
// also orders one owner's writes, and a status read has nothing to order: it used to queue
// behind that owner's settlements, which is exactly the traffic a settling import produces —
// the queue's poller and the commit response both read an operation right after settling one,
// and every such read waited for the next settlement to finish.
//
// The access mode is ReadOnly rather than a comment, so a future edit that tries to write here
// fails at query time instead of quietly widening what a read may do.
func (p *Postgres) readOnly(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
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

// ownerFileActivitySQL covers every row that could describe content this owner
// already held. A missing budget plus any of these rows means the automatic
// empty-owner path cannot prove the accounting and the operator must admit it.
const ownerFileActivitySQL = `select exists(select 1 from nodes where owner_id=$1)
	or exists(select 1 from file_operations where owner_id=$1)
	or exists(select 1 from file_uploads where owner_id=$1)
	or exists(select 1 from file_reservations where owner_id=$1)
	or exists(select 1 from file_entry_charges where owner_id=$1)
	or exists(select 1 from file_content_generations where owner_id=$1)
	or exists(select 1 from file_action_bindings where owner_id=$1)
	or exists(select 1 from file_credit_claims where owner_id=$1)`

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func ownerHasFileActivity(ctx context.Context, q rowQuerier, ownerID string) (bool, error) {
	var active bool
	err := q.QueryRow(ctx, ownerFileActivitySQL, ownerID).Scan(&active)
	return active, err
}

// HasFileActivity is the cheap precondition for automatic empty-owner admission.
// It is not a snapshot or a writer fence; AdmitEmptyOwner re-checks under the
// owner lock before inserting anything.
func (p *Postgres) HasFileActivity(ctx context.Context, root Root) (bool, error) {
	if err := validateRoot(root); err != nil {
		return false, err
	}
	return ownerHasFileActivity(ctx, p.pool, root.OwnerID)
}

// AdmitEmptyOwner establishes the zero baseline for an owner that has never held a
// file. It is the only automatic initialization: the caller has already proved the
// exact native root empty and no retained object generation possible. Any prior
// index, operation, upload, reservation, charge or generation row leaves the owner
// on the operator-admitted path, so a restore or missing baseline can never be
// papered over with a zero count.
func (p *Postgres) AdmitEmptyOwner(ctx context.Context, root Root, logicalLimit, physicalLimit int64) error {
	if err := validateRoot(root); err != nil {
		return err
	}
	if logicalLimit < 0 || physicalLimit < 0 || logicalLimit > 9007199254740991 || physicalLimit > 9007199254740991 {
		return E("invalid_request", nil)
	}
	evidence, err := canonicalJSON(json.RawMessage(`{"version":1,"reason":"empty-owner-provisioning"}`))
	if err != nil {
		return err
	}
	base := Baseline{LogicalLimit: logicalLimit, PhysicalLimit: physicalLimit, NativeComplete: true, IndexCompared: true, LegacyWritersDrained: true, Evidence: evidence}
	return p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error {
		var stored []byte
		err := tx.QueryRow(ctx, `select root_binding from file_budgets where owner_id=$1 for update`, root.OwnerID).Scan(&stored)
		if err == nil {
			// An operator may have admitted this owner first. Never replace a real
			// baseline with a zero one; only confirm the binding is still ours.
			var prior Root
			if err = json.Unmarshal(stored, &prior); err != nil {
				return err
			}
			if prior != root {
				return E("root_conflict", nil)
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		active, err := ownerHasFileActivity(ctx, tx, root.OwnerID)
		if err != nil {
			return err
		}
		if active {
			return E("baseline_required", nil)
		}
		rb, _ := json.Marshal(root)
		bb, _ := json.Marshal(base)
		_, err = tx.Exec(ctx, `insert into file_budgets(owner_id,root_binding,baseline,logical_bytes,physical_bytes,logical_limit,physical_limit) values($1,$2,$3,0,0,$4,$5)`, root.OwnerID, rb, bb, base.LogicalLimit, base.PhysicalLimit)
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
	var replayed bool
	err = p.transaction(ctx, req.Root.OwnerID, func(tx pgx.Tx) error {
		var err error
		op, replayed, err = p.beginTx(ctx, tx, req, fingerprint, reserve, true)
		return err
	})
	return op, replayed, err
}

// guardHolder returns the operation whose durable guard conflicts with one of want, or
// nil when the paths are free. self names the asking operation, whose own rows never
// conflict with it.
//
// A guard row held by an `open` upload session is skipped. Sessions take their guard when
// they freeze (see freezeUpload), so such a row can only belong to a session opened before
// that rule: it has no native effect behind it and fences nothing, exactly like every open
// session since. The row disappears when its session freezes, ends or lapses, which the
// seven-day session lifetime bounds; after that this exclusion matches nothing.
func guardHolder(ctx context.Context, tx pgx.Tx, root Root, kind string, want []Guard, self string) (*Error, error) {
	var own any
	if self != "" {
		own = self
	}
	rows, err := tx.Query(ctx, `select g.native_path,g.subtree,o.action,o.id::text from file_operation_guards g
		join file_operations o on o.id=g.operation_id and o.owner_id=g.owner_id
		where g.owner_id=$1 and ($2::uuid is null or g.operation_id<>$2::uuid)
		  and not exists(select 1 from file_uploads u where u.operation_id=g.operation_id and u.owner_id=g.owner_id and u.state='open')
		order by o.id,g.ordinal`, root.OwnerID, own)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var holder *Error
	for rows.Next() {
		var g Guard
		var held, operationID string
		if err = rows.Scan(&g.Path, &g.Subtree, &held, &operationID); err != nil {
			return nil, err
		}
		if held == "move" {
			g.Subtree = true
		}
		for _, w := range want {
			if kind == "move" {
				w.Subtree = true
			}
			if holder == nil && guardConflict(root, g, w) {
				holder = &Error{Code: "operation_in_progress", OperationID: operationID, OperationKind: held}
			}
		}
	}
	return holder, rows.Err()
}

// insertGuards makes an operation's declared guards durable. A row that already exists is
// kept: it is this operation's own, written by an earlier attempt of the same transition.
func insertGuards(ctx context.Context, tx pgx.Tx, op Operation) error {
	for i, g := range op.Guards {
		if _, err := tx.Exec(ctx, `insert into file_operation_guards(operation_id,owner_id,ordinal,native_path,comparison_path,subtree) values($1,$2,$3,$4,$5,$6)
			on conflict(operation_id,ordinal) do nothing`, op.ID, op.Root.OwnerID, i, string(g.Path), comparePath(op.Root, g.Path), g.Subtree); err != nil {
			return err
		}
	}
	return nil
}

// insertOperationSQL is the one statement that creates a journal row, so the promoted
// columns and the document are always written together from operationArgs. A test that needs
// a row shaped exactly like a real one uses it too, instead of rebuilding the column list.
//
// entry_count is not part of operationArgs: it is set once here from the manifest the
// admission carries and once by FreezeAction, and never re-derived by saveOperation, so a
// row written by a binary that keeps entries in the table cannot have its count zeroed by a
// document write that carries none. entries_layout is set explicitly to 'table' (Release B):
// the column default stays 'document' so that a writer which forgets the column produces a
// complete, readable document rather than a row with no entries anywhere. The default is a
// safety net, not the writer's answer.
const insertOperationSQL = `insert into file_operations(
	id,owner_id,principal_id,action,idempotency_key,fingerprint,snapshot,
	state,phase,created_at,finished_at,retain_until,cleanup_attempted_at,cleaned_at,
	lease_token,lease_expires_at,root_connection_id,root_generation,root_content_root,
	root_system_root,root_case_sensitive,root_retains_versions,purge_path,intent_target,
	stage_size,stage_handle,entry_count,entries_layout)
	values(@id,@owner_id,@principal_id,@action,@idempotency_key,@fingerprint,@snapshot,
	@state,@phase,@created_at,@finished_at,@retain_until,@cleanup_attempted_at,@cleaned_at,
	@lease_token,@lease_expires_at,@root_connection_id,@root_generation,@root_content_root,
	@root_system_root,@root_case_sensitive,@root_retains_versions,@purge_path,@intent_target,
	@stage_size,@stage_handle,@entry_count,@entries_layout)`

// beginTx shares admission with upload sessions under the same owner lock and transaction.
//
// fence decides whether the guards become durable now. Every action and the simple PUT
// fence at admission, because their native effect follows in the same request. An upload
// session does not: it can publish only at commit, hours later, so it is admitted against
// the current guards and takes its own when it freezes.
func (p *Postgres) beginTx(ctx context.Context, tx pgx.Tx, req BeginRequest, fingerprint string, reserve int64, fence bool) (Operation, bool, error) {
	var op Operation
	replayed := false
	err := func() error {
		// Replay precedes quota/conflict checks and does not require live native ancestry.
		if req.IdempotencyKey != "" {
			var id string
			err := tx.QueryRow(ctx, `select id::text from file_operations where owner_id=$1 and principal_id=$2 and action=$3 and idempotency_key=$4`, req.Root.OwnerID, req.Actor.ID, req.Kind, req.IdempotencyKey).Scan(&id)
			if err == nil {
				op, _, err = readOperation(ctx, tx, id)
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
		holder, err := guardHolder(ctx, tx, req.Root, req.Kind, req.Guards, "")
		if err != nil {
			return err
		}
		if holder != nil {
			return holder
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
		op = Operation{Attribution: req.Actor, ID: newID(), Root: req.Root, PrincipalID: req.Actor.ID, Kind: req.Kind, State: Queued, Phase: Admitted, Intent: req.Intent, Fingerprint: fingerprint, Guards: req.Guards, CreatedAt: now, RetainUntil: now.Add(retention), Reservation: req.Reservation, EntriesLayout: EntriesLayoutTable}
		for _, e := range req.Manifest {
			op.Entries = append(op.Entries, EntryEffect{ManifestEntry: e, State: "pending"})
		}
		b, err := json.Marshal(documentOf(op))
		if err != nil {
			return err
		}
		var key any
		if req.IdempotencyKey != "" {
			key = req.IdempotencyKey
		}
		args := operationArgs(op, b)
		args["owner_id"] = req.Root.OwnerID
		args["principal_id"] = req.Actor.ID
		args["action"] = req.Kind
		args["idempotency_key"] = key
		args["fingerprint"] = fingerprint
		// The count is set here because admission is one of the two moments a manifest can
		// appear (the other is FreezeAction); entries never change after that.
		args["entry_count"] = len(op.Entries)
		args["entries_layout"] = string(op.EntriesLayout)
		_, err = tx.Exec(ctx, insertOperationSQL, args)
		if err != nil {
			return err
		}
		// Release B: the table is the entry record, written with the parent in this
		// transaction and under the owner lock, so a manifest can never be admitted
		// without its rows (see postgres_entries.go).
		if err = writeEntries(ctx, tx, op, op.Entries); err != nil {
			return err
		}
		if fence {
			if err = insertGuards(ctx, tx, op); err != nil {
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
	}()
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
	err := p.readOnly(ctx, func(tx pgx.Tx) error {
		var err error
		op, _, err = readOperationSnapshot(ctx, tx, id)
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
	var repaired []string
	err := p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error {
		var err error
		var diverged []string
		op, diverged, err = readOperation(ctx, tx, id)
		if err != nil {
			return err
		}
		if len(diverged) > 0 {
			// A row written before the column split (the deploy window between `eam migrate`
			// and the restart) has a newer document than columns. Rewrite the columns from the
			// document and commit that repair; nothing is claimed on this call. A terminal
			// operation must leave the pending index here — returning the usual
			// invalid_transition would roll the repair back and let the sweep select the same
			// settled row on every pass, forever. A non-terminal row is repaired and then
			// claimed normally, because its document is what the worker acts on.
			if err = saveOperation(ctx, tx, op); err != nil {
				return err
			}
			if op.Terminal() {
				repaired = diverged
				return nil
			}
		}
		if err = authorizeOperation(op, root, a, true); err != nil {
			return err
		}
		if op.Terminal() {
			return E("invalid_transition", nil)
		}
		// A 'document' row is upgraded on its first Claim: this binary cannot transition
		// entries it keeps in a document, and the upgrade is what lets an operation from
		// before Release B (or from the deploy window) settle normally (§2.5). It reads the
		// document's list, which is the authority for that layout, overwrites any shadow
		// Release A left behind, and flips the layout in this transaction.
		if err = upgradeEntries(ctx, tx, &op); err != nil {
			return err
		}
		var uploadState string
		err = tx.QueryRow(ctx, `select state from file_uploads where operation_id=$1 and owner_id=$2`, op.ID, root.OwnerID).Scan(&uploadState)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && uploadState != string(UploadCommitting) {
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
		uncertain := false
		if op.EntryCount > 0 {
			if uncertain, err = uncertainEntriesExist(ctx, tx, op); err != nil {
				return err
			}
		}
		if op.Phase == EffectStarted && (op.EntryCount == 0 || uncertain) {
			op.State = Unknown
		} else if op.State != Unknown {
			op.State = Running
		}
		return saveOperation(ctx, tx, op)
	})
	if err == nil && len(repaired) > 0 {
		return op, E("journal_repaired", fmt.Errorf("operation %s had columns disagreeing in %s; repaired from the document, nothing to claim", id, strings.Join(repaired, ",")))
	}
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
		op, _, err := readOperation(ctx, tx, l.OperationID)
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
		op, _, err = readOperation(ctx, tx, id)
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
