# Native uploads through EdgeOne and embedded reception

The official service uses COS. File bytes travel from the browser through ordinary EdgeOne forwarding directly
to COS, which validates a narrowly scoped signature issued by the application. There is no edge function, extra
upload server or `eam upload-gateway` command. Self-hosting defaults to byte reception inside `eam server`.

This user-approved signed transport supersedes the earlier prohibition on client-visible native upload
parameters. Clients still use one origin and receive no signing secret, broad STS credential or provider SDK.
The returned request is an opaque short-lived capability for one exact part, not a general COS API credential.

## Byte and control paths

| Deployment | File-part bytes | Control and completion |
|---|---|---|
| Official, FILE_UPLOAD_INGRESS=edgeone | Browser -> same-origin `/api/files/transfer/` -> EdgeOne -> COS UploadPart | Browser -> application authorize/confirm/commit; application -> COS ListParts/Complete; index/result |
| Self-hosted COS, embedded | Browser -> application part PUT -> native unpublished multipart | Application-controlled Complete and index/result |
| Self-hosted Local, embedded | Browser -> application part PUT -> isolated durable Local staging | Application verifies and publishes using the Local driver |

COS parts are unpublished until server-controlled completion. Commit verifies exact acknowledged coverage,
seals the manifest and performs native completion once, followed by index/result settlement. It never assembles
or uploads another whole-file body. It is a native publication operation, not just a database update.

Both transports retain four concurrent files, up to four parts per file (maximum sixteen browser data requests),
8 MiB parts and at most 10000 parts (80000 MiB before budgets or lower storage limits). Small single-content PUT
has its separate default 32 MiB/object maximum 5 GiB policy and is disabled in edgeone mode. Zero-byte sessions
have no parts and use the existing guarded zero-byte publication at commit.

## Signed part lifecycle

Sessions declare `transport: "cos-signed"` or `"embedded"`. For a signed part:

1. The client computes the assigned Blob's SHA-256 and base64 MD5. It POSTs size, sha256 and md5 to
   `/api/files/uploads/{id}/parts/{number}/authorize` with ordinary application authentication/CSRF.
2. The application checks current authorization, root/session state and exact part geometry. Under the durable
   part fence, it binds size, client SHA-256 assertion and Content-MD5 before issuing authority. A different
   digest for the same slot conflicts. It creates/reuses the recorded native multipart allocation.
3. The application returns `{method:"PUT",url,headers,expiresAt}`. The URL is origin-relative below
   `/api/files/transfer/`. Its signature binds the final COS Host, object path, PUT method, uploadId,
   partNumber, Content-Length, Content-Type and Content-MD5. Expiry is five minutes. The browser cannot set a
   different Host; EdgeOne rewrites it to exactly the Host used for signing. The URL exposes the native upload
   parameters needed for this one request but no secret key.
4. The client first calls POST `/api/files/uploads/{id}/parts/{number}/confirm` to recover an already stored
   part after a lost reply. `409 part_pending` means no verified native part is ready yet. Only then does it
   PUT the exact Blob with the returned Content-MD5 and Content-Type through EO. COS checks signature and MD5.
5. After transfer (including an ambiguous network error), confirm queries COS server-side ListParts for the
   recorded native upload. Exact part size and the admitted native ETag=MD5 semantics must match the bound
   candidate. Browser-supplied ETag or a successful browser XHR alone is never sufficient. Only then does the
   application persist the public part receipt. Duplicate confirm returns that receipt.
6. Normal commit freezes complete service receipts, rechecks current authorization and target/parent conditions,
   performs guarded native Complete, and settles the existing Operation/index. No Complete/Delete authority is
   delegated to the browser.

A signed receipt includes `checksumSource: "client-sha256-cos-md5"`. Its SHA-256 is the client assertion bound
before the grant; the application did not read the file body and cannot claim to have independently computed
or verified SHA-256. COS validates Content-MD5, and confirm checks native size/ETag. This mode requires exact-bucket
`signedMultipartParts: true` capability evidence in addition to `multipartUploads: true`: Content-MD5 enforcement,
ETag=MD5 without incompatible encryption/transformation, signature scoping and native part listing must all be
verified. ETags are never relabeled as SHA-256. Embedded receipts retain server-computed SHA-256. MD5 provides
COS's supported content-integrity binding, not SHA-256 collision resistance against chosen malicious content.

COS verifies the signature and Content-MD5; application confirmation verifies storage state. Grants must be
signed with separate `COS_UPLOAD_SIGN_SECRET_ID` / `COS_UPLOAD_SIGN_SECRET_KEY`, never the broad mutation key.
The signing principal's effective policy must allow **only name/cos:UploadPart** on the configured bucket/root,
with no inherited UploadPartCopy, PutObject, CompleteMultipartUpload, AbortMultipartUpload or DeleteObject.
An extra unsigned x-cos-copy-source header can select a different COS operation on a similar PUT URL; signed
fields alone do not prove absence of extra headers. Verify negative action-policy probes against the actual
signing principal, and strip all x-cos-* request headers on the EO transfer rule unless a specifically reviewed
header is required. A distinct SecretID is enforced by code, but effective IAM scope is a deployment proof.

## EdgeOne configuration

