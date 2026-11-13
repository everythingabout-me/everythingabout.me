-- Canonical name comparison in Go used to stop after full case folding, while SQL
-- file_identity_comparison (026) applies NFC, full fold, NFC. The trailing NFC only
-- changes a spelling when folding leaves a base letter and a following combining
-- mark that then compose, but that is exactly where the unique sibling index and the
-- Go admission fence disagreed about how many siblings exist. Go now computes the
-- SQL pipeline at every comparison site (internal/namecompare.Key), so every stored
-- comparison_path must be rewritten: the canonical key is the legacy key with one
-- NFC applied, and normalize(value, NFC) is therefore the whole upgrade.
--
-- file_credit_claims can collide after the rewrite: its primary key is
-- (owner_id, comparison_path, version), so two claims for one native revision under
-- two spellings become one key. A revision's size credit may be consumed once, so
-- the earliest claiming operation keeps the claim (created_at, then operation id as a
-- deterministic tiebreak) and the later duplicate rows are deleted. A later
-- operation whose claim disappears can no longer present that receipt: its
-- positive-size delete settles as credit_conflict, which is exactly what losing the
-- race at the primary key does, instead of consuming the same credit twice.
--
-- The release that applies this file also matches stored rows by
-- normalize(comparison_path, NFC) (fileops comparisonShim), because `eam migrate`
-- runs before the service restart and the old binary keeps writing legacy keys for
-- those seconds. Matching the stored value is what covers the whole equivalence
-- class: the legacy key is not a function of the canonical key, so looking for the
-- legacy spelling of the request's own spelling would miss a row the old binary
-- wrote for an index-equal sibling. The expression index below keeps the
-- credit-claim settlement read on an index for that release; the successor drops
-- it, re-runs these updates once more to catch rows written during the window, and
-- deletes the shim.
--
-- In the other direction the old binary may read a row this file already rewrote.
-- For ordinary names the rewrite is a no-op, and for the spellings it does change
-- the old binary's exact lookup already treated the two spellings as different
-- names — the defect being fixed — so the worst case in those seconds (a missed
-- upload supersede, or a credit settlement that answers credit_conflict and is
-- retried) is that pre-existing behaviour, not a new one.
--
-- Re-runnable: after the first pass every update matches no row and the duplicate
-- delete finds nothing, so losing the version table and re-executing this file is
-- harmless.

update file_operation_guards
set comparison_path = normalize(comparison_path, NFC)
where comparison_path is distinct from normalize(comparison_path, NFC);

update file_uploads
set comparison_path = normalize(comparison_path, NFC)
where comparison_path is distinct from normalize(comparison_path, NFC);

delete from file_credit_claims claims
using (
    select claims.owner_id,
           normalize(claims.comparison_path, NFC) as canonical,
           claims.version,
           (array_agg(claims.operation_id order by operation.created_at, operation.id))[1] as keep
    from file_credit_claims claims
    join file_operations operation
      on operation.id = claims.operation_id and operation.owner_id = claims.owner_id
    group by claims.owner_id, normalize(claims.comparison_path, NFC), claims.version
    having count(*) > 1
) duplicates
where claims.owner_id = duplicates.owner_id
  and claims.version = duplicates.version
  and normalize(claims.comparison_path, NFC) = duplicates.canonical
  and claims.operation_id <> duplicates.keep;

update file_credit_claims
set comparison_path = normalize(comparison_path, NFC)
where comparison_path is distinct from normalize(comparison_path, NFC);

-- The applying release reads stored rows through this exact expression. The
-- primary key on (owner_id, comparison_path, version) cannot serve a functional
-- predicate, and settlement reads the table once per entry, so without this
-- index a large delete's settlement would scan every claim of the owner for
-- every entry. The successor drops it with the read shim.
create index if not exists file_credit_claims_name_comparison_shim
    on file_credit_claims(owner_id, normalize(comparison_path, NFC), version);
