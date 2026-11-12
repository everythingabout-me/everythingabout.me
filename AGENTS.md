# AGENTS.md

Working notes for AI agents and new teammates in this repo. The full story is in
[README.md](README.md); the API design is in [docs/api/api.md](docs/api/api.md) and the event model in
[docs/design/events.md](docs/design/events.md). The accepted storage/API architecture is in
[docs/design/storage.md](docs/design/storage.md); read it, [the driver contract](docs/design/storage-driver.md)
and [the transfer protocol](docs/design/transfer-protocol.md) before changing file transfers or storage drivers. The accepted [stable identity contract](docs/design/file-identity.md)
implements IDs, canonical parent/name paths and direct ID-or-path selection. First-phase
[changes/audit/snapshots/SSE](docs/design/file-changes.md) is implemented too. Disposable integration/browser
tests passed; targeted race checks passed. The clean-slate [file API](docs/api/api.md) supersedes old
route/envelope/path-identity assumptions for replacement work.
New contracts have no legacy-client compatibility requirement.
The runtime conventions below describe existing code unless the new specifications explicitly replace them.
This file only covers what you must know before touching anything, and does not repeat the README.

## One paragraph

My personal data base: **an email-backed account system + a file layer + one self-organizing stream**.
Accounts, the folder tree, attributes, and events live in Postgres; file bytes live in Tencent Cloud COS.
Go backend (standard library `net/http`) + React SPA (Vite + React Router), frontend and backend split,
no Next.js.

## Bytes are the source of truth

This is the product's basic philosophy, and it decides what may be treated as optional.

The native tree — folder structure, original filenames, raw contents — **is** the product. Postgres is a
**read cache** over it: accounts, the parent/name index, ACLs, events, stable IDs, change cursors and
operation journals all exist to make that tree fast and safe to use, and all of them are disposable.
Every piece of content that matters to a user lives in the bytes, in a form standard tools can open
without this service.

**Only user-visible loss is loss.** If the database is lost, every file is still there under its real name
in its real folder and the user has lost nothing; the rebuild — scan the tree, re-index, re-issue IDs,
re-apply ACLs, lose share links and tokens — is our cost, not their loss. Never describe operator cost as
user cost, and never let a convenience for recovering our cache become a requirement on the user's data.

Two consequences follow, and both are easy to get wrong:

- **A cache may be thrown away, so nothing irreplaceable may live in it.** Credentials stay in `.env` or
  another `0600` file and the database holds a reference to them, because a disposable artifact gets
  dumped, copied off the host, restored into a drill and hand-edited. This is not hygiene; it is the same
  rule applied to secrets.
- **A cache that cannot be rebuilt is not a cache.** Rebuilding the index by scanning the native tree is
  the load-bearing operation of this architecture. A change that makes it impossible, lossy, or dependent
  on state the tree does not carry breaks the product's premise, not just an internal convenience.

It also settles what a backup is: a user's content survives because the storage layer keeps copies of the
tree. Application metadata is an operator convenience, and no user is ever asked to understand it.

## Contracts carry their reasons

Every contract here — an API shape, an identity rule, an ordering, a "must"/"never", a storage
invariant — states **why it exists**: the failure it prevents, the cost it imposes, and what would
justify revisiting it. A rule whose reason is not written down is not a contract, it is folklore:
the next person cannot tell whether it still holds, so they either obey it blindly or break it blindly.

When you add or change a contract, write the reason next to it (design doc, code comment, migration
header, or the error message itself) in the same change. When you propose something a contract
forbids, argue with the reason, not the sentence: if the facts that justified it changed, or its cost
now outweighs what it protects, change the contract and its reason together. "The contract says so"
is not an argument; "the contract says so because X, and X still holds here" is. A reason that no
longer matches the code is worse than none, because it makes an obsolete rule look settled.

