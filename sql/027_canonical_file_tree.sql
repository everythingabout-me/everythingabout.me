-- Canonical parent/name authority permits a native tree rename to relink one row.
-- Existing path/key columns remain nullable derived caches for old import callers.
-- Their unique indexes are retained because the earlier migrations recreate them.
alter table nodes alter column path drop not null;
alter table nodes alter column parent drop not null;
alter table nodes alter column key drop not null;
alter table file_identity_roots add column if not exists tree_revision bigint not null default 0;
alter table file_identity_roots add column if not exists membership_revision bigint not null default 0;
alter table nodes add column if not exists membership_revision bigint not null default 0;

create or replace function file_identity_configure_rules(owner uuid, version text)
returns void language plpgsql as $$
begin
  perform pg_advisory_xact_lock(hashtextextended('file-name-rules:'||owner::text,0));
  insert into file_identity_roots(owner_id,name_comparison_version) values(owner,version)
    on conflict(owner_id) do update set name_comparison_version=excluded.name_comparison_version
    where file_identity_roots.name_comparison_version<>excluded.name_comparison_version;
end $$;
create or replace function file_identity_root_write()
returns trigger language plpgsql as $$
begin
  if NEW.id is distinct from OLD.id or NEW.owner_id is distinct from OLD.owner_id then
    raise exception 'root identity cannot change on update' using errcode='23514';
  end if;
  if NEW.name_comparison_version is distinct from OLD.name_comparison_version
    and not pg_try_advisory_xact_lock(hashtextextended('file-name-rules:'||NEW.owner_id::text,0)) then
    raise exception 'name rules changed concurrently; retry using file_identity_configure_rules' using errcode='40001';
  end if;
  return NEW;
end $$;

create or replace function file_tree_path(owner uuid, entry_id uuid)
returns text language plpgsql stable strict as $$
declare root_id uuid; current_id uuid := entry_id; item record; result text := ''; seen uuid[] := '{}';
begin
  select id into root_id from file_identity_roots where owner_id=owner;
  if root_id is null then return null; end if;
  while current_id<>root_id loop
    if current_id=any(seen) or cardinality(seen)>=64 then
      raise exception 'canonical file ancestry is cyclic or exceeds its bound' using errcode='23514';
    end if;
    seen := array_append(seen,current_id);
    select parent_id,name into item from nodes where owner_id=owner and id=current_id;
    if not found or item.parent_id is null then return null; end if;
    result := case when result='' then item.name else item.name||'/'||result end;
    current_id := item.parent_id;
  end loop;
  return result;
end $$;

create or replace function file_tree_resolve(owner uuid, relative_path text)
returns uuid language plpgsql stable strict as $$
declare current_id uuid; segment text; version text;
begin
  select id,name_comparison_version into current_id,version from file_identity_roots where owner_id=owner;
  if not found then return null; end if;
  if relative_path='' then return current_id; end if;
  foreach segment in array string_to_array(relative_path,'/') loop
    select id into current_id from nodes where owner_id=owner and parent_id=current_id
      and comparison_name=file_identity_comparison(segment,version);
    if not found then return null; end if;
  end loop;
  return current_id;
end $$;

create or replace view file_entries as
select n.id,case when n.owner_id is null then n.key
  else 'drive/'||n.owner_id::text||'/'||resolved.path||case when n.is_dir then '/' else '' end end as key,
  case when n.owner_id is null then n.path else resolved.path end as path,
  case when n.owner_id is null then n.parent else regexp_replace(resolved.path,'(^|/)[^/]+$','') end as parent,
  n.name,n.is_dir,n.size,n.mime,n.etag,n.storage_class,n.created_at,n.updated_at,n.meta,n.owner_id,
  n.source_modified_at,n.parent_id,n.comparison_name,n.membership_revision
from nodes n cross join lateral(select coalesce(file_tree_path(n.owner_id,n.id),n.path) as path) resolved;

