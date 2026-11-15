-- The service no longer records a client-asserted content digest.
--
-- Why it went: a creation that found the same path, length and recorded digest answered
-- "unchanged" instead of publishing, which made a person's "replace" replace nothing. The
-- digest was an assertion by the owner's client — this service cannot verify it, because a
-- class A publication never sees the bytes and a multipart object's ETag describes its parts
-- — so "same path, same digest" never proved the bytes were the same. If the index and the
-- object had drifted, the skip hid exactly the re-upload that would have repaired it, and the
-- person who chose "replace" was the one not told.
--
-- The gain it bought was narrow (a re-import of unchanged content), and the cost was a third
-- branch in the client's admission path plus a fact nobody could check. A future version that
-- wants it should offer it as its own explicit answer — replace / keep / replace-unless-
-- identical — so the skip is a decision rather than a guess. 054 stays recorded wherever it
-- ran; this drops what it added.
-- Guarded for the same reason 054 was: a database adopted by the bootstrap rule is only
-- assumed to have the shape 001-043 produced, and the migration gate must apply this file
-- there too rather than fail every later release on a table it never had.
do $$
begin
  if exists (select 1 from information_schema.tables
             where table_schema = current_schema() and table_name = 'file_content_generations') then
    alter table file_content_generations drop column if exists content_sha256;
  end if;
end $$;