Configure `FILE_UPLOAD_INGRESS=edgeone` on the existing application with the admitted COS connection, explicit
FILE_COOKIE_ORIGINS and stable application identity. The old `external` mode is rejected. Do not deploy another
Go process. All control routes, including authorize, confirm, receipts and commit, continue to reach the app.

At the existing public origin, configure an EO rule matching **PUT** with the exact literal prefix
`/api/files/transfer/`. Its source is the explicit COS bucket HTTPS endpoint. Do not use a browser redirect.

| Forwarding setting | Required behavior |
|---|---|
| Origin | The actual configured COS bucket endpoint, not the application server |
| Origin Host | Exact bucket host used by the server-side COS signer |
| Path rewrite | Remove only the literal `/api/files/transfer` prefix, leaving the leading `/` and exact encoded object path |
| Query parameters | Preserve every signed query parameter, uploadId and partNumber; do not sort/decode/re-encode or append parameters covered by signing |
| Method/body | Preserve PUT and original binary bytes |
| Required headers | Preserve Content-Length, Content-MD5 and Content-Type exactly |
| App/provider selector headers | Remove Cookie, application Authorization, X-CSRF-Token, all x-cos-* headers and unrelated application identity headers before COS |
| Cache | Disable caching for transfers and all upload control routes |
| Redirects/fallback | No redirects, URL normalization or fallback to application ingress; route failure must fail the upload |
| Body limit/timeouts | At least the advertised 8 MiB part size and suitable transfer timeouts; the site's 800 MB ordinary upload limit is sufficient |

The byte request intentionally carries COS authorization in the signed query, not the application Bearer header.
Control requests use the normal Cookie/CSRF or explicit Bearer rules. XHR does not explicitly add application
headers to byte requests, but same-origin cookies may be attached by the browser; the EO stripping rule is
required. Do not send application credentials to the COS origin. Redact signed queries from access logs and
analytics; do not cache grant responses or place a signed URL in a persistent/public document.

The app refuses all requests under `/api/files/transfer` and refuses ordinary file-body PUTs in edgeone mode,
returning `503 edgeone_route_required` before body reads. This prevents a missing EO rule from silently consuming
application bandwidth. Client validation rejects cross-origin/protocol-relative URLs and unexpected methods,
headers or transfer paths. EO rules must preserve special filenames including `+`, spaces, `%`, `?`, `#` and
Unicode. A literal encoded-looking filename must not be decoded twice.

Private COS origin read authorization is not the upload credential: the per-part signature supplies that
permission. Do not configure EO to replace it with an unrelated read-only Authorization header. Rule availability
and exact UI/action names depend on the account/plan. These requirements describe the necessary transformations;
this repository does not provision the live EO site, DNS/TLS or attest that a specific account's rule is active.

References: [COS request signatures](https://cloud.tencent.com/document/product/436/7778),
[Upload Part](https://cloud.tencent.com/document/api/436/7750),
[EO Host rewrite](https://edgeone.ai/zh/document/46965),
[EO origin request parameters](https://edgeone.ai/zh/document/52036),
[EO ordinary upload size](https://cloud.tencent.com/document/product/1552/70782).
The edge-function 1 MB limit does not apply because no edge function is used.

## Embedded reception and recovery

`FILE_UPLOAD_INGRESS=embedded` remains the self-hosted default. The app receives each part, computes SHA-256,
and persists Local bytes or native COS/S3 parts before acknowledging the receipt. Native metadata remains
owner/root/configuration bound in Postgres (migration025); embedded byte handling has a 60-second read-idle
limit, not a total upload timeout. The application's admitted publication metadata/locks remain durable.

The browser retains four-by-four scheduling, combined progress, bounded retries and exact session/create-key
recovery. It verifies receipt hashes against a reselected File before skipping bytes. Signed retries always
reconcile provider state through confirm before retransmission; an unknown confirm grants no durable ACK.
Cancellation aborts browser requests and the exact server session/Operation, never a committed readable file.

Issued signed requests cannot be revoked merely by logout, application cancellation or expiry of the session
cookie. They authorize only the bound part of the old unpublished native upload. Future grants/confirm/commit
still require current application authorization. Cleanup aborts recorded native allocations and retains physical
liability for outstanding signed authority/uncertain native requests, rather than treating wall-clock expiry as
proof that provider work is terminal. Final-path unknown outcomes retain their existing guard; no repeated
Complete or timeout-based unlock is introduced. Native publication remains server-controlled.

Provider lifecycle must retain acknowledged native parts for the advertised session lifetime (up to seven days),
and handle orphaned allocations after a lost initiation reply. Do not recreate an allocation beneath acknowledged
receipts after provider loss. Drain existing sessions and old issued authority before changing transport or
admission. Keep database, root bindings and application publication ledger recoverable; restored metadata may
be behind actual native effects. See [authority drain](storage-p6-backend.md) and [driver contract](storage-driver.md).

Validation uses disposable databases, SDK/EO forwarding fixtures and browser tests. A local forwarding fixture
can verify signature/path/header contracts, missing/tampered grants and app byte isolation, but is not evidence
that the live EdgeOne site or real COS bucket has been configured correctly. Run a scoped positive/negative
upload probe on the actual deployment before enabling production signed grants.
