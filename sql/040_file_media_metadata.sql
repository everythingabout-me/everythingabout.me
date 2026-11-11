-- Capture facts a photo or video states inside its own bytes (time taken, GPS position,
-- device, pixel size, duration, exposure), parsed on demand when someone opens the file
-- and kept here so the next view costs no storage request.
--
-- This is a cache in the strict sense: every row is recomputed from the file by a few
-- ranged reads, so the table may be truncated at any time and nothing a user owns is lost.
-- It is deliberately not filled at upload: the bytes of a presigned upload never cross
-- this service, and most files are never opened.
--
-- A row is valid for one content state and one parser. content_key is the index's own
-- description of the bytes (size plus ETag); parser_version is mediameta.Version. A
-- replaced file or an improved parser makes the stored row a miss, and the next view
-- overwrites it. The row follows the entry through moves (the id is stable) and goes away
-- with it, so a deleted file leaves no coordinates behind.
create table if not exists file_media_metadata (
  owner_id       uuid not null references users(id) on delete cascade,
  node_id        uuid not null references nodes(id) on delete cascade,
  content_key    text not null,
  parser_version integer not null,
  info           jsonb not null,
  parsed_at      timestamptz not null default now(),
  primary key (owner_id, node_id)
);
