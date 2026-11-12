# Legacy upload tables: liveness audit and retirement plan

> **Status: audit and plan only.** This document deletes no code, row or table. It records which tables
> track an upload or its parts, which of them are still reachable, and the exact conditions and order under
> which the dead ones may be retired. `fs_jobs` (owned by another branch) and the handler packages (a third
> branch) are out of scope and were not changed.
>
> It lives here rather than as a section of [transfer-protocol.md](transfer-protocol.md) because that file
> states the protocol every client and driver must implement, while this is a one-time audit of the
> implementation that protocol replaced. Its conclusion is a deployment gate (drain, remove readers, then
> drop), not a transfer contract; mixing the two would make a settled contract look open.

## 1. Inventory

Applying all 46 migrations with the real runner to a disposable database and listing the schema (`\dt`)
returns 51 tables. Exactly six exist to track an upload or its parts:

| Table | Migration | Generation | What a row is |
|---|---|---|---|
| `upload_sessions` | `006_upload_sessions.sql` | 1 | a presigned single/multipart upload straight to COS, completed by client-reported ETags |
| `upload_index_watches` | `018_upload_index_watches.sql` (+`021`) | 2 | an index observation owed for a legacy direct-upload grant whose browser confirmation may be lost |
| `browser_uploads` | `022_browser_uploads.sql` | 3 | a server-owned browser upload intent, with a checkpoint and publish/abort state machine |
| `file_uploads` | `024_file_uploads.sql` (+`033`, `038`, `039`) | 4 | a durable provider-neutral upload session, keyed by its `file_operations` id |
| `file_upload_parts` | `024_file_uploads.sql` | 4 | one immutable class C part receipt (size, SHA-256, spool handle, ACK sequence) |
| `file_upload_native_metadata` | `025_native_upload_metadata.sql` | 4 | class A native multipart allocation/part identities shared with independent byte receivers |

The review's count is exact: **four generations, six tables.** Generations 1–3 are the legacy designs;
generation 4 is the current journal and is three tables because a class C receipt, a class A native
allocation and the session itself have different lifetimes.

Adjacent tables were examined and excluded because they are not upload-tracking tables:

- `file_operations`, `file_operation_guards`, `file_operation_checkpoints`, `file_action_bindings` — the
  operation journal shared by mkdir, move, copy, delete and `upload_commit`; an upload is one `action`.
  Live for every mutation.
- `file_reservations`, `file_budgets`, `file_credit_claims`, `file_entry_charges` — quota accounting keyed
  by an operation; live.
- `file_content_generations` — content revisions preserved through moves and replacements; live.
- `nodes` / the `file_entries` view — the index itself, not an upload record.
- `fs_jobs` — background deletion, explicitly out of scope.

No view or trigger references the three legacy tables (`pg_views`, `pg_trigger`), so a later drop has no
secondary dependency to unwind.

## 2. How liveness was proved

A table is treated as live when any supported configuration **creates** a row in it, or when any code
**reads** it on a path that can still run (a request route or a started worker). A row that can only be
mutated by a drain of rows created by an older release is reported separately as *producer-dead*.

- **The router is the authority.** `Server.Handler` (`backend/internal/api/server.go:191`) is the only
  mux. `/upload`, `/upload/` and `/api/fs*` are registered to `retiredFileRoute`, which answers
  `410 legacy_file_api_retired` (`server.go:234-236`, `server.go:272-274`). Everything under `/api/files`
  is dispatched by the switch in `filesHandler` (`backend/internal/api/files_boundary.go:358-470`).
- **The legacy handlers are unreachable.** `handleAPIUpload`, `handleAPIComplete`,
  `handleAPIUploadStatus` (`api_upload.go`), `handleUploadSign`, `handleUploadConfirm`, `handleMkdir`,
  `handleDelete` (`files.go`) and the retired account/API v1 handlers have no route registration. Their
  only callers are tests that build their own mux (`files_delete_test.go:171-173`,
  `upload_index_test.go:230,461`). `browser_delete_test.go` separately asserts the retired paths answer
  `410`.
- **Wiring decides the configuration.** `api.New` creates `uploadIndex` and `browserUploads` only for
  `STORAGE_BACKEND=cos` with `FILE_COORDINATED_WRITES=false` (`server.go:107-111`); it wires
  `filesJournal` whenever a database is configured (`server.go:100-104`). Every native `/api/files` mutation returns
  `501 not_implemented` unless coordinated writes are on (`files_mutations.go:144-147`).
- **The schema was inspected, not assumed.** Migrations were applied with `eam migrate` to a disposable
  database and `\dt` listed the tables above; each legacy table matches its creating migration plus, for
  `upload_index_watches`, the `021` alteration. No later migration touches the three.

Reproducing the audit:

