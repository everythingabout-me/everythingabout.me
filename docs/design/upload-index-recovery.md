# Browser upload progress and index recovery

This change improves the current legacy COS browser flow. It does not implement the pending unified write/upload API or claim atomic publication across COS and Postgres.

## Directory presentation

The browser lists only immediate children of the current directory from the start of an upload. Real indexed entries and pending entries merge by normalized path. Folders are navigable while their creation/upload is pending. Each folder summarizes selected descendant files: total and confirmed file counts, sent/total bytes, percentage, failures, and cancel/retry actions. Directory creation tasks do not increase file counts. Completed task metadata remains in memory for these summaries, while its local `File` references are released. A new batch after completion clears the prior statistics. Refreshing deliberately discards unfinished browser tasks.

The figures describe the selected upload, not a recursive census of pre-existing files in a merged destination folder. Progress reaches byte completion before server confirmation; a file counts as uploaded only after confirmation. Completion removes progress bars without changing paths or listing order.

## Durable observations

Apply `sql/018_upload_index_watches.sql` and restart the backend. The migration is repeatable. Before `/api/fs/upload/sign` returns its existing STS credentials, it must persist an observation for the owner/path. Before `/api/fs/mkdir` writes a folder marker, it must persist a directory observation. If registration fails, credentials are not returned and the folder marker is not written. No file bytes or credentials are stored in this table.

The startup worker runs independently of the optional timeline organizer. It resumes due records immediately, polls every five seconds, claims at most 32 records, and performs at most four checks concurrently. Each storage/index observation has a ten-second deadline. Failed storage reads or database writes retry after 30 seconds. Durable generation and lease-version conditions prevent old workers from overwriting scheduling renewed by another sign or confirmation.

A pending watch is checked every 15 seconds through its maximum issued credential expiry plus one hour. A normal successful confirmation changes that path to a daily check, but does not remove its watch. Pending watches also become daily after the hot period. Cold records are retained indefinitely: expiration or one successful HEAD cannot prove that previously authenticated native requests cannot finish late. A crashed worker's claim becomes available after three minutes. These are scheduling intervals, not unconditional recovery latency guarantees under load, outages, or process downtime.

The binding hashes the legacy backend, connection identity/generation, bucket, region and owner-key layout, excluding rotating credentials. A changed storage binding cannot reinterpret old watches against a different content root. Returning to a previous binding resumes its records. The operator must explicitly reconcile a migrated root rather than silently rebinding old recovery work.

## What is recovered

The worker checks the exact file key or directory-marker key. It indexes the actual complete object that exists, regardless of the intended upload size; this is native-tree observation, not a receipt proving an upload attempt succeeded. Zero-byte files and directory markers remain distinct. Missing objects and unmerged multipart parts do not become file records. Recovery never merges parts, retries file transfers, removes objects, or prunes missing index entries.

Normal confirmation and background observation both start a serializable database snapshot and lock existing ancestor/target rows before reading COS. Index writes are committed with those locks; serialization failures require a fresh observation. This prevents a captured old HEAD from overwriting a concurrently updated index or recreating an already indexed file after application deletion has finished. Implicit parents are repaired as well. This is not a native request fence: an old authenticated PUT can still arrive after delete/rename/replace; when it creates a real native object again, the observer indexes that native fact.

An unchanged representation keeps its original accepted browser modification time, MIME, annotations, node ID, creation time and `updated_at`. Changed native content invalidates the old content checksum while preserving unrelated annotations. Lost-confirm recovery uses current COS Content-Type/Last-Modified (or a filename MIME fallback), because the original browser `File.lastModified` is not recoverable from file bytes alone. A normal later confirmation may supply the original timestamp. Repeated unchanged checks do not continually trigger timeline processing.

COS and Postgres cannot share a transaction. There can therefore be a temporary gap after native upload success. The durable observation makes that gap recoverable after browser refresh, request cancellation, database failure or backend restart, provided the database record and configured native storage remain accessible. It cannot provide instant global consistency, fence external writers, undo native effects, or restore a lost database without a scan.

These hooks cover browser sign/confirm and folder-marker creation from this release onward. They do not retroactively discover paths whose old browser confirmation was already lost; use scoped `reconcile --owner` for existing unindexed objects. Legacy public `/upload` and external writers require their own existing reconciliation procedures until the unified write migration.

## Validation

- Frontend tests cover immediate-child projection, nested counts/bytes, mixed roots, Unicode coalescing, completed-task identity, zero-byte files, empty folders, cancellation/retry selection and retained navigation.
- Browser checks exercise pending-folder navigation, completed rows and mobile layout using the actual frontend with intercepted sample transfers.
- Worker tests cover lost confirmation, restart, late objects, missing objects, cancellation, bounded claims/concurrency, failed checkpoints, stale generation/lease acknowledgments and cold-watch replay.
- Disposable PostgreSQL tests run migrations twice, exercise real journal fencing and owner/root isolation, recover lost browser confirmation and canceled directory requests through API handlers, preserve accepted metadata, and check stale HEAD/delete races.