Existing contracts without a stated reason are debt: when a change touches one, either find the
reason and record it, or treat it as negotiable until someone can. Worked example from this repo: a
folder must exist before a file may be created into it (`409 parent_missing`) because identity is
parent/name and an empty folder needs a native marker object. That reason — not the status code — is
what a proposal for implicit ancestor folders has to answer, and what decides whether the marker
stays worth its cost.

## A finding must change a decision

An agent's contribution in a discussion is measured by what it changes, not by how much diligence it
displays. A finding earns its paragraph only if it changes what someone will build or how, and then it
has to say so: the decision, the reason, the alternative. Everything else costs the reader time and,
worse, makes a settled premise look open again.

- **Do not re-open a premise that is already a contract.** The reasons are written down here and in the
  design docs. If the facts behind a reason changed, argue with the reason; otherwise build on it.
  Asking whether a stated premise is tested, or whether it was considered, is not a contribution — it is
  the appearance of one.
- **Do not dress a checklist item as an opinion.** "This needs to be verified" names no decision, no
  failure and no next step. Say what would be verified, what would break if it were not, and what you
  would do differently — or leave it out.
- **Do not invent a problem in order to have something to say.** If a stated contract already covers the
  failure you are about to raise, raising it is noise; find the part the contract does not cover.
- **Do not pad.** No unrequested caveats, no restating the question, no risk list that ends without a
  recommendation, no summary of what was just said. If there is nothing substantive, say that in one line.
- **When corrected, accept it in one sentence and move to the substance.** Re-explaining the mistake at
  length is another way of not contributing.

The test is blunt: if deleting the paragraph would not change what anyone builds, delete it.

## Running it

Prerequisites: Go 1.26+, Node 20.19+, Postgres. Local development also needs a COS bucket and a Tencent
Cloud sub-account key pair.

```bash
cp .env.example .env    # fill in DATABASE_URL / COS_*; never commit .env
make install            # go mod download + frontend npm install
make migrate            # apply pending sql/*.sql migrations in filename order
make dev                # backend :8080 + Vite :5173 (/api is proxied)
```

Common commands:

| Command | What it does | External dependencies |
|---|---|---|
| `make dev` | Run the Go backend and Vite together | Postgres + COS |
| `make server` | Backend only (also serves the frontend if `web/dist` exists) | Same |
| `make build` | Build `web/dist`, then compile `bin/eam` | None (needs node_modules) |
| `make test` | `go test ./...` + `tsc --noEmit`; Postgres-backed tests skip and are counted | None |
| `make test-pg` | Go tests with the Postgres-backed suite required; fails instead of skipping when the URL is unset | Postgres (disposable, `CREATEDB`) |
| `make fmt` | `gofmt -w` the backend | None |
| `make migrate` | Create / upgrade tables | Postgres |
| `make check` / `make check-write` | `eam doctor` self-check (`--write` writes a probe object, then deletes it) | Postgres + COS (+ SES) |

`EAM_TEST_DATABASE_URL` is the only switch for the Postgres-backed tests. It must name a **disposable**
PostgreSQL service with `CREATEDB`: those tests create random databases, apply every `sql/*.sql` file to each
one twice, and drop them, so never point it at a live database. Without the URL they skip, and `make test`
ends with `WARNING: N Postgres-backed test(s) skipped` — a green run that quietly executed none of them is
exactly the hole this count exists to close. `make test-pg` sets `EAM_TEST_REQUIRE_DATABASE_URL=1`, so a
missing database fails the run. This matters because the upload journal, the operation journal and the change
stream are only actually exercised against Postgres; a `ListUploads` read-only-transaction regression shipped
while its tests were skipping. CI (`.github/workflows/test.yml`) runs `make migrate`, `make test` and
`make test-pg` against a `postgres:17` service container.

- Every backend maintenance command lives in one binary: `cd backend && go run ./cmd/eam <cmd>` (or `./bin/eam`).
- `internal/config` loads `.env` automatically, searching up to three directories upward, so you never export
  by hand; Go itself does not read `.env`.
