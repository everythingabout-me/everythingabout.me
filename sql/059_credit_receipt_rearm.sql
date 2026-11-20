-- A deletion receipt (a file_credit_claims row held by a delete or by the removal half
-- of a move) records that one native revision's removal was refunded, so that refund
-- must never be earned twice. The row says nothing about a revision that came *back* to
-- the same path afterwards, and nothing released it when it did: settlement released a
-- replacement's own fence (credit.go) but left every earlier receipt alone. A folder
-- moved out and moved back therefore names the identical (path, revision) pairs it left
-- — revisions are derived from content, so unchanged bytes come back with the same
-- revision — and every later delete, move or replacement of those bytes failed with
-- credit_conflict at freeze, with no operator command to clear it.
--
-- Code from this release releases the receipt in the same transaction that settles the
-- presence: an applied copy entry, a published upload, or a native tree move's target
-- prefix (fileops.releaseCredit / releaseCreditSubtree). This file repairs the rows that
-- were already written, so an existing database needs no manual SQL.
--
-- Staleness is proven from the operation journal, not guessed from the index, because the
-- index cannot tell "the bytes were never removed" from "they were removed and put back":
-- a receipt's revision is present at its path in both cases, and only the second may be
-- released. The proof is a succeeded operation that established that path later than the
-- claiming operation:
--
--   * a copy entry, matched on its exact native revision. Every copy of those bytes
--     carries the same content-derived revision, so the match is precise.
--   * an upload, matched on the path alone. The promoted column carries the target but no
--     published revision (sql/047), and the path is enough: the receipt describes bytes
--     that are gone from that path, and a later successful write there can only have put
--     *something else* there — or the identical revision, which is the same event. Bytes
--     that are not at a path cannot be removed from it again without a later, counted,
--     write of their own, so releasing a receipt this way cannot fund a second refund.
--
-- Paths are matched through file_identity_comparison with the owner root's comparison
-- version, the same function that derives comparison_path, so the match is exact for both
-- nfc-sensitive-v1 and nfc-fold-v1. Revisions are compared as decoded bytes rather than
-- through encode(...,'base64'), whose line breaks would make long revisions compare
-- unequal.
--
-- The evidence is materialized once and joined as a set: expressing it as a correlated
-- EXISTS per claim makes the planner rescan the entry journal for every one of the tens of
-- thousands of claims, which is minutes of locked table on the sizes these drives reach.
--
-- A claim whose evidence was already pruned keeps its row: file-operations-prune removes
-- the operation a claim points at, taking the evidence with it, and a missing proof must
-- leave the exclusion in place — refusing a removal is the safe direction, a second
-- refund is not.
with evidence as (
  select 'copy'::text as kind,
         o.owner_id,
         file_identity_comparison(e.target_path, root.name_comparison_version) as comparison_path,
         decode(e.result->'Entry'->>'Revision', 'base64') as version,
         o.created_at
    from file_operation_entries e
    join file_operations o on o.id = e.operation_id and o.owner_id = e.owner_id
    join file_identity_roots root on root.owner_id = o.owner_id
   where e.action = 'copy'
     and e.state = 'applied'
     and o.state = 'succeeded'
  union all
  select 'upload',
         o.owner_id,
         file_identity_comparison(o.intent_target, root.name_comparison_version),
         null::bytea,
         o.created_at
    from file_operations o
    join file_identity_roots root on root.owner_id = o.owner_id
   where o.action = 'upload_commit'
     and o.state = 'succeeded'
     and o.intent_target is not null
), stale as (
  select distinct c.owner_id, c.comparison_path, c.version
    from file_credit_claims c
    join file_operations holder on holder.id = c.operation_id
    join evidence on evidence.owner_id = c.owner_id
                 and evidence.comparison_path = c.comparison_path
                 and (evidence.kind = 'upload' or evidence.version = c.version)
   where holder.phase = 'settled'
     and evidence.created_at > holder.created_at
)
delete from file_credit_claims c
 using stale
 where c.owner_id = stale.owner_id
   and c.comparison_path = stale.comparison_path
   and c.version = stale.version;
