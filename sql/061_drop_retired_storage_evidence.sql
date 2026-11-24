-- Drop the two storage_connections columns that held evidence this service no longer
-- collects.
--
-- `admission` (035) cached per-connection capability evidence: what a probe had been able to
-- make that endpoint do. e4e7e81 replaced that with a product row in
-- internal/storageproduct, because a request that once succeeded is not knowledge about the
-- software — a missing `If-Match` or a CVE is. Nothing read the column any more, not even as
-- a fallback, so keeping it only leaves a second answer for the next person to trust.
--
-- `last_probe` (055) cached the numbers from the last speed test the server ran. The speed
-- test runs in the browser now, along the path an upload actually takes, and its result
-- describes one person's network at one moment rather than a property of the store; the
-- page shows it once and keeps nothing. A cached copy would print someone else's speed as
-- this provider's.
--
-- Both are caches of disposable measurements, so losing them costs nothing. The body is
-- guarded for the same reason 055's is: a database that predates schema_migrations is
-- adopted without executing 001-043, so a very old one has no storage_connections at all,
-- and failing here would abort every later file for a schema problem this file cannot fix.
do $do$
begin
  if to_regclass('storage_connections') is null then
    raise notice '061: storage_connections does not exist (035 was adopted without being executed); nothing to drop';
    return;
  end if;
  execute 'alter table storage_connections drop column if exists admission';
  execute 'alter table storage_connections drop column if exists last_probe';
end
$do$;
