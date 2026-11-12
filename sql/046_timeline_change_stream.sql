-- Drive the timeline from the durable file change feed instead of periodic scans.
--
-- In a coordinated-writes deployment `StartTimeline` returned immediately, so the
-- organizer never ran and an import through /api/files never reached the stream.
-- The feed already records every settled index transition in the settlement
-- transaction (internal/filechanges), so the timeline becomes one more durable
-- consumer of it: a per-owner checkpoint plus a coalescing 'changes' job.
--
-- `position` is the last feed position the organizer has fully analyzed. It is
-- derived state: dropping it re-reads the retained feed, and a checkpoint below
-- the retained floor (feed pruning, restore, reset) queues one full backfill
-- rather than silently skipping the missing facts.
create table if not exists timeline_change_positions (
  owner_id   uuid primary key references users(id) on delete cascade,
  position   bigint not null default 0,
  updated_at timestamptz not null default now()
);

-- The per-entry revision (`Change.After.Revision`) whose facts produced this row.
-- It makes analysis idempotent per revision: a replayed fact, a retried job or a
-- concurrent scan does not re-read the same entry state. An empty value means
-- "unknown" (a scan wrote these facts) and is always re-analyzed by the feed.
alter table timeline_assets add column if not exists source_revision text not null default '';

-- At most one unfinished 'changes' job per owner, so an upload storm coalesces
-- into one consumer run like the existing scan/backfill/month jobs do.
create unique index if not exists timeline_jobs_changes_pending
  on timeline_jobs (user_id)
  where kind = 'changes' and status in ('pending', 'running');
