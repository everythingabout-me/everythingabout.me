package fileops

// Release B of docs/design/journal-entries.md: file_operation_entries is the operation's
// per-entry record, and the whole document no longer carries the list.
//
// This file owns every SQL statement that touches the table. That is deliberate: the read
// guard it replaces (Release A's TestJournalEntriesTableIsWriteOnly) existed to keep the
// table write-only, and the risk Release B introduces is the opposite one — the settlement
// load being hoisted out of the transaction that holds the parent row lock (§2.3, §2.7).
// Keeping the table in one file lets the inverted guard assert two textual facts instead:
// the settlement loader is called from Settle, and it is not called from the file that
// implements Get/readOperation.
//
// Why a row per entry: every entry has its own durable state (pending -> started ->
// applied/not_applied/outcome_unknown) written several times, and every write of one
// entry's state must not rewrite the other 10,001. The measured volume of the old shape is
// in §1.2; Release B removes it from the document, not from the journal's contract.
//
// Atomicity: every write here runs inside the transaction that already holds the parent row
// lock (`leased` -> readOperation ... FOR UPDATE) except the two admission paths that run
// under the owner advisory lock and insert the parent in the same transaction. The parent
// lock, not the entry row, is the serialization: an entry row must never be written outside
// a transaction that already holds the parent row lock.
//
// Forward-only: a binary from before this release cannot read a table-backed operation
// (see EntriesLayout in types.go and docs/design/journal-entries.md §2.6).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"

	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

// entryQuerier is the read side a table-backed operation needs. pgx.Tx and
// pgxpool.Pool both satisfy it, so the same loader serves a settlement
// transaction and the read-only progress/list calls.
type entryQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// entryColumnList names every column once, in the order entryRow scans them. The
// writer below declares the same columns in jsonb_to_recordset, so a column added
// to the table without a reader fails the round-trip tests rather than going NULL.
const entryColumnList = `number, action, source_path, target_path, source_revision,
	source_entry, target_absent, target_revision, size, state, checkpoint, evidence, result`

// writeEntriesSQL writes or updates the given entries of one operation. It takes the
// entries as one JSON array and declares every column in `jsonb_to_recordset`, so a
// missing or renamed key is a NOT NULL violation here instead of a silently NULL
// column: the caller cannot drift from the table without failing the transition.
//
// `on conflict do update` makes it idempotent. Both callers need that: FreezeAction
// and admission insert the whole manifest at once, and the Claim-time upgrade
// (upgradeEntries) overwrites a 'document' row's shadow from the document, which is
// why insert-and-ignore would be wrong — a shadow written by Release A may be stale
// after a transition that took the document path.
const writeEntriesSQL = `insert into file_operation_entries
	(owner_id, operation_id, number, action, source_path, target_path, source_revision,
	 source_entry, target_absent, target_revision, size, state, checkpoint, evidence, result)
select $1::uuid, $2::uuid, r.number, r.action, r.source_path, r.target_path,
       case when r.source_revision is null then null else decode(r.source_revision, 'base64') end,
       nullif(r.source_entry, 'null'::jsonb), r.target_absent,
       case when r.target_revision is null then null else decode(r.target_revision, 'base64') end,
       r.size, r.state,
       case when r.checkpoint is null then null else decode(r.checkpoint, 'base64') end,
       case when r.evidence is null then null else decode(r.evidence, 'base64') end,
       nullif(r.result, 'null'::jsonb)
  from jsonb_to_recordset($3::jsonb) as r(
       number integer, action text, source_path text, target_path text,
       source_revision text, source_entry jsonb, target_absent boolean,
       target_revision text, size bigint, state text, checkpoint text,
       evidence text, result jsonb)
on conflict (operation_id, number) do update set
       action = excluded.action,
       source_path = excluded.source_path,
       target_path = excluded.target_path,
       source_revision = excluded.source_revision,
       source_entry = excluded.source_entry,
       target_absent = excluded.target_absent,
       target_revision = excluded.target_revision,
       size = excluded.size,
       state = excluded.state,
       checkpoint = excluded.checkpoint,
       evidence = excluded.evidence,
       result = excluded.result,
       updated_at = clock_timestamp()`

