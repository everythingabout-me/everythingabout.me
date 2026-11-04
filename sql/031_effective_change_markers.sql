-- Recursive change state is distinct from ancestor namespace movement. A deep
-- sibling edit advances a folder's subtree marker, never an unrelated file's.
create table if not exists file_change_namespace_markers (
 owner_id uuid not null references users(id) on delete cascade,
 entry_id uuid not null,
 position bigint not null,
 change_token uuid not null,
 last_changed_at timestamptz not null,
 primary key(owner_id,entry_id)
);

create or replace function file_effective_change(p_owner uuid,p_entry uuid)
returns table("position" bigint,change_token uuid,last_changed_at timestamptz)
language sql stable as $$
 with recursive ancestry(id,parent_id,depth) as (
  select id,parent_id,0 from nodes where owner_id=p_owner and id=p_entry
  union all
  select parent.id,parent.parent_id,a.depth+1 from nodes parent
  join ancestry a on a.parent_id=parent.id
  where parent.owner_id=p_owner and a.depth<1024
 ), candidates as (
  select m.position,m.change_token,m.last_changed_at from file_change_markers m
  where m.owner_id=p_owner and m.entry_id=p_entry
  union all
  select m.position,m.change_token,m.last_changed_at from file_change_namespace_markers m
  join ancestry a on a.id=m.entry_id where m.owner_id=p_owner
 ) select * from candidates order by position desc limit 1
$$;