- Without SES configured in development, the verification code is printed to the backend log and filled into
  the login page automatically — **never expose a development server to the public internet**.

## Repo map

| Path | Contents |
|---|---|
| `backend/cmd/eam/` | Single entrypoint: `server` / `migrate` / `doctor` / `reconcile` / `claim` / `token` |
| `backend/internal/api/` | Routes and handlers: web `/api/*`, the public API, `/events` |
| `backend/internal/nodes/` | Canonical parent/name index, dynamic current paths and transactional identity/change settlement; never bytes |
| `backend/internal/filechanges/` | Commit-safe owner change facts, recursive markers, projection and durable file audit |
| `backend/internal/storage/` | Object storage interface; implementation in `internal/cos/` (Tencent Cloud) |
| `backend/internal/mediameta/` | Capture facts (time, GPS, device, size) read from a photo or video by ranged reads, on demand; results are a disposable cache |
| `backend/internal/timeline/` + `events/` | The stream: analyze → group → score → narrate, stored as `events` |
| `backend/internal/auth/` `emailcode/` `mail/` | Verification codes, passwords, sessions, API tokens, SES |
| `backend/internal/jobs/` | Async jobs for directory move / copy |
| `sql/` | Forward-only migrations, run in filename order |
| `web/src/` | React 19 + Vite + React Router 8; styling is CSS Modules |
| `docs/` | API and design docs; read these before changing semantics |

## Backend conventions

- Standard library `net/http` + Go 1.22 method routing (`mux.HandleFunc("GET /api/...")`). **Do not add a web
  framework.**
- Handlers all use `func(http.ResponseWriter, *http.Request) error` and are wrapped with `s.wrap` (web) or
  `s.api` (public API). Business errors use `httpx.Error` (`httpx.BadRequest(...)` etc.), which maps to a
  status code plus `{ok:false,error}`.
- **An error must carry its cause.** A bare code, an empty detail, or "see the server log" hands the next
  person a debugging session instead of an answer — `storage invalid_config:` with an empty path cost hours
  on a bucket move that only needed `evidence is for ap-hongkong, configured is ap-beijing`. Say which
  file/field/owner/connection disagreed, what was expected and what was found, and the one command that
  resolves it. Keep provider response bodies out of client-visible text, and remember that the request id
  and the log line accompany the message; they do not replace it.
- **Do not bound request-scoped work with a wall clock.** Bound the work itself (entry/page limits, a
  per-request provider timeout) and let the request context be the only deadline. A timer on top produces a
  failure the response cannot explain, and it fires exactly when the library is large enough to matter.
  The same applies to every other budget: it is **computed from the work it admits**, never picked as a round
  number. A request count comes from the client's legitimate rate (requests per file × concurrency), a byte
  budget from size ÷ a pessimistic transfer rate, a memory bound from bytes per entry × entries. If the
  product's own workflow can reach a budget, the budget is wrong — 600 requests/minute could not survive the
  first minute of a 23k-file import, and a 30-second clock aborted the recursive delete that needed 58.
- The current runtime uses `{ok:true,data}` / `{ok:false,error}` (`internal/httpx`). The replacement file API
  uses plain JSON resources and Problem Details as specified in `docs/api/api.md`.
- **Everything in this repo is written in English** — comments, docs, SQL comments, Makefile and scripts,
  commit messages, logs, CLI output, API error text, and test failure messages. Chinese exists only as
  product copy: the `zh` entries in `web/src/i18n/messages.ts`, the `zh` email templates, CJK test fixtures
  that exercise non-ASCII paths, and keyword data that recognizes Chinese file/folder names.
- Preserve complete native folder structure, original accepted filenames, and raw contents below each user
  content root. The database indexes files and holds application metadata; it must not be required to interpret
  the stored tree. Replacement work follows `docs/design/storage-driver.md`; anonymous authoritative blobs
  and database-only moves/copies/deletes are rejected. Keep current COS key construction in `internal/paths`.
