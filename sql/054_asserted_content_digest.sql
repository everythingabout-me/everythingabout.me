-- The content digest the owner's client asserts for the bytes a publication puts at an
-- entry, recorded with the settlement that published them.
--
-- Why the service needs one at all: it cannot compute it. A class A publication never
-- passes its bytes through this application, and the provider's ETag is a content MD5 only
-- for a single-part PUT — a multipart object's ETag is a digest of part digests, so it says
-- nothing about the content. Without this, a re-import of a 20k-file tree cannot recognise
-- its own bytes: every unchanged file pays a session, a guard and a settlement.
--
-- Why on the content generation rather than on `nodes`: the browse reads are planned against
-- `nodes`'s shape, and adding a nullable column there flipped the concealed-usage walk from
-- the captured subtree to the per-entry predicate —
-- TestPendingDeleteConcealmentIsBoundedByTheDeletedSubtree measured 321 buffers before and
-- ~4,600 after on a 5,000-entry fixture. The generation row is the node's *content* record,
-- cascades with the node, and is deleted whenever the entry loses its content, which is
-- exactly the lifetime this digest has.
--
-- What it is: an assertion by the owner's client, never evidence. It answers one question —
-- may this creation skip its transfer — and is not used for identity, authorization, audit
-- or change detection. A row without one simply never skips, and a publication that carries
-- no digest writes null instead of inheriting the previous file's.
-- Guarded: a database adopted by the bootstrap rule (schema, no history) is only assumed to
-- have the shape 001-043 produced, and the migration gate must still apply this file there.
-- A tree without the table is one this service cannot serve at all, so it stays a runtime
-- finding rather than a migration failure that blocks every later release.
do $$
begin
  if exists (select 1 from information_schema.tables
             where table_schema = current_schema() and table_name = 'file_content_generations') then
    alter table file_content_generations add column if not exists content_sha256 text;
    alter table file_content_generations drop constraint if exists file_content_generations_sha256_check;
    alter table file_content_generations add constraint file_content_generations_sha256_check
      check (content_sha256 is null or content_sha256 ~ '^[0-9a-f]{64}$');
  end if;
end $$;
