# Unified file API: implementation state and remaining work

> Source-aligned 2026-11-04 in the shared identity branch. R0–R5 code and first-phase contracts are present;
> full Go/disposable-Postgres, client, production-build and built-SPA Chrome acceptance passed. Targeted
> race checks passed. This replaces the earlier path-only gap inventory based on
> main revision 6c96399; the implementation is now rebased onto main 9a32267. Implementation presence,
> disposable test evidence and production admission are
> separate claims. No deployment or live provider capability is inferred from this document.
>
> Runtime references: [API](../api/api.md), [identity](file-identity.md), [changes/audit](file-changes.md),
> [architecture](storage.md), [driver](storage-driver.md), [native upload deployment](native-upload-deployment.md).

## 1. Current delivered foundations

P0–P6 are implemented foundations, not a greenfield backlog. Local/COS/S3 trees, indexed reads, durable
Operations/guards/quota reservations, admitted namespace writes, numbered uploads and the shared Web client
remain in place. Metadata/list/search/usage use Postgres without synchronous native lookups. Native files keep
accepted filenames, complete readable folders and original bytes, independently usable without Postgres.

Official COS uploads retain exact same-origin signed UploadPart requests through ordinary EdgeOne forwarding.
The application authorizes/confirms/completes; COS verifies the signature and MD5. Embedded mode retains
server-computed SHA-256 receipts and Local/native object staging. Native COS/S3 part bodies stream directly
through the application to the provider while hashing, without retaining a complete part in memory. Current
geometry is 32 MiB × at most 10,000 parts (320,000 MiB before lower provider/budget limits), with sixteen
parallel part requests. Local retains its durable staging strategy. Signed receipts explicitly identify client
SHA-256 assertion plus COS-verified MD5. Commit performs native completion and index/result settlement without
whole-file reassembly/reupload. No edge function or extra upload service is introduced by identity or changes.

Object revision evidence remains current ETags with operation-specific native conditions. COS ignores PUT
If-Match, so admitted replacement uses gateway coordination and an immediate ETag recheck. Bucket versioning
is not required; object reads are not version-pinned and arbitrary external writers are not globally fenced.
Writes require connection evidence, legacy-authority drain and a verified budget baseline. Native uncertainty
retains guards even after lease expiry. The old protocol routes return 410 and issue no new native authority.

The browser queue remains page-memory state, preserved during folder navigation and discarded on refresh or
close. It has up to eight running queue tasks sharing sixteen part requests, no extra per-file cap, same-page
receipt/retry recovery and exact cancellation identity. Ephemeral owner/path Web Locks coordinate cooperating
same-origin tabs. Durable server sessions and accepted Operations survive independently. Identity/change work
does not restore a browser queue after reload or automatically abort unrelated clients. Displayed progress
updates once per second; transfer speed uses the trailing ten-second window from the rebased main branch.

## 2. R0–R5 source status

All implemented rows below have integrated disposable-Postgres/client/browser evidence recorded in section 4.
Targeted race checks passed. Production admission and live deployment gates remain separate.

| Slice | Implemented source | Boundary |
|---|---|---|
| R0 — Contracts | [API](../api/api.md), [identity](file-identity.md), [changes](file-changes.md), architecture/driver/README/agent guidance and client contracts | Source-based contracts now describe implemented first-phase forms; historical phase docs remain context |
| R1 — Identity tree | [026](../../sql/026_file_identity.sql), [027](../../sql/027_canonical_file_tree.sql), [identity service](../../backend/internal/nodes/file_identity.go) | Persisted root UUIDs, existing node UUID continuity, parent/name authority and unique equivalent siblings |
| R2 — ID/path boundary | [shared selector resolution](../../backend/internal/api/files_identity.go), [selectors](../../backend/internal/filecontract/selector.go), [client](../../web/src/api/client.ts) | Same routes accept ID or direct path; owner/current path scope, concealed parent redaction and admission bindings remain enforced |
| R3 — Native tree move | [capture/recovery](../../backend/internal/fileops/actions.go), [index settlement](../../backend/internal/nodes/file_actions.go), [30,000-entry fixture](../../backend/internal/fileops/native_tree_move_local_test.go) | Eligible Local folder moves capture one root effect and relink parent/name; longer/deeper destinations and flat object moves use finite fallback |
| R4 — Facts/markers/audit | [journal](../../backend/internal/filechanges), [settlement](../../backend/internal/nodes/file_changes.go), [scan observations](../../backend/internal/nodes/file_scan_changes.go), [audit boundary](../../backend/internal/api/files_audit.go), [028](../../sql/028_file_changes.sql), [031](../../sql/031_effective_change_markers.sql), [032](../../sql/032_file_change_root_binding.sql) | Commit-safe heads, effect deduplication, retained ancestry, recursive/namespace markers, durable third-party read admission and owner audit |
| R5 — Bootstrap/pull/SSE | [snapshot handlers](../../backend/internal/api/files_change_snapshots.go), [pull/subscriptions](../../backend/internal/api/files_changes.go), [029](../../sql/029_file_subscriptions.sql), [030](../../sql/030_file_change_snapshots.sql) | Immutable synchronous snapshots, policy-bound private cursors, durable seven-day SSE subscriptions; webhooks and async snapshots deferred |

