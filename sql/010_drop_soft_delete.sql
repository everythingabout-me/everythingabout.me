-- Remove soft delete / trash for good: overwrites and deletes are final, with no hidden state.
--
-- Before upgrading, if the old database still has rows with a non-null deleted_at, run this once
-- with the previous version:
--   eam purge --days 0
-- so the objects go too. This migration only deletes index rows; objects left in the bucket are
-- scanned back in by reconcile.

-- On a migration re-run deleted_at may already be gone, so check before deleting.
do $$
begin
  if exists (
    select 1 from information_schema.columns
     where table_schema = 'public' and table_name = 'nodes' and column_name = 'deleted_at'
  ) then
    delete from nodes where deleted_at is not null;
  end if;
end $$;

-- Drop the indexes that depend on deleted_at
drop index if exists nodes_parent_live;
drop index if exists nodes_deleted;
drop index if exists nodes_owner_parent_live;
drop index if exists nodes_owner_path_live_uniq;
drop index if exists nodes_legacy_path_uniq;

alter table nodes drop column if exists deleted_at;

-- Rebuild the indexes without the soft-delete predicate
create unique index if not exists nodes_owner_path_uniq on nodes (owner_id, path);
create unique index if not exists nodes_legacy_path_uniq on nodes (path) where owner_id is null;
create index if not exists nodes_owner_parent on nodes (owner_id, parent);
create index if not exists nodes_parent on nodes (parent);