- Multi-user isolation rests on `owner_id` (`users.id`, a UUID) — new tables and queries must always carry the
  user dimension.
- Dependencies are wired in `api.New`; define an interface first for a new service (see `storage.Storage`,
  `timeline.Analyzer`) and never construct clients inside a handler.
- **Deletion is permanent** (there is no trash); deleting a nonempty directory requires explicit `?recursive=true`, and
  code must never recurse "helpfully" on the user's behalf. An accepted delete is concealed from every read at
  once; the native purge and index settlement continue in the background.
- File clients share `/api/files`; legacy cookie `/api/fs/*` and root public file routes return 410.
  The accepted file API is one `/api/files` contract for every client,
  accepting a stable service UUID or direct path at the same endpoint, with no `/api/web/files` mount.
  EntrySelector JSON is `{id}|{path}`; query selection is exactly one nonempty id or path. Creation destinations
  use `{path}` or `{parent:{id}|{path},name}`. Current path shortcuts are explicitly retained in the API docs.
  Metadata returns id/parentId alongside path/parentPath, redacting concealed parents. Root ID is persisted.
  Move/content replacement retain IDs; copy/recreation allocate fresh IDs. IDs never bypass owner/path scope.
  Explicit Authorization selects Bearer without fallback; absent Authorization may use the session cookie
  with CSRF protection on every mutation.
- Canonical identity is owner/root/parent/name/comparisonName; legacy path/key columns are nullable derived
  caches. Use file_entries/current ancestry, never stale cached paths as identity or authorization. Comparison
  versions nfc-sensitive-v1/nfc-fold-v1 must agree between Go and Postgres; full Unicode folding is not lower().
  Eligible Local non-growing folder moves use one root native effect; growing/deeper targets and flat object
  moves retain finite manifests. Dynamic ancestry/search is not universally O(1).
- State vocabulary: **revision** (`Entry.Revision`, `ExpectedRevision`, `SourceRevision`) is the driver's opaque
  token for one native state — the CAS/identity receipt, derived from the ETag or a provider version id, never
  exposed by the API. **version** (`RetainsVersions`, `PinnedRead`, `ReadOptions.PinnedVersion`,
  `Object.VersionID`) is a retained historical copy, a feature this service does not use. **index revision**
  (`nr_…`, `If-Revision`) gates application mutations and is unrelated to both. Do not call a revision a version.
  Hashed or persisted byte spellings (`native-etag:`, `native-version:`, the `Version` key in content-revision
  evidence) keep their historical form so old records stay comparable.
- Change facts, recursive/namespace markers and confirmed effect audit settle with index/results. No native
  I/O or new index locks after acquiring the short owner-head lock. Retain effect receipts across feed pruning.
  lastChangedAt/changeToken are optional observed-index markers, not native validators or cursors.
- Incremental clients use immutable change-snapshots plus changes cursors, not ordinary list pagination.
  Snapshots are synchronous and bounded (100k entries/64 MiB/24 hours, plus a per-statement SQL timeout); no async snapshot Operation.
  Tokens need explicit changes+read, and subscribe additionally for SSE; old tokens/save links gain neither.
  PolicyVersion advances on actual policy edits/revoke, never name-only edits. SSE wakeups do not acknowledge
  consumer progress. Webhooks are deferred. GET /api/audit requires owner cookie, not app permissions.
  Default change-row expiry is 90 days; file-changes-prune removes only expired contiguous feed rows and
  retains dedup receipts. No automatic scheduler is wired; file audit currently remains indefinitely.