### Identity and native rename details

Canonical authority is owner/root/parentId/name/comparisonName. The file_entries view resolves current paths
through ancestry; legacy path/parent/key columns are derived nullable caches, not identity. A verified move
preserves node/event references and descendant IDs. Copy/recreation allocate fresh identities; content
replacement preserves file ID and changes contentRevision. Parent-ID and path destinations remain equivalent.
Unknown IDs never create entries, and ID-selected admitted work does not follow an intervening ancestor move.

Comparison is versioned nfc-sensitive-v1 or nfc-fold-v1. The latter uses Unicode full case folding with the
same generated mapping in Postgres and Go; lower() or ASCII-only matching is insufficient. Original accepted
spelling stays visible. Equivalent native sibling collisions are rejected rather than exposed by ID alone.

The native_tree_move strategy is admitted only when the driver proves native tree movement, the selected
source is a folder with native evidence, and the destination prefix does not grow in UTF-8 byte length or
segment depth. It records one move effect, root evidence and subtree guards without descendant enumeration.
The non-growing restriction avoids making descendant path-limit claims without stored subtree summaries.
Longer/deeper destinations use the finite manifest to preflight names/paths. Generic object moves still
copy/verify/delete actual entries; copy and recursive delete retain finite manifests and partial recovery.

Native rename avoids mandatory per-descendant identity writes or one change event per descendant. It does
not establish universal O(1) behavior: dynamic path resolution is depth-sensitive, and search can perform
work proportional to indexed candidates times ancestry depth. Derived cache maintenance, fallback moves,
copy and delete can scale with tree size. Benchmark these separately from the native rename root effect.

### Change and audit details

Confirmed index effects, change facts, affected-folder patches, recursive markers and effect audit commit
through the existing settlement transaction. Per-owner head allocation is serialized until commit, after
index/quota locks and without native I/O. Effect receipts survive feed pruning for replay deduplication.
Application reads and denials are audit facts, not file changes. Public feed projection excludes actors and
hidden endpoints; GET /api/audit is a separate owner-cookie management collection.

Metadata may include lastChangedAt and an opaque random changeToken. A folder includes observed descendant
changes; files inherit ancestor namespace moves without inheriting sibling content edits. Hidden aggregate
coverage omits markers. Additive import emits external creation/update observations only when metadata
changes. It still does not prune outside deletions or identify ambiguous outside renames.

Token changes/subscribe rights are explicit, dependent on read, and false for existing tokens/save links and
new Connect apps unless granted. PolicyVersion advances on actual policy changes and revoke, so expansion
and restoration cannot resurrect old cursors. Name-only changes preserve policy generation.

### First-phase bootstrap and notification limits

Snapshots capture the index and committed head in one repeatable-read view, then materialize immutable
metadata before 201. Bounds are 100,000 entries, 64 MiB serialized metadata, 25-second request/20-second SQL
statement windows, 24-hour lifetime and ten active snapshots per principal. Failure is explicit, including
503 snapshot_too_large/temporarily_unavailable; no asynchronous snapshot Operation is advertised. Targets
must exist and match their declared kind. Started/unknown overlapping native work prevents a complete baseline.

Pull returns items/nextCursor/hasMore with a sealed fixed filter and cycle head. ID watches follow current
identity only while authorized; path watches stay at their location. Terminal ID cleanup ends with
watch_ended and then 409 watch_deleted/watch_access_lost. Views receiving a required refresh take a new
whole-view snapshot; narrower ID invalidation continuations return baseline_required. Targeted cursor splicing
is deferred. A complete visible ID/parent tree can apply one atomic subtree linkage move in place.

SSE subscriptions have seven-day renewable leases and at most 100 active resources per principal. Durable
matcher progress and notification IDs are independent of consumer cursors. Streams last at most 25 seconds,
poll approximately each second, send one coalesced availability notification and close. Reconnect and pull;
Last-Event-ID is not feed acknowledgement. Only sse delivery is accepted; there is no outbound webhook code.

Rows have a 90-day default expiry. `eam file-changes-prune --owner-id UUID [--dry-run]` removes only an expired
contiguous feed prefix and retains effect/deduplication receipts. No automatic maintenance scheduler is wired.
Audit history currently remains indefinitely, with no 365-day deletion policy implemented. Snapshot expiry
is enforced and creation performs bounded principal-local expired snapshot cleanup.

Migration 032 persists the stream root namespace. BindRoot acquires shared root protection before producer
index locks; normal ordering-head allocation follows index/quota work. Repeatable-read readers acquire shared
session root protection before their MVCC snapshot, then verify the durable namespace inside the transaction.
Reset takes exclusive root protection before the head. First use binds an empty namespace; later mismatches
reject rather than follow configuration silently.

