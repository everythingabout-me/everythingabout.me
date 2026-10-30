# P0: boundary decisions and production write admission

P0 adds executable contract helpers, isolated primitive probes and a read-only baseline command.
It does not activate the replacement API, change existing authentication, migrate the database,
revoke credentials, or modify customer content. The [capability evidence](storage-p0-capabilities.md)
distinguishes verified primitives from unverified provider guarantees. The production write gate
remains closed until the deployment-specific evidence below exists.

## Boundary decisions for P1–P3

- The API path `/` maps to internal `""`; `/Notes/a.md` maps to `Notes/a.md`. Keep existing
  `drive/<ownerUUID>/<relative-path>` keys. Decode a query once, then validate without trimming,
  cleaning, case conversion or NFC rewriting. NFC is comparison evidence, not a new native name.
- An explicit Authorization header selects Bearer even if its value is empty or whitespace.
  Unsupported/invalid credentials return 401 without a cookie fallback. Token/session verification
  remains the existing auth service's responsibility; the P0 helper selects the credential source.
- Cookie mutations require both a single approved Origin and an opaque `X-CSRF-Token` validated
  against the authenticated session. This includes content/part PUT, commit, abort and operation cancel.
  The token must be cryptographically random, session-bound and checked without leaking secrets.
  P2 must wire issuance into authenticated browser bootstrap, never into provider credentials.
- Configure canonical API origins explicitly at startup. Do not derive the trusted origin from
  request Host, Forwarded or X-Forwarded-Host. Proxies must strip untrusted forwarding headers;
  proxy trust for client IP/TLS is a separate deployment concern. Missing/null/multiple Origin on
  cookie mutations is denied; native clients use explicit Bearer. P0 helpers do not change the
  existing server's permissive origin policy.
- P2 preflight must admit the actual file methods and `Authorization`, `Content-Type`, `Range`,
  `If-Match`, `If-None-Match`, `If-Range`, `If-Revision`, `Content-Digest`, `Idempotency-Key`, `Prefer`,
  `X-Source-Modified-At` and `X-CSRF-Token`. Expose `ETag`, `Accept-Ranges`, `Content-Range`,
  `Content-Length`, `Content-Disposition`, `Location` and `Retry-After` to permitted browser clients.
  Bearer access and credentialed cookie access have separate origin decisions; never combine
  credentialed CORS with a wildcard allowed origin. These are P2 integration requirements, not an
  assertion that the current server's allowlist already supports the new routes.
- The replacement file routes use plain resources and Problem Details; account/event responses
  retain their existing parsers. Strong content revisions require proven native evidence or the
  declared coordinated writer model. A metadata/index revision alone cannot authorize replacement.
- Keep operation/session IDs, private database PKs and native locators distinct from public file
  identity. Root identity binds owner, connection, generation and root configuration; credential
  rotation does not change it. Missing recovery evidence retains a durable unknown-outcome guard.

## Capture and compare a baseline

The new maintenance command is read-only against Postgres and COS. It selects exactly one existing
owner UUID, reads index rows in a repeatable-read/read-only transaction, saves full node metadata
(including private IDs/event-link inputs), enumerates exact native keys with checked pagination,
and records unfinished legacy upload rows including cancelled/expired rows. It performs no HEAD,
GET, PUT, DELETE, index update, quota write, or cleanup. COS credentials/configuration come from the
normal application configuration only for this explicitly invoked read command.

```bash
cd backend
go run ./cmd/eam storage-baseline \
  --owner-id 00000000-0000-0000-0000-000000000001 \
  --output /secure-backup/new-owner-baseline.json

# Offline recheck: does not load .env, connect to Postgres or call COS.
go run ./cmd/eam storage-baseline \
  --input /secure-backup/new-owner-baseline.json \
  --output /secure-backup/rechecked-baseline.json \
  --quota-bytes 107374182400
```

The output path must not exist. The command creates it with mode 0600; back it up securely because
it contains private names, metadata and internal upload locators. Partial output after an I/O failure
must not be treated as complete. No bytes or credential secrets are stored in the artifact.
Memory usage is proportional to native/index entry count; large estates require a bounded inventory
implementation in P1 before using this tool at their scale. A failed listing returns an error and
never produces a complete artifact. This scan deliberately bypasses legacy reconcile parsing/pruning.

A successful command means a report was saved, not that write admission is safe. Inspect `report.issues`
and `report.reservationBaselineReady`. The report counts all captured native file bytes, including
unindexed content, distinguishes implicit parent folders from explicit markers, and reports missing
index/native entries, exact key/path mismatches, Unicode collisions, invalid names and validator changes.
ETags are opaque comparison evidence, never claimed to be SHA-256 or native generation fences.
The COS comparison is case-sensitive; destination filesystems need their own name-rule preflight.

