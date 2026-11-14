-- The storage settings page shows how a destination behaved the last time its
-- owner tested it. That answer is a measurement, so it belongs on the row that
-- names the connection it measured: a cached value elsewhere could outlive a
-- credential or endpoint change and describe a store that no longer exists.
--
-- It is deliberately a cache. Losing this column costs one line of text on the
-- page, never a backup; the backup artifact is the tree under the destination,
-- and no restore ever reads this value.
--
-- The body is guarded because of the version-table bootstrap rule: a database that
-- predates schema_migrations adopts 001-043 without executing them, and a database
-- older than the release that introduced 035 genuinely has no storage_connections.
-- Failing here would abort every later file for a schema problem this file cannot
-- fix; a no-op leaves the missing table to the first query that needs it.
do $do$
begin
  if to_regclass('storage_connections') is null then
    raise notice '055: storage_connections does not exist (035 was adopted without being executed); nothing to alter';
    return;
  end if;
  execute 'alter table storage_connections add column if not exists last_probe jsonb';
  execute $comment$comment on column storage_connections.last_probe is 'the last reachability/latency/throughput measurement requested by the account, as {testedAt, ok, error?, latencyMs, uploadBytesPerSecond, downloadBytesPerSecond, backupReady}; null until tested'$comment$;
end
$do$;
