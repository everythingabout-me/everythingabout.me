# Browser upload progress and recovery

The current browser COS upload flow uses the existing sign/confirm routes with same-origin file bytes. It does not activate the pending unified `/api/files` mutation/upload API. COS and Postgres still cannot share an atomic transaction.

## Normal path

1. `POST /api/fs/upload/sign` accepts `{path,size,overwrite,mime?,modifiedAt?}` and creates a temporary owner/root-bound upload record. It returns only `{uploadId}`. No provider credentials, native upload IDs, URLs or SDK requirement reach the browser.
2. The browser streams the original `File` to `POST /api/fs/upload/confirm?uploadId=...` using `application/octet-stream` and an exact Content-Length. The server receives nonempty files into unpublished native multipart state, preserving their ordinary final path, original accepted name and raw contents.
3. After exact body length and EOF verification, the server durably records its publication intent, sends native completion once, records known native success, and immediately writes the database index. The bounded final phase continues independently of browser cancellation after body reception.
4. The result becomes `completed`. That record is immediately excluded from recovery queries. A 24-hour result receipt permits querying an interrupted response; expiry cleanup touches only database receipts. It performs no COS HEAD, LIST or deletion for successful files.

Empty files receive no native allocation during body reception. The final phase sends a single zero-byte PUT with the same attempt marker and journal protections. Nonempty files use 16 MiB native parts and SHA-256 while reading. At most four Stage calls run concurrently per process, bounding their byte buffers at about 64 MiB independently of total selected file count. The complete file is never buffered in memory or Postgres.

## Interrupted uploads

Browser queue state remains in memory and disappears on refresh. No server record is created for the rest of a selected directory until an individual file starts. Those files therefore have neither native objects nor database file entries.

An incomplete body never starts final publication. Its known multipart allocation is aborted and the temporary record becomes `aborted`; no file index row is invented. An abandoned `open` record expires after one hour. A receiving process crash is recovered after its 90-second lease expires. Lease tokens renew every 15 seconds and prevent stale workers from changing a newer record. An allocation response lost before its provider handle was saved can leave unpublished provider fragments; bucket multipart lifecycle management remains the backstop for those allocations. Recovery never deletes a final readable file to clean staging.

For known native success followed by database failure, the record remains `applied` and background recovery retries only index settlement, without another native completion. Restart resumes unfinished records. Recovery scans at most 32 pending records per pass; each record's recovery work is bounded by a one-minute context. It never scans all completed objects.

A failed final native request is different from incomplete reception. The outcome remains `unknown`; it must not be declared aborted or blindly republished. A server-assigned native metadata marker attributes an observed object to the exact attempt. Marker presence can permit index repair, but alone does not prove all old final-path requests terminal. Those rare unresolved attempts retain their path protection and are checked again; once their index result is saved it is not rewritten on every check. Native quiescence that cannot be proven needs operator recovery, rather than a timeout pretending no effect occurred. Successful synchronous native responses with SDK retries disabled settle normally and leave the recovery queue.

The browser retries `POST /api/fs/upload/confirm` with JSON `{uploadId}` before resending any file. A pending/unknown result returns 409 and keeps that receipt; a completed result returns the stored file resource. Only definitive 404/410 permits starting a fresh receipt and body. Aborting the XHR cancels receiving, while the finalizing UI disables cancellation after the body has been sent.

## Namespace and metadata

Upload admission and existing browser delete/rename actions share an owner namespace admission lock. Those actions reject paths affected by unfinished uploads; completed receipts do not block them. New uploads also reject source/destination subtrees of pending or running directory move/copy jobs, so an asynchronous browser rename cannot outlive the admission lock and race a new file. The lock is not held across file body reception. This guards the current browser flow; it does not silently move old public API writers, external clients or the P3/P4 write-admission foundation under the same coordination.

Index confirmation preserves its serializable snapshot/ancestor-row protections, original source modification time, MIME and ordinary native paths. Source metadata is recorded before receiving bytes, so recovery need not guess an old local timestamp from COS Last-Modified. Native completion has a unique attempt marker, and successful replay queries return the saved result instead of re-running the index write.

## Folder presentation

The browser lists only immediate children throughout upload. Real indexed entries and pending entries merge by normalized path. Each folder summarizes selected descendant file count, confirmed files, bytes, percentage and failed tasks. These counts describe the selected batch, not an inventory of all pre-existing files merged into a destination folder. Completed lightweight metadata remains in browser memory for the summary, while local `File` references are released. Refresh discards it.

Folder markers keep small temporary observations until marker/index creation succeeds. Both explicit success and successful background repair retire those observations. They are not permanent per-folder polls. Directory markers and zero-byte files remain distinct.

## Upgrade and old grants

Apply all forward migrations, including `021_upload_watch_retirement.sql` and `022_browser_uploads.sql`, then restart the backend and refresh browser clients. Migrations are idempotent. The former removes already-settled observation records and adds an incarnation fence, preventing a stale worker from deleting a later watch for the same path. The latter stores temporary upload state and partial indexes for active recovery and terminal receipt cleanup.

Existing broad STS credentials issued by the previous browser code cannot be revoked by changing the frontend. Stop/drain old browser upload sessions before cutover and do a scoped `reconcile --owner` if needed. Unresolved legacy file watches retain their conservative recovery behavior; no new file watches are created by this flow. Legacy public `/upload` and outside native writers remain separate until their migration. This implementation does not claim a historical broad grant has been fenced by a database receipt.

## Validation

Unit tests cover body length/EOF, zero-byte publication, marker attribution, one native completion request, partial allocation cleanup, lease loss, restart, index failure, unknown outcomes without republishing, receipt reuse and no repeated index churn. Disposable PostgreSQL tests apply migrations twice, exercise concurrent claim fences, owner/root isolation, receipt expiry and real API recovery. A 100,000-completed-receipt regression verifies completed files stay outside the active recovery query; this is not a live COS throughput benchmark. Frontend tests exercise XHR progress/cancellation and ensure retries inspect the old receipt before a new body is sent.