create or replace function file_canonical_node_write()
returns trigger language plpgsql as $$
declare version text; parent_path text; expected uuid; existing_id uuid; cached record;
  segment text; ancestor text := ''; ancestor_parent text := ''; current_id uuid; root_id uuid;
begin
  if TG_OP='UPDATE' then
    if NEW.id is distinct from OLD.id or (OLD.owner_id is not null and NEW.owner_id is distinct from OLD.owner_id) then
      raise exception 'file identity and owner cannot change on update' using errcode='23514';
    end if;
  end if;
  if NEW.owner_id is null then
    NEW.parent_id := null; NEW.comparison_name := normalize(NEW.name,NFC); return NEW;
  end if;
  perform pg_advisory_xact_lock_shared(hashtextextended('file-name-rules:'||NEW.owner_id::text,0));
  root_id := file_identity_root(NEW.owner_id);
  select name_comparison_version into strict version from file_identity_roots where owner_id=NEW.owner_id;
  -- NULL invalidates only the advisory cache. Canonical links never follow it.
  if TG_OP='UPDATE' and NEW.path is null then
    if NEW.parent_id is distinct from OLD.parent_id or NEW.name is distinct from OLD.name then
      raise exception 'canonical relink requires an explicit current path' using errcode='23514';
    end if;
    NEW.comparison_name := file_identity_comparison(NEW.name,version); return NEW;
  end if;
  -- Metadata-only updates retain the canonical link even if an ancestor moved.
  if TG_OP='UPDATE' and NEW.path is not distinct from OLD.path and NEW.parent is not distinct from OLD.parent
    and NEW.name is not distinct from OLD.name and NEW.parent_id is not distinct from OLD.parent_id
    and NEW.owner_id is not distinct from OLD.owner_id and NEW.parent_id is not null
    and (NEW.parent_id=root_id or exists(select 1 from nodes where owner_id=NEW.owner_id and id=NEW.parent_id and is_dir)) then
    NEW.comparison_name := file_identity_comparison(NEW.name,version); return NEW;
  end if;
  if NEW.path is null or NEW.path='' or NEW.path ~ '(^/|/$|//|(^|/)\.\.?(/|$))' then
    raise exception 'canonical entry requires a non-root relative logical path' using errcode='23514';
  end if;
  parent_path := regexp_replace(NEW.path,'(^|/)[^/]+$','');
  if NEW.parent<>parent_path or NEW.name<>regexp_replace(NEW.path,'^.*/','') then
    raise exception 'canonical path, parent and name disagree' using errcode='23514';
  end if;
  current_id := root_id;
  if parent_path<>'' then
    foreach segment in array string_to_array(parent_path,'/') loop
      ancestor := case when ancestor='' then segment else ancestor||'/'||segment end;
      select id,is_dir into cached from nodes where owner_id=NEW.owner_id and parent_id=current_id
        and comparison_name=file_identity_comparison(segment,version);
      if not found then
        insert into nodes(owner_id,key,path,parent,name,is_dir)
          values(NEW.owner_id,'drive/'||NEW.owner_id::text||'/'||ancestor||'/',ancestor,ancestor_parent,segment,true)
          on conflict(owner_id,parent_id,comparison_name) do nothing;
        select id,is_dir into strict cached from nodes where owner_id=NEW.owner_id and parent_id=current_id
          and comparison_name=file_identity_comparison(segment,version);
      end if;
      if not cached.is_dir then raise exception 'canonical parent is not a folder' using errcode='23514'; end if;
      current_id := cached.id; ancestor_parent := ancestor;
    end loop;
  end if;
  expected := current_id;
  if (TG_OP='INSERT' and NEW.parent_id is not null and NEW.parent_id<>expected)
    or (TG_OP='UPDATE' and NEW.parent_id is distinct from OLD.parent_id and NEW.parent_id is distinct from expected) then
    raise exception 'canonical parent belongs to another owner or path' using errcode='23514';
  end if;
  if TG_OP='UPDATE' and (expected=NEW.id or exists(
    with recursive ancestors(id,parent_id,depth) as (
      select id,parent_id,0 from nodes where owner_id=NEW.owner_id and id=expected
      union all select n.id,n.parent_id,a.depth+1 from nodes n join ancestors a on n.id=a.parent_id
        where n.owner_id=NEW.owner_id and a.depth<64
    ) select 1 from ancestors where id=NEW.id)) then
    raise exception 'canonical move would create a cycle' using errcode='23514';
  end if;
  NEW.parent_id := expected; NEW.comparison_name := file_identity_comparison(NEW.name,version);
  select id into existing_id from nodes where owner_id=NEW.owner_id and parent_id=expected and comparison_name=NEW.comparison_name;
  if existing_id is not null and (TG_OP='INSERT' or existing_id<>NEW.id) then
    if file_tree_path(NEW.owner_id,existing_id)<>NEW.path then
      raise exception 'canonical sibling spelling conflicts with an existing entry' using errcode='23505';
    end if;
  end if;
  -- Old-path recreation must not collide with a moved descendant's cache.
  update nodes set path=null,parent=null,key=null where owner_id=NEW.owner_id and path=NEW.path
    and id<>NEW.id and (existing_id is null or id<>existing_id);
  return NEW;