// selectEntriesSQL is the ordered load. One statement for the whole manifest is the
// point of the row layout; ordering by `number` reproduces the document's order, so
// every consumer that sorted or iterated the list keeps the same behaviour.
const selectEntriesSQL = `select ` + entryColumnList + `
  from file_operation_entries
 where operation_id=$1 and owner_id=$2
 order by number`

const selectEntrySQL = `select ` + entryColumnList + `
  from file_operation_entries
 where operation_id=$1 and owner_id=$2 and number=$3`

// The per-transition questions are predicates on one row, not loads of the list.
// Every one of them runs under the parent row lock the caller already holds.
const (
	uncertainEntriesSQL = `select exists(select 1 from file_operation_entries
		where operation_id=$1 and owner_id=$2 and state in ('started','outcome_unknown'))`
	anyAppliedEntrySQL = `select exists(select 1 from file_operation_entries
		where operation_id=$1 and owner_id=$2 and state='applied')`
	unfinishedEntrySQL = `select exists(select 1 from file_operation_entries
		where operation_id=$1 and owner_id=$2 and state <> 'applied')`
	entryProgressSQL = `select count(*) from file_operation_entries
		where operation_id=$1 and owner_id=$2 and state in ('applied','not_applied')`
)

// beginEntrySQL claims one pending entry. 0 rows means the entry is not pending —
// something else claimed it, it is already terminal, or the row is missing — and the
// caller answers recovery_required rather than issuing a second effect.
const selectAppliedEntriesSQL = `select ` + entryColumnList + `
  from file_operation_entries
 where operation_id=$1 and owner_id=$2 and state='applied'
 order by number`

const beginEntrySQL = `update file_operation_entries
   set state='started', updated_at=clock_timestamp()
 where operation_id=$1 and owner_id=$2 and number=$3 and state='pending'`

// saveEntrySQL records the outcome or checkpoint of one entry. It is an UPDATE, not
// an upsert: the frozen manifest is the only writer of rows, so a missing row here is
// a missing manifest entry and must fail rather than be re-created from a transition.
const saveEntrySQL = `update file_operation_entries
   set state=$4, checkpoint=$5, evidence=$6, result=$7, updated_at=clock_timestamp()
 where operation_id=$1 and owner_id=$2 and number=$3`

// flipEntriesLayoutSQL is the only statement that changes a row's layout. The
// upgrade writes it in the same transaction as the rows it copied from the
// document, so a crash can never leave a 'table' row without its entries.
const flipEntriesLayoutSQL = `update file_operations
   set entries_layout='table', entry_count=$3
 where id=$1 and owner_id=$2`

// shadowEntry is the JSON shape writeEntriesSQL declares. The keys are the SQL
// names, not the operation document's Go field names: this is a transport struct for
// one INSERT, and the SQL above is the other half of it.
type shadowEntry struct {
	Number         int             `json:"number"`
	Action         string          `json:"action"`
	SourcePath     string          `json:"source_path"`
	TargetPath     string          `json:"target_path"`
	SourceRevision *string         `json:"source_revision"`
	SourceEntry    json.RawMessage `json:"source_entry"`
	TargetAbsent   bool            `json:"target_absent"`
	TargetRevision *string         `json:"target_revision"`
	Size           int64           `json:"size"`
	State          string          `json:"state"`
	Checkpoint     *string         `json:"checkpoint"`
	Evidence       *string         `json:"evidence"`
	Result         json.RawMessage `json:"result"`
}

// entryBytes is the table's spelling of a nullable byte slice: standard base64, the
// same spelling the document used, so a value read back through either path is the
// same.
func entryBytes(b []byte) *string {
	if len(b) == 0 {
		return nil
	}
	s := base64.StdEncoding.EncodeToString(b)
	return &s
}

// entryJSON turns an optional JSON value into the column's value. The document
// spells an absent value as JSON null; a jsonb column should hold SQL NULL for it, so
// both spellings read back as "no value".
func entryJSON(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	return raw
}

