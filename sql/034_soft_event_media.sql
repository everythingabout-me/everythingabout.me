-- The stream references files; it does not own them.
--
-- Deleting an index entry used to cascade into event_media, so an event lost the
-- media it was written with even though the event row itself remained. Events are
-- the narrative record and must outlive the files they point at: make the
-- reference soft instead of a hard dependency. Removing a file now leaves the
-- event intact; its payload already carries the path it was written with, and the
-- media join simply resolves nothing for a reference whose file is gone.
--
-- timeline_assets keeps its cascade: it is per-file analysis (time, type,
-- narrative flags) keyed by node, not part of the narrative itself.
alter table event_media alter column node_id drop not null;

alter table event_media drop constraint if exists event_media_node_id_fkey;

do $$
begin
  if not exists (
    select 1 from pg_constraint
     where conrelid = 'event_media'::regclass
       and conname = 'event_media_node_id_fkey'
  ) then
    alter table event_media
      add constraint event_media_node_id_fkey
      foreign key (node_id) references nodes(id) on delete set null;
  end if;
end $$;