```bash
export DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5434/eam_audit_tmp?sslmode=disable'
(cd backend && go run ./cmd/eam migrate)
psql "$DATABASE_URL" -c '\dt'
grep -rn --include=*.go -E 'upload_sessions|upload_index_watches|browser_uploads|file_uploads|file_upload_parts|file_upload_native_metadata' backend cmd
grep -rn --include=*.go 'handleAPIUpload\|handleUploadSign\|handleMkdir\|handleDelete\|checkDeleteLegacyUploads' backend/internal/api
```

## 3. Liveness per table

| Table | Created by (reachable?) | Read by | Read on | Replaced by the newest design |
|---|---|---|---|---|
| `upload_sessions` | `api_upload.go:164,216` — **no reachable route** | `storage_baseline.go:147`; `browser_upload.go:145` (retired handler) | **operator CLI only** | `file_uploads` + `file_operations` |
| `upload_index_watches` | `uploadindex/journal.go:23` via `files.go:299` — **no reachable route** | `uploadindex/service.go:153-192` worker; `browser_upload.go:147` (retired) | **background worker**, COS + `FILE_COORDINATED_WRITES=false` | server commit + reconcile |
| `browser_uploads` | `browserupload/journal.go:29` via `files.go:177` — **no reachable route** | `journal.go:96` worker; `journal.go:38,123` | **background worker**, same configuration | `file_uploads` + `uploadspool`/native parts |
| `file_uploads` | `fileops/postgres_uploads.go:205` | `files_uploads.go`, `files_reclaim.go:40`, `postgres_workers.go`, `postgres.go:199,318,506`, `nodes/file_identity.go:68`, `cmd/eam/storage_resolve_upload.go:87` | request path + worker + operator CLI | current |
| `file_upload_parts` | `postgres_uploads.go:486` | `postgres_uploads.go:470,520`, `postgres_upload_lifecycle.go:122` | request path | current |
| `file_upload_native_metadata` | `postgres_upload_native.go:125` | `postgres_upload_native.go:115` | request path | current |

**Net: no table is provably dead and safe to drop today.** Three are live in both directions. Two are
producer-dead yet still read (and mutated) by a worker in a supported configuration, so they are live under
the rule above. One is producer-dead with no runtime reader at all, and its rows are the admission evidence
described in §4. The rest of this document proves each verdict and turns them into a gated plan.

### `file_uploads` — LIVE

Created by `handleFilesCreateUpload` (`files_uploads.go:303`) through `Postgres.CreateUpload`
(`postgres_uploads.go:205`), read back by every session status, part, commit, abort, worker and reclaim
path. Configuration: `FILE_COORDINATED_WRITES=true` on any backend (Local, COS or S3); without it no
upload session can be created at all. This is the table the review calls "the newest".

### `file_upload_parts` — LIVE

Written only by class C (relay) receipts: `handleFilesUploadPart` (`files_uploads.go:783`) streams each
part through `uploadspool.Receive` or `ReceivePart` and seals a receipt with
`AcceptUploadPart` (`postgres_uploads.go:486`); commit proves coverage from those receipts
(`postgres_upload_lifecycle.go:122`). Configuration: coordinated writes **and** a relay connection —
`FILE_TRANSFER_CLASS=relay`, which is the default and the only class Local serves. On a class A
(presigned-parts) connection the bytes never reach the application and no row is written; the provider's
own part listing is the progress record.

### `file_upload_native_metadata` — LIVE

The class A counterpart: the native multipart handle and immutable part identities are read and written
through `storage.UploadMetadata` (`postgres_upload_native.go:102-127`) by
`BeginPlannedUpload`/`SignUploadParts`/`ListUploadParts`
(`objectmutation/uploads_signed.go:46,167,231`), which the server calls from
`files_presigned_uploads.go:55-140`. Configuration: coordinated writes **and**
`FILE_TRANSFER_CLASS=presigned-parts`, which configuration validation restricts to COS
(`config/storage.go:142-149`).

### `browser_uploads` — PRODUCER-DEAD, CONSUMER-LIVE

The only creator is `Service.Begin` (`browserupload/service.go:41`), called from `handleUploadSign`
(`files.go:177`), a retired handler. No supported configuration can create a row.

