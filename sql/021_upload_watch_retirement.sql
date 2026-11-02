-- Retire confirmed observation work after the browser upload cutover. New file
-- uploads have a server-owned finalization journal and never create these watches.
-- Only unresolved legacy file grants remain observable; credential expiry alone
-- is not proof that their last authenticated native request has finished. Operators
-- must drain old browser sessions before relying on the new finalization contract.
-- Retiring a watch changes no native user data and makes no publication claim.

alter table upload_index_watches
  add column if not exists watch_id uuid not null default gen_random_uuid();

-- A fresh incarnation prevents a stale worker from acknowledging or deleting a
-- new watch recreated at the same path with reset generation and lease counters.
create unique index if not exists upload_index_watches_incarnation
  on upload_index_watches (watch_id);

delete from upload_index_watches where settled = true;