- Migration 032 binds the change stream's root namespace on first use only. Configuration selects the root: a
  bucket, region, prefix or generation change is adopted at startup (re-probe write capability, rebind the owner
  budget, rotate the change-stream namespace, log all three), because the native tree is authoritative and the
  index is a derived view. A mismatch appearing while running still rejects access, never rebinds silently.
  After DB restore/mapping rebuild, stop writers/settle work and use file-changes-reset --owner-id UUID
  --writers-quiesced [--dry-run]. Pending guards/nonterminal
  Operations block reset. It rotates epoch/floor=head, expires snapshots/subscriptions and clears markers;
  it never changes file IDs/native bytes or drops effect receipts. Same-namespace restore still needs reset.
  Producers call BindRoot with shared root protection before index locks, then the short head after index/quota.
  Repeatable-read consumers protect the root at session level BEFORE beginning their MVCC snapshot; reset
  takes exclusive root protection. Database checks do not replace actual native-writer quiescence.
- Long operations (move / copy) always use `Prefer: respond-async` and return `202 + Location`; do not invent a
  second "task" concept.
- Rate limiting is in-process memory (`internal/ratelimit`), so multiple instances each keep their own count;
  know that before changing it. It is a token bucket (continuous refill, no minute-turnover cliff), the file
  API's budget is keyed by owner (an address is not a user), and the account/legacy budget by token or address.
- The timeline organizer is started separately with `StartTimeline(ctx)` after the server starts; the sweeper
  scans for increments roughly every 20 minutes, and `POST /api/timeline/rebuild` is the manual full rebuild.

## Database / migrations

- Name new migrations `sql/0NN_xxx.sql`: increasing number, one topic per file, with a header explaining what
  changed and why.
- **`eam migrate` records applied files in `schema_migrations` (`sql/044_schema_migrations.sql`) and applies
  only files that are not recorded.** Each file runs in its own transaction together with its history row,
  so a failed file is not recorded and a second run is a no-op. Files are matched by base name: never rename
  or delete one. Keep new files cheaply re-runnable (`create table if not exists` / `add column if not
  exists`), guard destructive DDL, and keep data backfills in the same file — after a lost version table the
  bootstrap rule can only adopt files before 044, so files from 044 on are re-executed.
- **Bootstrap rule.** A database that predates the version table has the schema but no history: on its first
  run `schema_migrations` is empty while the core `nodes` table from 001 already exists. The old runner
  applied all files in one transaction, so a database last migrated by a release containing 001–043 has
  all of them; one last migrated by an older release could be behind, which this history-less rule cannot
  detect. Replaying the old files is no longer safe: `035` recreates a global unique index that `036`
  replaced, and that fails once two owners have a connection named "nas". The runner therefore records
  every file before `044_schema_migrations.sql` as `method='baseline'`, writes the reason into `reason`,
  and does not execute it. A genuinely empty database has no `nodes`, so every file runs in order. Once
  the table has rows it is the authority.
- **The run is bounded, not timed.** It takes a session advisory lock (`eam-migrate-v1`) so two deploys
  cannot migrate concurrently, and sets `lock_timeout=10s` so a migration waiting behind a running query
  fails naming the lock and the file instead of hanging. That bounds lock waits, not wall-clock work; there
  is deliberately no total timeout.
- `nodes` is an index, not a copy: **never write file bytes into the database**; the `path` / `key` mapping
  lives in `internal/nodes` and `internal/paths`. Parent/name links are canonical; current paths resolve
  through file_entries and legacy path/parent/key columns may be null/stale derived caches.
- `source_modified_at` on older rows is only complete after `reconcile` has run; until it is filled, the
  timeline falls back to `updated_at`.

## Frontend conventions

- Pure SPA, no SSR. Routing is a React Router data router (`router.tsx`); page data comes from `loader`, and
  logged-out requests `throw redirect("/login")`.
- Every request goes through `src/api/client.ts` (the single exit), which unwraps the envelope and raises
  `ApiError`; do not call `fetch` directly in a page. File calls use plain resources/Problem Details.
  Browser storage SDKs/STS are retired; integrations use the configured API origin and keep SDKs/credentials
  server-side. Class A/B transfer grants are the narrow exception documented below.
