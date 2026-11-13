-- Promote the operation-journal fields that SQL filters on into real columns.
--
-- Why: `file_operations.snapshot` is the whole Go `Operation` document, and five packages
-- asked it questions by field name (`snapshot->>'State'`, `snapshot->'Root'->>'Generation'`,
-- `snapshot->'Purge'->>'Path'`, ...). Renaming an Operation field therefore compiled, passed
-- every test, and silently stopped every suspended operation from being recovered: the
-- predicate compared NULL to a literal and matched nothing. The same shape made every
-- settlement rewrite the whole document several times per operation while holding the owner
-- lock — a 5000-entry move rewrote roughly 10 MB per transition — and nothing ever read
-- `RetainUntil`, so terminal rows accumulated forever (sql/048 and the
-- `file-operations-prune` command are its first reader).
--
-- What this file does:
--   1. adds the columns named by every existing predicate;
--   2. backfills them from the documents already in the table;
--   3. rebuilds the three partial indexes (041, 042, 043) over the columns, plus the new
--      indexes the moved predicates need.
--
-- Authority: the columns are authoritative for every SQL predicate; `snapshot` stays the
-- operation document (intent, result, guards, entries, problem, and the promoted fields
-- too, so a document still reads as a whole). `saveOperation` writes the columns and the
-- document in one UPDATE, so a row written by this binary cannot disagree. A row written
-- by a pre-split binary during the deploy window (migrate, then restart) or by corruption
-- can disagree: it is never treated as proof that an operation is finished — the merge
-- path rewrites the columns from the document whenever it loads one, and `Claim` repairs
-- and skips it once (so a settled operation leaves the pending scan instead of being
-- retried forever). Nothing deletes a row on the strength of a disagreeing column: the
-- operation document stays the authority for what the operation is.
--
-- The guard against a regression is a lint test, not a convention:
-- internal/fileops.TestOperationJournalPredicatesReadColumns fails on any arrow read from a
-- bare `snapshot` column, in backend Go sources and in migrations appended after this file.
-- A new predicate must name a column, and adding one starts here.
--
-- Field-by-field mapping (the JSON names are the Go field names as they existed before
-- this file; they are spelled only here, once, as history):
--   state                <- snapshot->>'State'
--   phase                <- snapshot->>'Phase'
--   created_at           <- snapshot->>'CreatedAt'
--   finished_at          <- snapshot->>'FinishedAt'
--   retain_until         <- snapshot->>'RetainUntil'
--   cleanup_attempted_at <- snapshot->>'CleanupAttemptedAt'
--   cleaned_at           <- snapshot->>'CleanedAt'
--   lease_token          <- snapshot->'Lease'->>'Token'
--   lease_expires_at     <- snapshot->'Lease'->>'ExpiresAt'
--   root_connection_id   <- snapshot->'Root'->>'ConnectionID'
--   root_generation      <- snapshot->'Root'->>'Generation'
--   root_content_root    <- snapshot->'Root'->>'ContentRoot'
--   root_system_root     <- snapshot->'Root'->>'SystemRoot'
--   root_case_sensitive  <- snapshot->'Root'->>'CaseSensitive'
--   root_retains_versions<- snapshot->'Root'->>'RetainsVersions'
--   root_namespace       <- Root.Namespace() (generated from the columns above + owner_id)
--   purge_path           <- snapshot->'Purge'->>'Path'
--   intent_target        <- snapshot->'Intent'->>'Target'
--   stage_size           <- snapshot->'Stage'->>'Size'
--   stage_handle         <- base64-decoded snapshot->'Stage'->>'Handle'
--
-- `action` was already a real column and is what the cleanup predicate reads.
--
-- The columns are added nullable, backfilled, and only then made NOT NULL: every promoted
-- column is added by this file as one set, so a NULL `state` is exactly "this row predates
-- the split". The second run of this file therefore finds no NULL state and does not touch
-- existing values (it must not: a later write heals the columns from the document, and a
-- backfill that overwrote them again could undo that). The defaults exist for one window:
-- a pre-split binary still inserts rows while `migrate` and the restart overlap, and those
-- inserts must not fail on a NOT NULL column they know nothing about. A row inserted that
-- way starts as queued/admitted and is repaired by the first write or claim, like any other
-- disagreeing row.
--
-- 041/042/043 are replaced because their keys were JSON expressions. Their names are kept
-- so the reasoning in those files (and the code comments that mention the index names)
-- still points at one index. A partial index stays the right shape: an unsettled operation,
-- an uncleaned terminal upload and a failed publication are all small left-over sets, while
-- the journal itself deliberately keeps every terminal row.

alter table file_operations add column if not exists state text;
alter table file_operations add column if not exists phase text;
alter table file_operations add column if not exists created_at timestamptz;
alter table file_operations add column if not exists finished_at timestamptz;
alter table file_operations add column if not exists retain_until timestamptz;
alter table file_operations add column if not exists cleanup_attempted_at timestamptz;
alter table file_operations add column if not exists cleaned_at timestamptz;
alter table file_operations add column if not exists lease_token text;
alter table file_operations add column if not exists lease_expires_at timestamptz;
alter table file_operations add column if not exists root_connection_id text;
alter table file_operations add column if not exists root_generation bigint;
alter table file_operations add column if not exists root_content_root text;
alter table file_operations add column if not exists root_system_root text;
alter table file_operations add column if not exists root_case_sensitive boolean;
alter table file_operations add column if not exists root_retains_versions boolean;
alter table file_operations add column if not exists purge_path text;
alter table file_operations add column if not exists intent_target text;
alter table file_operations add column if not exists stage_size bigint;
alter table file_operations add column if not exists stage_handle bytea;

