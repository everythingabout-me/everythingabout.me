-- Persist owner/principal-bound file subscriptions and explicit incremental grants.
-- Existing API tokens gain no new authority when this migration is reapplied.
alter table api_tokens add column if not exists can_changes boolean not null default false;
alter table api_tokens add column if not exists can_subscribe boolean not null default false;
alter table api_tokens add column if not exists policy_version bigint not null default 1;

create table if not exists file_subscriptions (
  id uuid primary key default gen_random_uuid(),
  owner_id uuid not null references users(id) on delete cascade,
  principal_id text not null,
  root_namespace text not null,
  epoch uuid not null,
  policy_fingerprint text not null,
  mode text not null check (mode in ('id','path')),
  selector_id uuid,
  selector_path text,
  anchor_path text not null,
  kind text not null check (kind in ('file','folder')),
  recursive boolean not null default false,
  start_position bigint not null check (start_position >= 0),
  signal_position bigint not null check (signal_position >= start_position),
  signal_anchor_path text,
  last_notification_id uuid,
  ended_reason text not null default '' check (ended_reason in ('','deleted','access_lost')),
  created_at timestamptz not null default now(),
  expires_at timestamptz not null default now() + interval '7 days',
  check ((mode='id' and selector_id is not null and selector_path is null)
      or (mode='path' and selector_id is null and selector_path is not null)),
  check (kind='folder' or not recursive)
);
-- Legacy in-progress subscriptions inherit their original anchor once. Future
-- matcher progress never changes the consumer's immutable baseline anchor.
alter table file_subscriptions add column if not exists signal_anchor_path text;
update file_subscriptions set signal_anchor_path=anchor_path where signal_anchor_path is null;

create index if not exists file_subscriptions_principal
  on file_subscriptions(owner_id,principal_id,root_namespace,id);
create index if not exists file_subscriptions_expiry on file_subscriptions(expires_at);

-- Both event endpoints are authorized independently before a public page limit.
-- Text comparisons reuse the identity comparison version, including Unicode fold.
create or replace function file_subscription_visible(scope_path text,entry_path text,hidden_paths text[],comparison_version text)
returns boolean language sql immutable as $$
 select coalesce(entry_path is not null
   and file_changes_within(scope_path,entry_path,comparison_version)
   and not exists(select 1 from unnest(hidden_paths) hidden(path)
     where file_changes_within(hidden.path,entry_path,comparison_version)),false)
$$;

create or replace function file_subscription_matches(snapshot jsonb,ancestors jsonb,
 selector_mode text,selected_id text,selected_path text,folder boolean,recursive boolean,comparison_version text)
returns boolean language sql immutable as $$
 select coalesce(snapshot is not null and (
   (selector_mode='id' and (snapshot->>'id'=selected_id
     or (folder and not recursive and snapshot->>'parentId'=selected_id)
     or (folder and recursive and exists(select 1 from jsonb_array_elements(ancestors) ancestor
       where ancestor->>'id'=selected_id))))
   or (selector_mode='path' and (
     file_identity_comparison(snapshot->>'path',comparison_version)=file_identity_comparison(selected_path,comparison_version)
     or (folder and recursive and file_changes_within(selected_path,snapshot->>'path',comparison_version))
     or (folder and not recursive and file_identity_comparison(snapshot->>'parentPath',comparison_version)=file_identity_comparison(selected_path,comparison_version))
   ))),false)
$$;