- Styling: global tokens live in `src/styles/globals.css` (warm paper feel + blue accent + automatic
  light/dark via `prefers-color-scheme`); page layouts use `*.module.css`. **No Tailwind, no CSS-in-JS.**
- i18n: the locale is chosen once at module load from the browser language (zh / en), with no switcher. New
  copy must be added to both the `en` and `zh` entries in `messages.ts`; a missing key fails `tsc`. Never
  hard-code Chinese UI copy.
- Take fonts, spacing, radii, and shadows from the variables in `globals.css`; `--danger` is only for
  irreversible actions.
- Keep the first-paint bundle small: dynamically import heavy dependencies.
- **Human-readable sizes are decimal**: `1 KB = 1000 bytes`, labels `KB/MB/GB/TB`, never KiB — the
  same convention Finder and `ls -h` use, so the number here matches the one in the user's file
  manager. Every byte count a person reads goes through `web/src/pages/sizeFormat.ts` (server text:
  `backend/internal/sizeformat.Bytes`), and a user-visible limit is declared in decimal and rendered
  by the formatter, never hard-coded into a message (`48 * 1000 * 1000 * 1000` prints "48 GB").
  Spec and the golden table both implementations must match: [docs/design/size-units.md](docs/design/size-units.md).

## After changing something

1. `make test` must pass (Go tests + frontend typecheck); run it even if you only touched the frontend.
   A change that touches uploads, the upload journal, operations or the change stream must also pass
   `make test-pg` against a disposable database — the suites covering `internal/browserupload`,
   `internal/uploadindex`, `internal/fileops`, `internal/filechanges`, `internal/nodes` and the upload/operation
   paths in `internal/api` skip without `EAM_TEST_DATABASE_URL`, so a green `make test` alone is not evidence.
   If no database is available, say so in the handoff instead of merging on skipped coverage.
2. Run `gofmt` on Go code (or `make fmt`); keep the existing TS style (2 spaces, double quotes, semicolons).
3. If you changed an API or a field: update `docs/api/api.md`, the endpoint table in the README, and the types
   in `web/src/api/client.ts`.