It is still read on a path that can run. `api.New` constructs the service for COS without coordinated
writes (`server.go:107-111`), and `StartUploadIndex` (`upload_index.go:100-124`, called from
`cmd/eam/server.go:121`) starts `Service.Run`. `Due` (`journal.go:96`) claims nonterminal rows and
`RunOnce` (`service.go:336`) aborts an expired `open` or a `receiving` attempt, indexes an `applied`
one, and for `publishing`/`unknown` inspects the destination. It never calls `Publish` — that is reachable
only from the retired `Receive` — and an applied-but-unproven attempt is retained as `unknown` forever
(`cos/browser_upload.go:118-131`: "a lost final response does not prove that all native I/O is
terminal"). So the drain cannot empty this table either.
`FILE_COORDINATED_WRITES` defaults to false (`config/storage.go:74-80`), and the README makes the legacy
drain a prerequisite for turning it on, so this is a supported configuration, not an accident.

### `upload_index_watches` — PRODUCER-DEAD, CONSUMER-LIVE

The only creator is `Journal.Register` (`uploadindex/journal.go:23`), called from `handleMkdir`
(`files.go:299`) through `watchUpload` — also a retired handler. No supported configuration can create a
row.

The observation worker runs in the same default COS configuration (`server.go:107-111`,
`upload_index.go:118-124`) and reads and updates rows through `Claim`/`Finish`
(`journal.go:43-97`). Its retirement rule is deliberately asymmetric: `Finish` **deletes a directory
watch** once the marker is indexed, but a **file watch is only rescheduled, never deleted**
(`journal.go:75-96`); migrations `018` and `021` state that unresolved legacy file grants stay observable
indefinitely. Rows here are therefore not merely stale — each is an obligation the code refuses to
discharge by itself.

### `upload_sessions` — DEAD as a feature, read only by the operator CLI

Written by `handleAPIUpload` (`api_upload.go:164,216`), `handleAPIComplete` (`:347`) and
`cancelPendingUploads` (`:395-430`); read by `loadUploadSession` (`:461`), `cleanupExpiredUploads`
(`:434-459`) and `checkDeleteLegacyUploads` (`browser_upload.go:142-157`). Every one of those callers is a
retired handler or a function reachable only from one. No request or worker path touches this table.

The one live reader is the operator command `eam storage-baseline`
(`cmd/eam/storage_baseline.go:147`), which copies every unfinished row into the baseline artifact. The
comparison then reports `legacy_sessions_present` and refuses `reservationBaselineReady`
(`storagebaseline/baseline.go:217`). That is the admission protocol saying, in the code, that database
status alone cannot retire a legacy provider allocation.

### The review's claim, precisely

- "Six tables" — correct (§1).
- "Four generations" — correct (§1).
- "Only the newest is written" — **correct only for row creation.** No reachable code creates a row in a
  legacy table. It is false for row *mutation*: in the default COS configuration the drain workers still
  lease, reschedule and delete rows in two of the three legacy tables, and deleting a file watch would be
  a settlement decision the worker never makes. That difference is the whole reason a drop is not a
  cleanup.

## 4. What the existing rows are

**No row in any of the six is the only record of a user-visible fact.** A published file is the native
object plus its index row; a terminal session, receipt or result is a replay cache. Specifically:

- `upload_sessions` with `completed_at` or `cancelled_at` set: the upload is over and, when it completed,
  the file is in the tree. The row is a receipt.
- `browser_uploads` in `completed`/`aborted`: the journal itself deletes these after `retain_until`
  (`journal.go:112-119`); the row is a receipt.
- `file_uploads`, `file_upload_parts`, `file_upload_native_metadata`: the current design retains terminal
  rows for idempotent replay; the file is the native object.

What a row can be the only **database** record of is unpublished native work:

- `upload_sessions` with `completed_at is null` maps an owner/path to a provider multipart upload
  (`provider_upload_id`) whose parts may still occupy the bucket. The provider is the authority on the
  bytes; the row is the only mapping from that allocation to an account and a path.
- `upload_index_watches`: each row is an observation the index still owes; a file row is never retired
  automatically, so it is the only record that a native object may exist unindexed.
- `browser_uploads` with a nonterminal state: the checkpoint names staged bytes that the worker can
  discard, or index if the object already carries this attempt's marker; it cannot publish them.

None of these is user-visible loss if dropped — the bytes are untouched and the native tree remains the
source of truth — but dropping them asserts a reconciliation that has not been performed.

**Stop-and-report (task item 4).** `upload_sessions` is dead as a feature, yet its unfinished rows are
the only database evidence of legacy provider allocations, and the product's own admission command treats
them as blocking. `upload_index_watches` is in the same position for an index observation. This is a
reconciliation problem, not a schema cleanup, so this task stops here: **no table is dropped now.** The
plan below is the gate that would make them droppable.

A timing fact bounds the risk without removing the obligation: the legacy routes were retired in
`4005cae` (2026-11-02) and a legacy session lived one hour (`api_upload.go:24`), so at the time of this
audit no client transfer from that code can still be arriving. What remains is abandoned provider state
and unresolved observations, which is exactly what `storage-baseline` and the watch worker exist to
resolve.

## 5. Retirement plan

### Phase 0 — Reconcile (operator, on the current revision)

This phase produces the evidence the drop will assume. It must be recorded (an artifact or a runbook
entry naming the owner and the date) before Phase 1 starts.

1. **`upload_sessions`.** For every row with `completed_at is null`, ask the provider whether the
   allocation still exists (`ListParts`/`ListMultipartUploads` for `provider_upload_id`; `Head` on the
   key for a single-part session) and abort the ones that do. Keep each row until its provider answer is
   recorded. The block clears when `eam storage-baseline --owner-id UUID --writers-quiesced` no longer
   reports `legacy_sessions_present` and `reservationBaselineReady` is true.
2. **`upload_index_watches`.** Run the observation worker (COS, `FILE_COORDINATED_WRITES=false`) until
   the remaining rows have been observed, then settle each remaining file watch by hand only with
   evidence that the path is indexed or absent. There is no automatic retirement for file watches
   (`journal.go:76-96`), so this step is operator work by design, not a bug to wait out.
3. **`browser_uploads`.** Let the recovery worker drain what it can (it aborts unreceived attempts and
   indexes applied ones) and clean terminal rows after `retain_until`. A row left in `unknown` is kept
   that way by design because the object's final native I/O is not proven terminal; record the decision
   and the row, since the worker will never retire it.
4. Record the result and re-run `eam doctor`.

**Gate:** do not start Phase 1 while any of the three tables holds a nonterminal row whose native outcome
is unknown, or while the baseline still reports a legacy session for an owner being admitted.

### Phase 1 — Release A: remove the readers and the drain, keep the tables

- `server.New`: stop constructing `uploadIndex`/`browserUploads` (`server.go:107-111`); remove
  `StartUploadIndex` and its call site (`upload_index.go:97-125`, `cmd/eam/server.go:121`).
- Delete `internal/uploadindex`, `internal/browserupload`, the retired handlers (`api_upload.go`, the
  legacy half of `files.go`, `api_actions.go`, `api_files.go`, `api_jobs.go` if nothing live references
  them), `watchUpload`, `settleUpload`, `checkDeleteLegacyUploads`, and the browser-namespace lock once
  its last caller is gone.
- `cmd/eam/storage_baseline.go`: remove the `upload_sessions` query. Keep
  `Snapshot.LegacyUploads` (emit `[]`) and `Version: 1` so artifacts captured before the change remain
  readable by the offline `--input` recheck.
- Do **not** drop anything in this release. A server still running the previous revision (during the
  deploy window, or after a rollback) reads these tables; its drain and its baseline command must keep
  working.

### Phase 2 — Release B, at least one deployment after A: drop

Ship the migration in §6 only once Release A is the oldest revision that can be running. After it, a
rollback past A is not supported and the release note must say so. Inside the file, drop
`browser_uploads`, then `upload_index_watches`, then `upload_sessions`: there are no foreign keys between
them, but the last one is the one whose absence changes baseline semantics, so it goes last.

### Why this order

`make migrate` runs from the new binary while the old server is still serving (the deploy script builds,
migrates, then restarts). A drop shipped in the same release as the code removal would therefore race a
running old binary, and a rollback would hit a missing table. No legacy table has a reachable producer, so
"stop writing first" here means **remove the readers and the drain first**; the table must outlive the
last revision that reads it.

## 6. Migration shape for the follow-up

`sql/047_retire_legacy_upload_tables.sql`:

```sql
-- Retire the three upload tables that no reachable code creates rows in.
--
-- Precondition, enforced by deployment order and recorded in the release note,
-- not by this SQL: (1) the legacy drain has been reconciled with the provider and
-- the index (plan Phase 0); (2) the release that removed the readers and the drain
-- is the oldest revision that can still be running (plan Phase 1).
--
-- Why drop, and why now: the rows are not the only record of any user-visible fact
-- (the native tree and the index are), and the unresolved part of them has been
-- settled against the provider and the index. Keeping the tables would keep the
-- old state machine looking reachable.
--
-- Deliberately unconditional. The runner records an applied file permanently; a
-- guard that declined to drop would be recorded as done and could never run again.
-- `if exists` only keeps the file cheaply re-runnable after a lost history table.
drop table if exists browser_uploads;
drop table if exists upload_index_watches;
drop table if exists upload_sessions;
```

Do not add a data backfill: there is no data in these tables that the native tree does not already
contain, and the rows that were evidence were reconciled in Phase 0. Do not touch `file_uploads`,
`file_upload_parts` or `file_upload_native_metadata`; they are live. If the generation-4 tables are ever
retired, drop `file_upload_parts` and `file_upload_native_metadata` before `file_uploads` — both have a
foreign key to it.

Verification after the drop: `eam migrate` twice (the second must report zero applied), `make test-pg`,
and `eam doctor`.
