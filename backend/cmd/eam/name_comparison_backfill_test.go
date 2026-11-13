package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/nameprobe"
)

// TestMigrateBackfillsLegacyComparisonPaths runs sql/052 against a database that
// already holds rows written by the legacy key. The other migration tests start
// empty, so a backfill never sees a row; this one seeds the three tables the
// legacy Go pipeline wrote and checks the rewrite, the credit-claim collapse and
// re-execution safety.
func TestMigrateBackfillsLegacyComparisonPaths(t *testing.T) {
	pool := migrateTestPool(t)
	ctx := context.Background()

	before := t.TempDir()
	entries, err := os.ReadDir("../../../sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") || name >= "052_" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("../../../sql", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(before, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runTestMigrate(t, pool, before); err != nil {
		t.Fatalf("migrate to 051: %v", err)
	}

	owner := "44444444-4444-4444-8444-444444444444"
	earlier := "55555555-5555-4555-8555-555555555555"
	later := "66666666-6666-4666-8666-666666666666"
	if _, err := pool.Exec(ctx, `insert into users(id,email,created_at) values($1::uuid,$2,now())`,
		owner, "name-backfill@example.test"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	for _, op := range []struct {
		id        string
		createdAt string
	}{{earlier, "2026-01-01 00:00:00+00"}, {later, "2026-01-02 00:00:00+00"}} {
		if _, err := pool.Exec(ctx, `insert into file_operations(id,owner_id,principal_id,action,fingerprint,snapshot,created_at)
			values ($1::uuid,$2::uuid,'owner-test','delete',$3,'{}'::jsonb,$4::timestamptz)`,
			op.id, owner, "fingerprint-"+op.id, op.createdAt); err != nil {
			t.Fatalf("seed operation %s: %v", op.id, err)
		}
	}

	// The divergent corpus names are the only spellings where the legacy key and
	// the canonical key differ. "ſ̂" (U+017F U+0302) and "Ŝ" (U+015C) are one name
	// under the canonical key, so their legacy spellings collide after the rewrite.
	first, second := "\u017f\u0302", "\u015c"
	legacyFirst := nameprobe.Key(nameprobe.LegacyPipeline, false)(first)
	legacySecond := nameprobe.Key(nameprobe.LegacyPipeline, false)(second)
	canonicalFirst := nameprobe.Key(nameprobe.ReferencePipeline, false)(first)
	canonicalSecond := nameprobe.Key(nameprobe.ReferencePipeline, false)(second)
	if legacyFirst == canonicalFirst || canonicalFirst != canonicalSecond || legacyFirst == legacySecond {
		t.Fatalf("fixture no longer separates the pipelines: legacy %q/%q canonical %q/%q",
			legacyFirst, legacySecond, canonicalFirst, canonicalSecond)
	}
	// A third spelling whose legacy key differs from the canonical key but does
	// not collide with anything: "ẛ̣" (U+1E9B U+0323).
	third := "\u1e9b\u0323"
	legacyThird := nameprobe.Key(nameprobe.LegacyPipeline, false)(third)
	canonicalThird := nameprobe.Key(nameprobe.ReferencePipeline, false)(third)
	if legacyThird == canonicalThird || canonicalThird == canonicalFirst {
		t.Fatalf("third fixture spelling is not an independent rewrite: %q -> %q", legacyThird, canonicalThird)
	}

	if _, err := pool.Exec(ctx, `insert into file_operation_guards(operation_id,owner_id,ordinal,native_path,comparison_path,subtree)
		values($1::uuid,$2::uuid,0,$3,$4,false)`, earlier, owner, "folder/"+first, legacyFirst); err != nil {
		t.Fatalf("seed guard: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into file_uploads(operation_id,owner_id,principal_id,client_fingerprint,comparison_path,
		part_size,part_count,state,created_at,expires_at)
		values($1::uuid,$2::uuid,'owner-test','fixture',$3,4,1,'open',now(),now()+interval '1 hour')`,
		earlier, owner, legacyFirst); err != nil {
		t.Fatalf("seed upload: %v", err)
	}
	for _, claim := range []struct {
		operation string
		path      string
		version   []byte
	}{
		{earlier, legacyFirst, []byte("native-etag:one")},
		{later, legacySecond, []byte("native-etag:one")},
		{earlier, legacyThird, []byte("native-etag:two")},
	} {
		if _, err := pool.Exec(ctx, `insert into file_credit_claims(owner_id,comparison_path,version,operation_id)
			values($1::uuid,$2,$3,$4::uuid)`, owner, claim.path, claim.version, claim.operation); err != nil {
			t.Fatalf("seed claim %q: %v", claim.path, err)
		}
	}

	after := t.TempDir()
	copyMigrations(t, after)
	if _, err := runTestMigrate(t, pool, after); err != nil {
		t.Fatalf("migrate with legacy rows: %v", err)
	}

	for table, query := range map[string]string{
		"file_operation_guards": `select comparison_path from file_operation_guards where owner_id=$1::uuid`,
		"file_uploads":          `select comparison_path from file_uploads where owner_id=$1::uuid`,
	} {
		var stored string
		if err := pool.QueryRow(ctx, query, owner).Scan(&stored); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if stored != canonicalFirst {
			t.Errorf("%s comparison_path=%q, want canonical %q", table, stored, canonicalFirst)
		}
	}

	type claimRow struct {
		operation string
		path      string
		version   string
	}
	rows, err := pool.Query(ctx, `select operation_id::text,comparison_path,encode(version,'escape') from file_credit_claims where owner_id=$1::uuid order by version`, owner)
	if err != nil {
		t.Fatal(err)
	}
	var claims []claimRow
	for rows.Next() {
		var c claimRow
		if err := rows.Scan(&c.operation, &c.path, &c.version); err != nil {
			t.Fatal(err)
		}
		claims = append(claims, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 {
		t.Fatalf("claims after backfill = %+v, want 2 (the colliding pair collapsed to the earlier operation)", claims)
	}
	if claims[0].version != "native-etag:one" || claims[0].operation != earlier || claims[0].path != canonicalFirst {
		t.Errorf("collapsed claim = %+v, want the earlier operation %s with canonical path %q", claims[0], earlier, canonicalFirst)
	}
	if claims[1].version != "native-etag:two" || claims[1].operation != earlier || claims[1].path != canonicalThird {
		t.Errorf("non-colliding claim = %+v, want path %q", claims[1], canonicalThird)
	}

	// A second run is a no-op for the history, and re-executing the file itself
	// (what a lost version table causes) does not rewrite or delete anything.
	output, err := runTestMigrate(t, pool, after)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if !strings.Contains(output, "nothing to apply") {
		t.Fatalf("second migrate was not a no-op:\n%s", output)
	}
	backfill, err := os.ReadFile("../../../sql/052_name_comparison_backfill.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(backfill)); err != nil {
		t.Fatalf("re-execute 052: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `select count(*) from file_credit_claims where owner_id=$1::uuid`, owner).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("re-executing 052 changed claims: %d rows, want 2", count)
	}
}