func shadowEntryOf(e EntryEffect) (shadowEntry, error) {
	row := shadowEntry{
		Number:         e.Number,
		Action:         e.Action,
		SourcePath:     string(e.Source),
		TargetPath:     string(e.Target),
		SourceRevision: entryBytes(e.SourceRevision),
		TargetAbsent:   e.TargetConditions.Absent,
		TargetRevision: entryBytes(e.TargetConditions.ExpectedRevision),
		Size:           e.Size,
		State:          e.State,
		Checkpoint:     entryBytes(e.Checkpoint),
		Evidence:       entryBytes(e.Evidence),
		Result:         entryJSON(e.Result),
	}
	if e.SourceEntry != nil {
		b, err := json.Marshal(e.SourceEntry)
		if err != nil {
			return row, err
		}
		row.SourceEntry = b
	}
	return row, nil
}

// writeEntries writes or updates the given entries of one operation. It is called
// with the whole manifest at freeze, admission and the Claim-time upgrade, and with
// a single entry from no path — per-entry transitions use the row UPDATEs above so
// they never rewrite a sibling.
func writeEntries(ctx context.Context, tx pgx.Tx, op Operation, entries []EntryEffect) error {
	if len(entries) == 0 {
		return nil
	}
	rows := make([]shadowEntry, 0, len(entries))
	for _, e := range entries {
		row, err := shadowEntryOf(e)
		if err != nil {
			return err
		}
		rows = append(rows, row)
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, writeEntriesSQL, op.Root.OwnerID, op.ID, b)
	return err
}

// entryRow is one scanned row, before it becomes an EntryEffect.
type entryRow struct {
	Number         int
	Action         string
	SourcePath     string
	TargetPath     string
	SourceRevision []byte
	SourceEntry    []byte
	TargetAbsent   bool
	TargetRevision []byte
	Size           int64
	State          string
	Checkpoint     []byte
	Evidence       []byte
	Result         []byte
}

func (r entryRow) effect() (EntryEffect, error) {
	e := EntryEffect{
		ManifestEntry: ManifestEntry{
			Number:         r.Number,
			Action:         r.Action,
			Source:         storage.Path(r.SourcePath),
			Target:         storage.Path(r.TargetPath),
			Size:           r.Size,
			SourceRevision: r.SourceRevision,
			TargetConditions: storage.Conditions{
				Absent:           r.TargetAbsent,
				ExpectedRevision: r.TargetRevision,
			},
		},
		State:      r.State,
		Checkpoint: r.Checkpoint,
		Evidence:   r.Evidence,
	}
	if len(r.SourceEntry) > 0 && !bytes.Equal(r.SourceEntry, []byte("null")) {
		var entry storage.Entry
		if err := json.Unmarshal(r.SourceEntry, &entry); err != nil {
			return EntryEffect{}, err
		}
		e.SourceEntry = &entry
	}
	if len(r.Result) > 0 && !bytes.Equal(r.Result, []byte("null")) {
		e.Result = json.RawMessage(bytes.Clone(r.Result))
	}
	return e, nil
}

func scanEntryRow(row pgx.Row) (EntryEffect, error) {
	var r entryRow
	if err := row.Scan(&r.Number, &r.Action, &r.SourcePath, &r.TargetPath,
		&r.SourceRevision, &r.SourceEntry, &r.TargetAbsent, &r.TargetRevision,
		&r.Size, &r.State, &r.Checkpoint, &r.Evidence, &r.Result); err != nil {
		return EntryEffect{}, err
	}
	return r.effect()
}

