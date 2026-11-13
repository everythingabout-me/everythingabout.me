package fileops

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// Release A shadow of the operation journal's per-entry state (docs/design/journal-entries.md
// §2.6). While a row is entries_layout='document', every entry transition writes its row to
// file_operation_entries *as well as* keeping the entry inside the operation document, and the
// document stays complete and authoritative: nothing in this release reads the table, and
// settlement, progress, recovery and prune are untouched.
//
// Why the shadow exists: Release B flips entries_layout to 'table' and stops carrying Entries
// in documents. Without a shadow, that flip would need a backfill window in which no operation
// is created and no entry transitions — an operational stop of the journal. With it, every
// operation created after Release A is already in the table and every entry transition after
// the deploy is already applied there, so the flip is a per-row flag plus one Claim-time
// upgrade for old rows (design §2.5), not a migration that has to catch up with live traffic.
//
// What it costs: one extra row write per entry transition, and one bulk insert when the
// manifest freezes. The row is small (a path pair, a size, a state and a base64 revision /
// checkpoint / evidence / result — under 1 KB for a typical photo entry), so for one release
// the journal's per-transition database work grows by roughly the size of one row next to a
// document rewrite that is already megabytes on the operation this table exists for. Phase 1
// measured that: see the measured shadow cost in the design doc's §4. The volume is one
// release long, which is the other reason Release A must not be kept for long.
//
// Atomicity: every function here writes inside the transaction that already holds the parent
// row lock (`leased` -> readOperation ... FOR UPDATE, or beginTx under the owner lock), and
// saveOperation writes the document in that same transaction. A commit produces both or
// neither, so a crash cannot leave a table row whose document says something else. The pairing
// is a property of the call sites: postgres_transitions.go calls shadowWriteEntry from the two
// entry transitions (BeginEntry, SaveEntry), postgres_actions.go calls shadowWriteEntries when
// the manifest freezes, and postgres.go calls it at admission. A new entry-mutating path must
// do the same, which TestShadowEntriesMatchDocument catches the day it is added.
//
// Disagreement detection: the exact detector is TestShadowEntriesMatchDocument, which rebuilds
// every entry from the table and compares it to the document with `is distinct from` after a
// real Prepare + Run. sql/050's header has the gross detector (row count vs entry_count) and
// the interruption story.
//
// Read guard: TestJournalEntriesTableIsWriteOnly fails if any production Go source other than
// this file names the table, or if any production source reads it (`from
// file_operation_entries`). Release B deletes that guard together with the layout flag.
//
// The layout is not checked at write time. This binary is the one that writes 'document' rows,
// the column's default, and Release B's writer replaces this one; testing it here would read
// the column on every transition to confirm a value this code cannot change, and would be the
// first reader switch of a release that deliberately has none.

// shadowEntriesSQL is the only write of file_operation_entries. It takes the entries as one
// JSON array and declares every column of the row in `jsonb_to_recordset`, so a missing or
// renamed key is a NOT NULL violation here instead of a silently NULL column: the caller
// cannot drift from the table without failing the transition. `on conflict do update` makes it
// idempotent: a replayed transition rewrites the row it already wrote, and Release B's
// Claim-time upgrade overwrites a 'document' row's shadow from the document (§2.5), which is
// why an insert-and-ignore would be wrong.
const shadowEntriesSQL = `insert into file_operation_entries
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

// shadowEntry is the JSON shape shadowEntriesSQL declares. The keys are the SQL names, not the
// operation document's Go field names: this is a transport struct for one INSERT, and the SQL
// above is the other half of it.
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

// shadowBytes is the shadow's spelling of a nullable byte slice: the document spells it as
// standard base64, and so does this, so a value read back through either path is the same.
func shadowBytes(b []byte) *string {
	if len(b) == 0 {
		return nil
	}
	s := base64.StdEncoding.EncodeToString(b)
	return &s
}

// shadowJSON turns an optional JSON value into the column's value. The document spells an
// absent value as JSON null; a jsonb column should hold SQL NULL for it, so both spellings read
// back as "no value".
func shadowJSON(raw json.RawMessage) json.RawMessage {
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
		SourceRevision: shadowBytes(e.SourceRevision),
		TargetAbsent:   e.TargetConditions.Absent,
		TargetRevision: shadowBytes(e.TargetConditions.ExpectedRevision),
		Size:           e.Size,
		State:          e.State,
		Checkpoint:     shadowBytes(e.Checkpoint),
		Evidence:       shadowBytes(e.Evidence),
		Result:         shadowJSON(e.Result),
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

// shadowWriteEntries writes or updates the given entries of one operation. It is called with
// the whole manifest at freeze and admission, and with a single entry from each per-entry
// transition; both go through the same statement so the two cannot describe different rows.
func shadowWriteEntries(ctx context.Context, tx pgx.Tx, op Operation, entries []EntryEffect) error {
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
	_, err = tx.Exec(ctx, shadowEntriesSQL, op.Root.OwnerID, op.ID, b)
	return err
}

// shadowWriteEntry is the per-transition form: one row, still through the same statement.
func shadowWriteEntry(ctx context.Context, tx pgx.Tx, op Operation, e EntryEffect) error {
	return shadowWriteEntries(ctx, tx, op, []EntryEffect{e})
}
