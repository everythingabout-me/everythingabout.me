-- Environment-seeded storage connections keep a reference to their credentials
-- instead of a copy of them.
--
-- storage_connections is a cache: it is dumped, copied off the host, restored into
-- a drill and hand-edited. A usable signing credential must not travel with it, so
-- the row created from the deployment environment stores the names of the
-- variables that hold the credential:
--
--   {"cos":{"secretId":"COS_SECRET_ID","secretKey":"COS_SECRET_KEY"}}
--
-- A row has exactly one source: `secret_reference` is null and `secret` holds an
-- account-added connection's own key (a user-supplied key has no .env to point at),
-- or `secret_reference` names variables and `secret` is empty. The check below is
-- what keeps the second case from silently becoming a second copy: a stale inline
-- value would survive the rotation the reference exists to make possible.
--
-- The conversion of an existing row needs the environment, which a migration does
-- not have, so startup logic performs it after verifying the environment still
-- matches what the row was created from (see connections.AdoptEnvironmentSecret).
--
-- The body is guarded because of the version-table bootstrap rule: a database that
-- predates schema_migrations adopts 001-043 without executing them, and a database
-- older than the release that introduced 035 genuinely has no storage_connections.
-- Failing here would abort every later file for a schema problem this file cannot
-- fix; a no-op keeps the run honest and leaves the missing table to the first query
-- that needs it.
do $do$
begin
  if to_regclass('storage_connections') is null then
    raise notice '045: storage_connections does not exist (035 was adopted without being executed); nothing to alter';
    return;
  end if;
  execute 'alter table storage_connections add column if not exists secret_reference jsonb';
  execute $comment$comment on column storage_connections.secret_reference is 'environment variables that hold this row''s credentials, as a map of credential field to variable name; null when the row carries its own inline secret'$comment$;
  if not exists (
    select 1 from pg_constraint
    where conname = 'storage_connections_secret_source_check'
      and conrelid = 'storage_connections'::regclass
  ) then
    execute $ddl$alter table storage_connections add constraint storage_connections_secret_source_check
      check (
        secret_reference is null or (
          coalesce(secret->'cos'->>'secretId', '') = '' and
          coalesce(secret->'cos'->>'secretKey', '') = '' and
          coalesce(secret->'s3'->>'accessKeyId', '') = '' and
          coalesce(secret->'s3'->>'secretAccessKey', '') = '' and
          coalesce(secret->'s3'->>'sessionToken', '') = ''
        )
      )$ddl$;
  end if;
end
$do$;