Without `--writers-quiesced`, `reservationBaselineReady` is always false. Supply this flag only after
independently establishing native writer quiescence for the entire capture interval. It is an operator
assertion recorded in the artifact, not a lock, provider guarantee or automatic proof. A DB transaction
and two equal lists cannot establish that no delayed write can still arrive. Offline recheck cannot
upgrade this assertion. Unfinished legacy session rows block readiness conservatively; resolve/archive
them through a reviewed migration later, rather than editing this report or deleting evidence.

Existing over-quota data remains intact. A complete, consistent baseline may be ready while over quota:
P3 must preserve that usage, disallow additional liability beyond policy and allow policy-approved
non-growing changes. Never delete native content or subtract unindexed files to make a quota fit.

The artifact is a migration comparison aid, not a complete application backup. Independently preserve
Postgres (accounts, sessions, permissions, annotations, events and relationships), owner-to-root mappings,
bucket/region/root configuration, and credential/secret references in an access-controlled backup. Back
up the current save-link signing seed explicitly before separating it from COS/DB configuration in P1;
never print that seed into baseline JSON or logs. Test restoration of configuration and application metadata
separately from direct native-file portability.

## Drain old write authority before enabling a root

1. Record the owner/root mapping and storage credential identity, deployment build, UTC start time and
   all writer processes. Stop admission at the actual issuing routes (`POST /upload`, Web upload sign),
   legacy raw PUT routes, complete/confirm handlers, move/copy/delete workers, import/claim commands,
   automation and external provider/native tools. Disable issuance across every instance. A new DB guard
   does not control these old paths. Keep reads limited to guarantees that old/native writers permit.
2. Record the latest possible grant issuance and expiry. Current public single PUT and multipart-part
   URLs use `uploadTTL = 1 hour`; Web STS requests 3600 seconds (the helper accepts up to 7200).
   Include deployments with older settings, cache/client-held grants and clock uncertainty. STS grants
   include native multipart initiation/completion, so the application's upload_sessions table is not a
   complete inventory. Current audit/session rows do not prove all STS grants were recorded.
3. Drain requests already accepted by the application, workers, gateways and native provider. Credential
   expiry only prevents future authorization at the provider's evaluation point; it does not prove that
   a previously accepted remote request cannot finish later. Application shutdown, a lease expiry, a
   cancelled DB session or a HEAD of the desired file does not establish native quiescence.
4. If bounded expiry/draining cannot establish safety, use provider-supported credential/policy revocation
   or a separately verified write-deny window. Verify how it affects previously issued temporary credentials
   and accepted requests instead of assuming rotating a parent secret revokes everything immediately.
   Preserve access for backup/reads where possible. Record provider-side evidence and any unresolved effects.
5. Enumerate provider multipart sessions under the exact owner root, including STS-created sessions absent
   from Postgres. Match known native IDs and captured legacy intents. Resolve possibly completed writes
   before aborting known temporary sessions; aborts themselves can be ambiguous. Never recursively delete
   content or abort every bucket upload as a cleanup shortcut. Persist unrecognized or in-flight effects as
   unresolved liabilities; no production write activation for conflicting roots/paths until resolved.
6. During verified quiescence, capture the native/index baseline with `--writers-quiesced`. Preserve exact
   names and internal PKs. Resolve mismatches by reviewed index-only scan/import in P1; the current
   `reconcile --dry-run` still exercises legacy normalization/parent-pruning logic and is not this gate.
   Capture again after repair. Over-quota files stay present and charged.
7. Attach baseline, credential/grant timeline, provider capability evidence, native quiescence evidence,
   backup/restore checks and unresolved-effect disposition to the release record. P2–P5 acceptance must
   pass on isolated roots before production activation. Retire old routes and grants in the P6 release;
   client incompatibility permission does not revoke previously issued native authority.

For providers without a safe automatic quiescence/recovery proof, retain a blocked root/path and require
explicit operator recovery. Never declare an unknown final effect aborted or release a conflicting guard
merely because the expected file is visible. No production capture/drain/revocation was performed as part
of implementing this tooling.

## Runnable evidence

```bash
# Normal suite includes boundary fixtures, baseline comparisons and isolated local probes.
make test

# Focused baseline/native enumeration tests; no application configuration required.
cd backend
go test ./internal/storagebaseline ./internal/cos ./cmd/eam
```

See [capabilities](storage-p0-capabilities.md) for local/COS probe commands and evidence limitations.
The helpers and fixtures test the replacement contract foundations, not the still-legacy HTTP server.
P1/P2 must reuse and exercise them through actual adapters/routes; P3 must prove durable journal/guard
behavior with real fault injection before write admission.