end $$;

drop trigger if exists file_identity_node_write on nodes;
drop trigger if exists file_canonical_node_write on nodes;
create trigger file_canonical_node_write before insert or update on nodes
  for each row execute function file_canonical_node_write();
do $$
declare item record;
begin
  for item in select id from nodes where owner_id is not null and parent_id is null and path is not null order by length(path),path loop
    update nodes set name=name where id=item.id;
  end loop;
end $$;

-- Membership changes touch only the old/new direct parent, never descendants.
create or replace function file_tree_membership_changed()
returns trigger language plpgsql as $$
begin
  if TG_OP='UPDATE' and (NEW.owner_id,NEW.parent_id,NEW.name,NEW.is_dir) is not distinct from (OLD.owner_id,OLD.parent_id,OLD.name,OLD.is_dir) then return NEW; end if;
  if TG_OP<>'INSERT' and OLD.owner_id is not null then
    update nodes set membership_revision=membership_revision+1 where owner_id=OLD.owner_id and id=OLD.parent_id;
    update file_identity_roots set membership_revision=membership_revision+1 where owner_id=OLD.owner_id and id=OLD.parent_id;
  end if;
  if TG_OP<>'DELETE' and NEW.owner_id is not null then
    update nodes set membership_revision=membership_revision+1 where owner_id=NEW.owner_id and id=NEW.parent_id;
    update file_identity_roots set membership_revision=membership_revision+1 where owner_id=NEW.owner_id and id=NEW.parent_id;
  end if;
  if TG_OP='DELETE' then return OLD; end if;
  return NEW;
end $$;
drop trigger if exists file_tree_membership_changed on nodes;
create trigger file_tree_membership_changed after insert or update or delete on nodes
  for each row execute function file_tree_membership_changed();

-- Content generations follow the service identity. Native evidence remains the
-- authority for reusing a revision; path is only the last observation's spelling.
alter table file_content_generations add column if not exists node_id uuid references nodes(id) on delete cascade;
alter table file_content_generations drop constraint if exists file_content_generations_pkey;
alter table file_content_generations alter column path drop not null;
create unique index if not exists file_content_generations_path_uniq on file_content_generations(owner_id,root_namespace,path);
update file_content_generations generation set node_id=file_tree_resolve(generation.owner_id,generation.path)
  where generation.node_id is null and generation.path is not null;
create unique index if not exists file_content_generations_node_uniq
  on file_content_generations(owner_id,root_namespace,node_id);
create or replace function file_content_generation_bind_identity()
returns trigger language plpgsql as $$
begin
  if NEW.node_id is null and NEW.path is not null then
    NEW.node_id := file_tree_resolve(NEW.owner_id,NEW.path);
  end if;
  return NEW;
end $$;
drop trigger if exists file_content_generation_bind_identity on file_content_generations;
create trigger file_content_generation_bind_identity before insert or update on file_content_generations
  for each row execute function file_content_generation_bind_identity();