// loadEntries is the one ordered load of an operation's manifest. The settlement
// keeps it inside its transaction and after the parent row lock (§2.3); see
// TestSettlementLoadsEntriesUnderParentLock.
func loadEntries(ctx context.Context, q entryQuerier, op Operation) ([]EntryEffect, error) {
	rows, err := q.Query(ctx, selectEntriesSQL, op.ID, op.Root.OwnerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]EntryEffect, 0, op.EntryCount)
	for rows.Next() {
		entry, err := scanEntryRow(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// loadEntry reads one entry by number. It is the per-transition read that replaces
// the old scan of the whole document (§2.4): one row, under the parent lock the
// caller's transition already holds in the write paths.
// appliedEntries returns only the applied rows, which is all the aggregate settlement
// result needs. It is not a load of the manifest.
func appliedEntries(ctx context.Context, tx pgx.Tx, op Operation) ([]EntryEffect, error) {
	if op.EntriesLayout != EntriesLayoutTable {
		applied := make([]EntryEffect, 0, len(op.Entries))
		for _, entry := range op.Entries {
			if entry.State == "applied" {
				applied = append(applied, entry)
			}
		}
		return applied, nil
	}
	rows, err := tx.Query(ctx, selectAppliedEntriesSQL, op.ID, op.Root.OwnerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var applied []EntryEffect
	for rows.Next() {
		entry, err := scanEntryRow(rows)
		if err != nil {
			return nil, err
		}
		applied = append(applied, entry)
	}
	return applied, rows.Err()
}

func loadEntry(ctx context.Context, q entryQuerier, op Operation, number int) (EntryEffect, error) {
	entry, err := scanEntryRow(q.QueryRow(ctx, selectEntrySQL, op.ID, op.Root.OwnerID, number))
	if errors.Is(err, pgx.ErrNoRows) {
		return EntryEffect{}, E("not_found", nil)
	}
	return entry, err
}

// Entries returns an operation's manifest entries. For a document-backed row it is
// the document's list the caller already holds; for a table-backed row it is one
// ordered query. Callers that are not settlement must not treat this as authority:
// the settlement reloads under its own lock (§2.7).
func (p *Postgres) Entries(ctx context.Context, op Operation) ([]EntryEffect, error) {
	if op.EntriesLayout != EntriesLayoutTable {
		return op.Entries, nil
	}
	return loadEntries(ctx, p.pool, op)
}

// Entry returns one entry of an operation without loading the rest.
func (p *Postgres) Entry(ctx context.Context, op Operation, number int) (EntryEffect, error) {
	if op.EntriesLayout != EntriesLayoutTable {
		for _, entry := range op.Entries {
			if entry.Number == number {
				return entry, nil
			}
		}
		return EntryEffect{}, E("not_found", nil)
	}
	return loadEntry(ctx, p.pool, op, number)
}

// EntryProgress reports how many entries are decided, for the status endpoint. It is
// a count over one operation's rows, never a load of them: the progress report is
// parsed on every client poll, and the old shape loaded the whole document to count.
func (p *Postgres) EntryProgress(ctx context.Context, op Operation) (int, error) {
	if op.EntriesLayout != EntriesLayoutTable {
		completed := 0
		for _, entry := range op.Entries {
			if entry.State == "applied" || entry.State == "not_applied" {
				completed++
			}
		}
		return completed, nil
	}
	var completed int
	err := p.pool.QueryRow(ctx, entryProgressSQL, op.ID, op.Root.OwnerID).Scan(&completed)
	return completed, err
}

// The predicate helpers below are used by the transition transactions. They take the
// transaction the caller already holds the parent row lock on, so a predicate and the
// write that follows cannot be separated by a concurrent transition.
func uncertainEntriesExist(ctx context.Context, tx pgx.Tx, op Operation) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, uncertainEntriesSQL, op.ID, op.Root.OwnerID).Scan(&exists)
	return exists, err
}

func anyAppliedEntry(ctx context.Context, tx pgx.Tx, op Operation) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, anyAppliedEntrySQL, op.ID, op.Root.OwnerID).Scan(&exists)
	return exists, err
}

func unfinishedEntryExists(ctx context.Context, tx pgx.Tx, op Operation) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, unfinishedEntrySQL, op.ID, op.Root.OwnerID).Scan(&exists)
	return exists, err
}

// upgradeEntries turns a 'document' row into a 'table' row on its first Claim. It is
// the only path that flips the layout during normal operation (§2.5): the document's
// list is the authority for that layout, so it is read from the already-decoded
// operation and written with `on conflict do update`, which repairs a stale Release A
// shadow instead of preserving it. Terminal rows are never upgraded — they cannot
// take another transition, and prune removes them with their document.
func upgradeEntries(ctx context.Context, tx pgx.Tx, op *Operation) error {
	if op.EntriesLayout != EntriesLayoutDocument {
		return nil
	}
	entries := op.Entries
	if err := writeEntries(ctx, tx, *op, entries); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, flipEntriesLayoutSQL, op.ID, op.Root.OwnerID, len(entries)); err != nil {
		return err
	}
	op.EntriesLayout = EntriesLayoutTable
	op.EntryCount = len(entries)
	op.Entries = nil
	return nil
}
