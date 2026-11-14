-- 055 named the cached test result's write-readiness field "backupReady". The
-- measurement is a property of the provider — can the service publish an object
-- through it? — not of the backup feature that happened to be its first user, so
-- the field is "writable". Updating the column comment keeps the stored shape's
-- contract truthful for the next feature that reads it.
--
-- Guarded for the version-table bootstrap rule, like 055: a database that predates
-- schema_migrations may have no storage_connections at all, and failing here would
-- abort later files for a table this file cannot create.
do $do$
begin
  if to_regclass('storage_connections') is null then
    raise notice '056: storage_connections does not exist; nothing to comment';
    return;
  end if;
  execute $comment$comment on column storage_connections.last_probe is 'the last reachability/latency/throughput measurement requested by the account, as {testedAt, ok, error?, latencyMs, uploadBytesPerSecond, downloadBytesPerSecond, writable}; null until tested'$comment$;
end
$do$;