-- Exactly Root.Namespace() from Go. Generated, so it cannot drift from the root columns the
-- writer sets, and a reader can compare one value instead of reapplying concat_ws in SQL.
alter table file_operations add column if not exists root_namespace text
  generated always as (
    owner_id::text || ':' || root_connection_id || ':' || root_generation::text || ':' ||
    root_content_root || ':' || root_system_root
  ) stored;

update file_operations o set
  state = s.state,
  phase = s.phase,
  created_at = s.created_at,
  finished_at = s.finished_at,
  retain_until = s.retain_until,
  cleanup_attempted_at = s.cleanup_attempted_at,
  cleaned_at = s.cleaned_at,
  lease_token = s.lease_token,
  lease_expires_at = s.lease_expires_at,
  root_connection_id = s.root_connection_id,
  root_generation = s.root_generation,
  root_content_root = s.root_content_root,
  root_system_root = s.root_system_root,
  root_case_sensitive = s.root_case_sensitive,
  root_retains_versions = s.root_retains_versions,
  purge_path = s.purge_path,
  intent_target = s.intent_target,
  stage_size = s.stage_size,
  stage_handle = s.stage_handle
from (
  select id,
    snapshot->>'State' as state,
    snapshot->>'Phase' as phase,
    (snapshot->>'CreatedAt')::timestamptz as created_at,
    (snapshot->>'FinishedAt')::timestamptz as finished_at,
    (snapshot->>'RetainUntil')::timestamptz as retain_until,
    (snapshot->>'CleanupAttemptedAt')::timestamptz as cleanup_attempted_at,
    (snapshot->>'CleanedAt')::timestamptz as cleaned_at,
    coalesce(snapshot->'Lease'->>'Token','') as lease_token,
    (snapshot->'Lease'->>'ExpiresAt')::timestamptz as lease_expires_at,
    coalesce(snapshot->'Root'->>'ConnectionID','') as root_connection_id,
    coalesce((snapshot->'Root'->>'Generation')::bigint,0) as root_generation,
    coalesce(snapshot->'Root'->>'ContentRoot','') as root_content_root,
    coalesce(snapshot->'Root'->>'SystemRoot','') as root_system_root,
    coalesce((snapshot->'Root'->>'CaseSensitive')::boolean,true) as root_case_sensitive,
    coalesce((snapshot->'Root'->>'RetainsVersions')::boolean,false) as root_retains_versions,
    snapshot->'Purge'->>'Path' as purge_path,
    snapshot->'Intent'->>'Target' as intent_target,
    (snapshot->'Stage'->>'Size')::bigint as stage_size,
    decode(snapshot->'Stage'->>'Handle','base64') as stage_handle
  from file_operations
  where state is null
) s
where o.id = s.id;

-- NOT NULL is what makes the predicates total: an operation always has a state and a
-- creation instant, so a missing one is a backfill bug and should stop this file.
alter table file_operations alter column state set default 'queued';
alter table file_operations alter column state set not null;
alter table file_operations alter column phase set default 'admitted';
alter table file_operations alter column phase set not null;
alter table file_operations alter column created_at set default clock_timestamp();
alter table file_operations alter column created_at set not null;
alter table file_operations alter column retain_until set default clock_timestamp();
alter table file_operations alter column retain_until set not null;
alter table file_operations alter column lease_token set default '';
alter table file_operations alter column lease_token set not null;
alter table file_operations alter column root_connection_id set default '';
alter table file_operations alter column root_connection_id set not null;
alter table file_operations alter column root_generation set default 0;
alter table file_operations alter column root_generation set not null;
alter table file_operations alter column root_content_root set default '';
alter table file_operations alter column root_content_root set not null;
alter table file_operations alter column root_system_root set default '';
alter table file_operations alter column root_system_root set not null;
alter table file_operations alter column root_case_sensitive set default true;
alter table file_operations alter column root_case_sensitive set not null;
alter table file_operations alter column root_retains_versions set default false;
alter table file_operations alter column root_retains_versions set not null;

-- The settlement scanner: (created_at, id) is now the scan's real filter and order, so the
-- plan reads the candidates in order and stops at its page limit instead of sorting the
-- journal. Same partial shape and reason as 041, same index name so its comment still applies.
drop index if exists file_operations_pending;
create index file_operations_pending on file_operations (created_at, id)
 where state in ('queued','running','outcome_unknown');

-- The cleanup sweep: (cleanup_attempted_at, finished_at) is the rotation key the sweep
-- ordered by, and it is now a column expression an index can hold, so the ORDER BY no
-- longer sorts the whole candidate set. Same partial predicate as 042.
drop index if exists file_operations_cleanup;
create index file_operations_cleanup on file_operations (coalesce(cleanup_attempted_at,finished_at), id)
 where action='upload_commit' and state in ('succeeded','failed','cancelled') and cleaned_at is null;

-- The publication-reclaim lookup: the target is an equality filter on a real column and a
-- stage is present exactly when its handle is (SaveStage rejects an empty handle). Same
-- partial shape and reason as 043.
drop index if exists file_operations_reclaim;
create index file_operations_reclaim on file_operations (owner_id, intent_target)
 where action = 'upload_commit' and state = 'failed' and stage_handle is not null;

-- The pending-delete reads (fileops.PendingDeleteRoots and the API's delete overlay) ask
-- one owner for the purge roots of its non-terminal deletes in one native namespace. Both
-- filters are columns now, so the answer no longer scans the owner's terminal journal.
create index if not exists file_operations_purge on file_operations (owner_id, root_namespace)
 where action = 'delete' and state in ('queued','running','outcome_unknown') and purge_path is not null;