After DB restore/mapping rebuild or each root rebind, including A → B → A, stop application/external writers,
settle accepted work and run `eam file-changes-reset --owner-id UUID --writers-quiesced [--dry-run]` against
current config. It refuses any pending guard/nonterminal Operation; rotates epoch, sets retained floor=head,
expires snapshots/subscriptions and clears markers. Positions, effect receipts, file IDs, audit and native
bytes remain. Same-namespace restores also require reset, so restored secrets/epochs do not revive cursors.
Both maintenance commands support rollback-only dry-run. See the [root lifecycle runbook](file-changes.md#durable-root-binding-and-restorerebind-procedure).

## 3. Remaining implementation and release work

| Work | Concrete boundary |
|---|---|
| Deployment-specific acceptance | Validate production root admission, authority drain, live EO/COS configuration and operating limits |
| Retention operation | Schedule/monitor the explicit prune CLI; define deployment disk/lag policy and audit-retention requirements |
| Outside deletion/identity reconciliation | Complete coordinated bounded enumeration before inferring deletion; retain uncertainty and native evidence |
| External-writer accounting | Review budget resynchronization separately from additive metadata import |
| Webhooks | Separate receiver-control, signature/rotation, constrained egress, durable delivery/retry and revocation design; no outbound behavior in phase one |
| Larger snapshot workloads | Explicit future async/batching design when synchronous limits are insufficient; never report partial metadata as ready |
| Tracking health/watchers | Native feed/watcher integration, overflow handling and freshness monitoring require explicit coverage evidence |
| Product interfaces | Audit/notification management UI and verified app registry are not implied by API types or self-reported Connect names |
| Thumbnails | Authenticated route remains 501; implement bounded safe asynchronous conversion before advertising support |
| More backends | WebDAV/cloud drives/NAS and cross-connection transfers remain optional integrations |

R6 is deployment evidence. Run fresh/repeated migrations, mapping backup/restore, no-database native exit,
actual browser/mixed-client workflows and large-tree tests on isolated roots. Measure native rename separately
from ancestry/search/fallback costs, uploads, retained liabilities, per-part staging and actual egress.

For official EO/COS verify exact forwarding rewrites and header stripping, no application-byte fallback,
UploadPart-only signer permissions, negative operation-selector probes, MD5/ETag assumptions, native completion,
abort/lifecycle behavior and old write-authority drain. Disposable fixtures cannot attest live IAM, DNS/TLS,
EdgeOne rules, bucket settings, real throughput or a production root's admission.

## 4. Verification record

Historical evidence is distinct from the final source: the main 6c96399 plan refresh passed make test with
194 Web tests; the earlier permission slice passed focused auth/Connect Postgres tests and make test with
205 Web tests. Those runs do not substitute for the integrated verification below.

The implementing parent confirmed the following results on the final integrated source rebased onto main
9a32267, including 32 MiB streaming parts, the trailing-ten-second speed window and all 32 migrations.
No post-rebase reruns remain pending:

- EAM_TEST_DATABASE_URL on the explicitly supplied disposable Postgres and repository-local GOCACHE:
  final make test exited successfully across all Go packages, real isolated PostgreSQL integration cases,
  TypeScript and 211 Web tests. Migration fixtures apply the complete sequence twice to isolated databases.
- npm run build passed. Real Chromium TestFilesBrowserAcceptance passed against that built SPA using its own
  temporary test HTTP service: 13.56 seconds in the browser (14.234 seconds for the Go test), with 15 checks
  covering actual upload, preview, cancellation, queue flows and scoped Bearer behavior. The fixture uses
  current part geometry and budgets. This did not start or deploy the production server.
- Native streaming SDK coverage passed with a 64 MiB nonseekable body: the actual S3 SDK sends its first bytes
  before consuming the complete body and hashes/verifies integrity in one pass. The per-UploadPart unsigned
  payload setting preserves SigV4 request authentication while avoiding whole-body prehash buffering. Focused
  S3/objectmutation tests, streaming race coverage and all previously affected native PostgreSQL cases passed.
- The native Local 30,000-descendant case passed, preserving source IDs/native inodes with no child index
  rewrites and one root move effect. Root-binding/reset PostgreSQL tests passed; migration-032 API feed and
  snapshot suites passed. Current object-baseline fixtures passed.
- Six transformed-client contract cases passed for snapshot create/pages/delete, cursor encoding and terminal
  Problems, subscription create/list/inspect/renew/delete and SSE URL construction, invalid selectors before
  network/CSRF, and cookie-only owner audit. The entire Web suite has 211 passing tests.
- The real-Postgres race suite passed: go test -race ./internal/api ./internal/fileops ./internal/filechanges
  ./internal/nodes ./internal/auth. This covers the five core packages with migration-032 root protection.
- Documentation link/JSON validation and owned-path whitespace checks passed. First-phase integrated
  verification is complete; production/deployment boundaries remain as described above.

These are disposable code/integration results, not production admission, live EO/IAM/COS probe evidence,
provider throughput benchmarks or a deployed-site browser check. No production migration, reset, prune,
admission, native-content move or deployment was performed by this documentation alignment.