4. If you changed the schema: append a migration under `sql/` and confirm a second `make migrate` is a no-op.
5. If you added UI copy: add both the `en` and `zh` versions.
6. If you added or changed a contract, write its reason where the contract lives, and update the reason when
   you update the rule ([Contracts carry their reasons](#contracts-carry-their-reasons)).
7. Commit messages use Conventional Commits with an English summary, for example
   `fix(cos): encode STS policy resource per path segment so uploads with + stop 403ing`.

## Git workflow

Principle: **the main working tree only ever checks out `main`; all work happens on a task branch in a
worktree, is rebased, and merges back linearly into `main`.** `main` cannot be checked out in two worktrees
at once, so "commit directly to main" means: commit on the task branch → rebase onto the latest `main` →
`merge --ff-only` in the main working tree → (if a remote exists) push.

Unless the user explicitly says "just work in the main tree", everything goes through a worktree.

### 1. Create a worktree

```bash
git fetch origin                                    # when a remote exists; skip otherwise
git worktree add .worktrees/<type>-<slug> -b <type>/<slug> main
```

- The directory is always under the repo's `.worktrees/` (already gitignored); do not put it outside the repo,
  since the sandbox only allows writes inside it.
- Branch names are `<type>/<slug>`, where type is `feat` / `fix` / `chore` / `docs`; add a date or random
  suffix if the name is taken.
- One worktree per task; parallel tasks get their own, and never reuse or touch someone else's branch.
- A new worktree has no `node_modules` and no `.env` (both are ignored); add links from the worktree root:

  ```bash
  ln -s ../../../web/node_modules web/node_modules   # needed by tsc in make test
  ln -s ../../.env .env                              # only needed to run the server or hit the DB
  ```

  `.gitignore` uses `node_modules` (no trailing slash), so this symlink does not show up in `git status`.
- The default Go cache in the sandbox is `~/Library/Caches/go-build` (outside the repo, possibly not
  writable); from the worktree root use the one inside the repo:

  ```bash
  GOCACHE=$PWD/../../.gocache make test
  ```

- Uncommitted changes in the main working tree **do not belong to this task**: do not stash them, do not reset
  them, do not bring them into the worktree. If the task must touch the same files, ask the user first.

### 2. Do it, test it, commit it

```bash
GOCACHE=$PWD/../../.gocache make test   # must pass; run it even for frontend-only changes
git status                # confirm only your files changed
git add <specific files>
git commit -m "feat(scope): English summary"
```

- One logical change per commit; use Conventional Commits with an English summary and put the "why" in the
  body.
- Do not use `git add -A` / `git commit -a`: the main tree may contain the user's own changes.
- If tests fail, fix them; if you cannot, stop and explain instead of committing a red state to `main`.
- **Do not commit with open questions** (unclear requirements, a decision that needs the user, a failing test
  still under investigation): keep the worktree and branch and state clearly in your final message where you
  stopped and what you need.
- **Do not commit a new version for every revision while things are still moving.** A user saying "go ahead"
  can count as final: shipping the first implementation with passing `make test` and merging to `main` is
  fine. But as soon as the user starts asking for changes (different framing, tuned parameters, corrected
  direction), the work is still moving — committing every revision writes drafts into `main` permanently
  (`main` only moves forward and is never squashed afterwards; collapsing them later means rewriting
  history). The right move is to leave changes uncommitted in the worktree, keep revising until the user says
  "that's it", then commit once and merge. Only changes you are confident will not move again (an isolated
  bug fix, a purely mechanical replacement) deserve a mid-flight commit.
- When the user says "don't commit yet / show me the diff first", do exactly that.

### 3. Merge back into main

```bash
git fetch origin                # when a remote exists
git rebase main                 # use origin/main when a remote exists
# On conflicts: resolve one by one → git add → git rebase --continue → make test again

git -C <main worktree> merge --ff-only <type>/<slug>
git -C <main worktree> push origin main          # when a remote exists
```

- Merging must happen in the main working tree (that is where `main` is checked out); `--ff-only` keeps
  history linear, so never create a merge commit.
- If you only realize a branch holds several drafts after the fact: `git -C <main worktree> merge --squash
  <type>/<slug>` → `git commit` in the main tree to squash the drafts into one commit on `main`. `--squash`
  also produces no merge commit, so history stays linear.
- If uncommitted changes in the main tree block the merge: ask the user first; do not force it and do not run
  `pull --rebase` on a dirty tree.
- **Never** `push --force` to `main`, and never `reset --hard` / `clean -fd` the main working tree.
- With a remote, if the push is rejected (someone pushed first): `git -C <main worktree> pull --rebase origin
  main` → re-run `make test` → push again; if that still loops, stop and report.
- With an origin configured, follow the fetch/rebase/push steps above. Only a repository without a remote
  uses a local merge alone and ends with "committed locally, not pushed".

### 4. Clean up

Only clean up in two cases: already merged into `main`, or the user explicitly says to drop the work.

```bash
# Remove the links added in step 1 first for a cleaner removal
rm -f .worktrees/<type>-<slug>/web/node_modules .worktrees/<type>-<slug>/.env

git -C <main worktree> worktree remove .worktrees/<type>-<slug>
git -C <main worktree> branch -d <type>/<slug>    # refuses if unmerged; that is protection, do not rush to -D
git -C <main worktree> worktree prune
git -C <main worktree> worktree list              # confirm only the main tree remains
```

- Merged → delete both the worktree and the branch; explicitly abandoned → delete the worktree but **keep the
  branch** and say so (so nothing is lost).
- **Do not clean up a task that is still waiting for an answer from the user**, or you will have to start over;
  give the worktree path in your message.
- If you truly must force-remove (for example the links are gone but files remain), confirm there are no
  uncommitted changes and use `worktree remove --force`; never `--force` the main working tree.

### 5. Format of the final message

Put the commit / cleanup status at the **very end** of the message, in one line, written in the user's
language:

```text
Committed main@abc1234 (pushed) · 3 files · worktree cleaned up
```

Variants: `Committed locally main@abc1234, not pushed` / `Not committed (tests failing: xxx)` /
`Not committed (waiting for your answer: xxx)` / `Merged main@abc1234, worktree kept at
.worktrees/xxx (waiting for your answer)`. With no changes at all, write `No changes, nothing committed`.

## Do not

- Commit `.env`, secrets, `bin/`, `.gocache/`, or `web/dist/` (all gitignored).
- Never expose signing secrets, broad STS credentials, provider download redirects or arbitrary upload origins.
  Each connection selects one transfer class (A presigned parts, B cloud-drive sessions, C relay); a transfer
  never falls back to another class. A and C are implemented; B is configuration-reserved until a cloud-drive
  driver exists. Class A signs part and read URLs with the configured `COS_SECRET_*` credential, which
  remains server-side; `FILE_TRANSFER_CLASS` selects `presigned-parts` or `relay`. Grants are scoped to one upload
  session or one file, time-limited,
  renewable and recorded before they are issued. Class A part URLs bind exact length and no digest.
  **A staged transfer is never published by its client**: multipart parts stay unpublished until the server
  completes them, class C bytes stay in the spool until the server publishes, and class B is a client contract.
  The exception is a **direct** class A session (one file that fits in a single part, on a connection admitted
  for conditional writes): its grant writes the destination itself — a conditional PUT, or a form upload whose
  signed policy carries the same condition (`x-cos-forbid-overwrite`, exact key and length) because a browser
  sends `multipart/form-data` without a CORS preflight — so the client's write is the publication and the
  commit only verifies the object and settles the index. A replacement always keeps the conditional PUT: a
  form upload cannot say "only this revision". That swap costs one window in
  which the native tree holds a file the index has not recorded (reconcile adopts it, and a purge must not
  treat it as an orphan), in exchange for one provider write instead of five per small file. A provider
  credential is never issued to a principal whose scope is narrower than its reach. See
  `docs/design/transfer-protocol.md`.
- Delete or rewrite an existing migration; only append.
- Change the 5173 port or the `/api` proxy in `web/vite.config.ts` — COS CORS and the development flow depend
  on it.
- Change current runtime auth routing accidentally before its migration. For the replacement ID-or-path
  `/api/files` contract, use the explicit Bearer-or-cookie selection and CSRF rules in `docs/api/api.md`;
  do not add a browser-specific file API or a separate ID-only route family.

## Known traps (already hit)

- `cos-js-sdk-v5` pins a `fast-xml-parser` with an audit advisory; `npm audit` reports critical and there is
  currently no upgrade path.
- STS decodes policy resource paths before comparing, so `+` is treated as a space; `internal/cos` already
  encodes per path segment, so do not regress that when touching signing or policies (otherwise uploads of
  filenames containing `+` return 403).
- macOS filenames are NFD; without normalization you get "two files with the same name". `paths.Normalize`
  converts to NFC.
- Flat object directory moves copy/verify/delete every entry. Each entry copy stays inside the provider (one
  conditional request, or part copies above 5 GiB), so no bytes cross the service, but a large tree still costs
  one request pair per file. Eligible Local native folder moves relink one root; growing/deeper destinations use
  finite fallback. Outside tool moves followed by additive reconcile do not automatically preserve public IDs or
  prune old rows.
- Changing the email address requires two verification codes, for **both the current and the new address**;
  a session alone cannot do it.
- The timeline does not refresh immediately after a move / copy: wait for the sweeper or call
  `POST /api/timeline/rebuild` manually.
- Upgrading from an older version: `sql/013_events.sql` clears the snapshot watermark and queues a recompute
  (it only runs while there are no events yet, so it is harmless if the file is ever re-executed);
  events only appear after the background pass finishes.
