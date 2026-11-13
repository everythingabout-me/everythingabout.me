package nodes

// Probe suite for docs/design/file-identity.md §"Write-path audit". It records what the
// nodes BEFORE trigger makes true, not what the application currently writes: every probe
// inserts the legacy cache columns (path/parent/name) exactly as the settlement code does
// and then asks the index where the entry came from.
//
// The suite is deliberately independent of the service structs. The question is about the
// schema contract (trigger, unique sibling index, file_entries view), so a probe that
// called a helper could pass while the trigger it is supposed to exercise was never reached.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/namecompare"
	"everythingabout.me/backend/internal/testpg"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func identityAuditDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	scratch := testpg.CreateDatabase(t, "eam_identity_audit_")
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatalf("migrations unavailable: %v", err)
	}
	// Twice: eam migrate applies each file once, and a bootstrap database may re-execute
	// files from 044 on. The identity migration must survive both.
	for pass := 0; pass < 2; pass++ {
		for _, migration := range migrations {
			data, err := os.ReadFile(migration)
			if err != nil {
				t.Fatalf("read %s: %v", migration, err)
			}
			if _, err := pool.Exec(ctx, string(data)); err != nil {
				t.Fatalf("migration pass %d %s: %v", pass+1, filepath.Base(migration), err)
			}
		}
	}
	return pool
}

func identityAuditOwner(t *testing.T, pool *pgxpool.Pool, rules string) string {
	t.Helper()
	ctx := context.Background()
	var owner string
	if err := pool.QueryRow(ctx, `insert into users(email) values(gen_random_uuid()::text||'@identity-audit.test') returning id::text`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if rules != "" {
		if _, err := pool.Exec(ctx, `select file_identity_configure_rules($1::uuid,$2)`, owner, rules); err != nil {
			t.Fatalf("configure %s: %v", rules, err)
		}
	}
	return owner
}

// identityAuditInsert writes a row the way every settlement path does: it supplies the
// legacy cache columns and nothing canonical.
func identityAuditInsert(t *testing.T, pool *pgxpool.Pool, owner, path, parent, name string, folder bool) string {
	t.Helper()
	key := "drive/" + owner + "/" + path
	size := int64(3)
	if folder {
		key += "/"
		size = 0
	}
	var id string
	if err := pool.QueryRow(context.Background(),
		`insert into nodes(owner_id,key,path,parent,name,is_dir,size)
		 values($1::uuid,$2,$3,$4,$5,$6,$7) returning id::text`,
		owner, key, path, parent, name, folder, size).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", path, err)
	}
	return id
}

// identityAuditRow is the canonical plus cached state of one row, read the way the
// service reads it (through the view) and the way the trigger stores it (from nodes).
type identityAuditRow struct {
	ID             string
	ParentID       *string
	ComparisonName *string
	StoredPath     *string
	StoredParent   *string
	ViewPath       *string
	ViewParent     *string
	ViewParentID   *string
	ViewComparison *string
	ViewKey        *string
}

func identityAuditRead(t *testing.T, pool *pgxpool.Pool, owner, id string) identityAuditRow {
	t.Helper()
	var row identityAuditRow
	err := pool.QueryRow(context.Background(),
		`select n.id::text,n.parent_id::text,n.comparison_name,n.path,n.parent,
		        v.path,v.parent,v.parent_id::text,v.comparison_name,v.key
		   from nodes n join file_entries v on v.id=n.id and v.owner_id=n.owner_id
		  where n.owner_id=$1::uuid and n.id=$2::uuid`, owner, id).
		Scan(&row.ID, &row.ParentID, &row.ComparisonName, &row.StoredPath, &row.StoredParent,
			&row.ViewPath, &row.ViewParent, &row.ViewParentID, &row.ViewComparison, &row.ViewKey)
	if err != nil {
		t.Fatalf("read entry %s: %v", id, err)
	}
	return row
}

func identityAuditResolve(t *testing.T, pool *pgxpool.Pool, owner, path string) *string {
	t.Helper()
	var id *string
	if err := pool.QueryRow(context.Background(), `select file_tree_resolve($1::uuid,$2)::text`, owner, path).Scan(&id); err != nil {
		t.Fatalf("resolve %q: %v", path, err)
	}
	return id
}

func identityAuditRoot(t *testing.T, pool *pgxpool.Pool, owner string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `select file_identity_root($1::uuid)::text`, owner).Scan(&id); err != nil {
		t.Fatalf("root: %v", err)
	}
	return id
}

func identityAuditString(t *testing.T, value *string) string {
	t.Helper()
	if value == nil {
		t.Fatal("expected a value, got NULL")
	}
	return *value
}

func TestCanonicalIdentityWritePathAudit(t *testing.T) {
	pool := identityAuditDatabase(t)
	ctx := context.Background()

	// The trigger is the only writer of the canonical columns today. Pin that fact first:
	// every probe below is about the consequences of this one line being true.
	var triggerDefined bool
	if err := pool.QueryRow(ctx, `select exists(select 1 from pg_trigger where tgrelid='nodes'::regclass
		and tgname='file_canonical_node_write' and not tgisinternal)`).Scan(&triggerDefined); err != nil {
		t.Fatal(err)
	}
	if !triggerDefined {
		t.Fatal("file_canonical_node_write is not installed: the audit's facts no longer describe this schema")
	}
	var viewDef string
	if err := pool.QueryRow(ctx, `select pg_get_viewdef('file_entries'::regclass,true)`).Scan(&viewDef); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(viewDef), "COALESCE(FILE_TREE_PATH") {
		t.Fatalf("file_entries no longer falls back to the cached path; re-derive the audit before trusting it:\n%s", viewDef)
	}

	t.Run("insert derives canonical identity from the settlement path", func(t *testing.T) {
		owner := identityAuditOwner(t, pool, "")
		root := identityAuditRoot(t, pool, owner)
		folder := identityAuditInsert(t, pool, owner, "Notes", "", "Notes", true)
		file := identityAuditInsert(t, pool, owner, "Notes/a.txt", "Notes", "a.txt", false)

		row := identityAuditRead(t, pool, owner, file)
		if row.ParentID == nil || *row.ParentID != folder {
			t.Fatalf("parent_id %v, want the Notes row %s", row.ParentID, folder)
		}
		if row.ComparisonName == nil || *row.ComparisonName != "a.txt" {
			t.Fatalf("comparison_name %v, want a.txt", row.ComparisonName)
		}
		if got := identityAuditString(t, row.ViewPath); got != "Notes/a.txt" {
			t.Fatalf("view path %q", got)
		}
		if got := identityAuditString(t, row.ViewParent); got != "Notes" {
			t.Fatalf("view parent %q", got)
		}
		if id := identityAuditResolve(t, pool, owner, "Notes/a.txt"); id == nil || *id != file {
			t.Fatalf("resolve returned %v, want %s", id, file)
		}
		// The folder row's parent is the identity root, not a node row.
		folderRow := identityAuditRead(t, pool, owner, folder)
		if folderRow.ParentID == nil || *folderRow.ParentID != root {
			t.Fatalf("folder parent %v, want root %s", folderRow.ParentID, root)
		}
		// The view derives the key, so a stale key column cannot change where bytes live.
		if got := identityAuditString(t, row.ViewKey); got != "drive/"+owner+"/Notes/a.txt" {
			t.Fatalf("view key %q", got)
		}
	})

	t.Run("rename keeps the ID and reparents on move", func(t *testing.T) {
		owner := identityAuditOwner(t, pool, "")
		_ = identityAuditInsert(t, pool, owner, "Notes", "", "Notes", true)
		archive := identityAuditInsert(t, pool, owner, "Archive", "", "Archive", true)
		file := identityAuditInsert(t, pool, owner, "Notes/a.txt", "Notes", "a.txt", false)
		notesParent := identityAuditString(t, identityAuditRead(t, pool, owner, file).ParentID)

		if _, err := pool.Exec(ctx, `update nodes set key=$3,path='Notes/b.txt',parent='Notes',name='b.txt'
			where owner_id=$1::uuid and id=$2::uuid`, owner, file, "drive/"+owner+"/Notes/b.txt"); err != nil {
			t.Fatalf("rename: %v", err)
		}
		row := identityAuditRead(t, pool, owner, file)
		if got := identityAuditString(t, row.ViewPath); got != "Notes/b.txt" {
			t.Fatalf("rename view path %q", got)
		}
		if row.ParentID == nil || *row.ParentID != notesParent {
			t.Fatalf("rename changed parent_id: %v -> %v", notesParent, row.ParentID)
		}
		if row.ComparisonName == nil || *row.ComparisonName != "b.txt" {
			t.Fatalf("comparison_name %v after rename", row.ComparisonName)
		}
		if id := identityAuditResolve(t, pool, owner, "Notes/a.txt"); id != nil {
			t.Fatalf("old spelling still resolves to %v", id)
		}

		if _, err := pool.Exec(ctx, `update nodes set key=$3,path='Archive/b.txt',parent='Archive',name='b.txt'
			where owner_id=$1::uuid and id=$2::uuid`, owner, file, "drive/"+owner+"/Archive/b.txt"); err != nil {
			t.Fatalf("move: %v", err)
		}
		row = identityAuditRead(t, pool, owner, file)
		if row.ParentID == nil || *row.ParentID != archive {
			t.Fatalf("move parent_id %v, want Archive %s", row.ParentID, archive)
		}
		if got := identityAuditString(t, row.ViewPath); got != "Archive/b.txt" {
			t.Fatalf("move view path %q", got)
		}
	})

	t.Run("subtree move relinks one row and descendants follow ancestry", func(t *testing.T) {
		owner := identityAuditOwner(t, pool, "")
		photos := identityAuditInsert(t, pool, owner, "Photos", "", "Photos", true)
		year := identityAuditInsert(t, pool, owner, "Photos/2024", "Photos", "2024", true)
		photo := identityAuditInsert(t, pool, owner, "Photos/2024/x.jpg", "Photos/2024", "x.jpg", false)
		media := identityAuditInsert(t, pool, owner, "Media", "", "Media", true)

		// The eligible native-tree move is one row update; descendants keep their old cache
		// columns and their parent links.
		if _, err := pool.Exec(ctx, `update nodes set key=$3,path='Media/Photos',parent='Media',name='Photos'
			where owner_id=$1::uuid and id=$2::uuid`, owner, photos, "drive/"+owner+"/Media/Photos/"); err != nil {
			t.Fatalf("tree move: %v", err)
		}
		root := identityAuditRead(t, pool, owner, photos)
		if root.ParentID == nil || *root.ParentID != media {
			t.Fatalf("tree move parent_id %v, want Media %s", root.ParentID, media)
		}
		yearRow := identityAuditRead(t, pool, owner, year)
		if yearRow.ParentID == nil || *yearRow.ParentID != photos {
			t.Fatalf("descendant parent_id changed: %v", yearRow.ParentID)
		}
		child := identityAuditRead(t, pool, owner, photo)
		if child.StoredPath == nil || *child.StoredPath != "Photos/2024/x.jpg" {
			t.Fatalf("descendant cache was rewritten to %v; the tree move is supposed to leave it stale", child.StoredPath)
		}
		if got := identityAuditString(t, child.ViewPath); got != "Media/Photos/2024/x.jpg" {
			t.Fatalf("descendant current path %q", got)
		}
		if id := identityAuditResolve(t, pool, owner, "Media/Photos/2024/x.jpg"); id == nil || *id != photo {
			t.Fatalf("descendant does not resolve at the new path: %v", id)
		}
	})

	t.Run("re-creation allocates a fresh ID and clears the old path cache", func(t *testing.T) {
		owner := identityAuditOwner(t, pool, "")
		photos := identityAuditInsert(t, pool, owner, "Photos", "", "Photos", true)
		year := identityAuditInsert(t, pool, owner, "Photos/2024", "Photos", "2024", true)
		old := identityAuditInsert(t, pool, owner, "Photos/2024/x.jpg", "Photos/2024", "x.jpg", false)
		_ = identityAuditInsert(t, pool, owner, "Media", "", "Media", true)
		if _, err := pool.Exec(ctx, `update nodes set key=$3,path='Media/Photos',parent='Media',name='Photos'
			where owner_id=$1::uuid and id=$2::uuid`, owner, photos, "drive/"+owner+"/Media/Photos/"); err != nil {
			t.Fatalf("tree move: %v", err)
		}

		recreated := identityAuditInsert(t, pool, owner, "Photos/2024/x.jpg", "Photos/2024", "x.jpg", false)
		if recreated == old {
			t.Fatal("re-creation reused the old ID")
		}
		if id := identityAuditResolve(t, pool, owner, "Photos/2024/x.jpg"); id == nil || *id != recreated {
			t.Fatalf("the old path resolves to %v, want the new row %s", id, recreated)
		}
		// The trigger nulls the moved descendants' cache columns so the re-created path can
		// take over the unique (owner_id, path) slot. Their canonical path is still derived.
		for _, id := range []string{year, old} {
			row := identityAuditRead(t, pool, owner, id)
			if row.StoredPath != nil {
				t.Fatalf("retained descendant %s still caches %q after re-creation", id, *row.StoredPath)
			}
			if row.ViewPath == nil || !strings.HasPrefix(*row.ViewPath, "Media/Photos/") {
				t.Fatalf("retained descendant %s lost its canonical path: %v", id, row.ViewPath)
			}
		}
	})

	t.Run("NFD spelling is preserved while the comparison key is NFC", func(t *testing.T) {
		owner := identityAuditOwner(t, pool, "")
		nfd := "cafe\u0301.txt" // NFD: e + combining acute
		nfc := "café.txt"
		id := identityAuditInsert(t, pool, owner, nfd, "", nfd, false)

		row := identityAuditRead(t, pool, owner, id)
		if row.ComparisonName == nil || *row.ComparisonName != nfc {
			t.Fatalf("comparison_name %q, want NFC %q", identityAuditString(t, row.ComparisonName), nfc)
		}
		if row.StoredPath == nil || *row.StoredPath != nfd {
			t.Fatalf("stored spelling %q, want the accepted NFD spelling", identityAuditString(t, row.StoredPath))
		}
		if got := identityAuditString(t, row.ViewPath); got != nfd {
			t.Fatalf("view path %q, want the accepted spelling", got)
		}
		for _, spelling := range []string{nfd, nfc} {
			if got := identityAuditResolve(t, pool, owner, spelling); got == nil || *got != id {
				t.Fatalf("resolve(%q) = %v, want %s", spelling, got, id)
			}
		}
	})

	t.Run("case-insensitive root rejects an equivalent sibling", func(t *testing.T) {
		owner := identityAuditOwner(t, pool, "nfc-fold-v1")
		id := identityAuditInsert(t, pool, owner, "README", "", "README", false)

		var pgErr *pgconn.PgError
		_, err := pool.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size)
			values($1::uuid,$2,'readme','','readme',false,3)`, owner, "drive/"+owner+"/readme")
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" || !strings.Contains(pgErr.Message, "canonical sibling spelling conflicts") {
			t.Fatalf("equivalent sibling was not rejected by the canonical index: %v", err)
		}
		for _, spelling := range []string{"README", "ReadMe", "readme"} {
			if got := identityAuditResolve(t, pool, owner, spelling); got == nil || *got != id {
				t.Fatalf("resolve(%q) = %v, want %s", spelling, got, id)
			}
		}
		// Full folding, not lower(): ß folds to ss under nfc-fold-v1, so Straße and
		// STRASSE are one sibling even though lower() keeps them distinct.
		_ = identityAuditInsert(t, pool, owner, "Straße", "", "Straße", false)
		_, err = pool.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size)
			values($1::uuid,$2,'STRASSE','','STRASSE',false,3)`, owner, "drive/"+owner+"/STRASSE")
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("full-fold equivalent sibling was not rejected: %v", err)
		}
	})

	t.Run("NULL path cache is a cache: identity still answers", func(t *testing.T) {
		owner := identityAuditOwner(t, pool, "")
		_ = identityAuditInsert(t, pool, owner, "Media", "", "Media", true)
		_ = identityAuditInsert(t, pool, owner, "Media/Photos", "Media", "Photos", true)
		_ = identityAuditInsert(t, pool, owner, "Media/Photos/2024", "Media/Photos", "2024", true)
		file := identityAuditInsert(t, pool, owner, "Media/Photos/2024/x.jpg", "Media/Photos/2024", "x.jpg", false)

		// The trigger allows a metadata-only update with a NULL cache; the link is unchanged.
		if _, err := pool.Exec(ctx, `update nodes set path=null,parent=null,key=null,size=size+1
			where owner_id=$1::uuid and id=$2::uuid`, owner, file); err != nil {
			t.Fatalf("null the cache: %v", err)
		}
		row := identityAuditRead(t, pool, owner, file)
		if row.StoredPath != nil {
			t.Fatalf("cache was not nulled: %q", *row.StoredPath)
		}
		if got := identityAuditString(t, row.ViewPath); got != "Media/Photos/2024/x.jpg" {
			t.Fatalf("view path with NULL cache %q", got)
		}
		if got := identityAuditString(t, row.ViewParent); got != "Media/Photos/2024" {
			t.Fatalf("view parent with NULL cache %q", got)
		}
		// A NULL cache plus an explicit relink is refused, because a relink needs a path.
		_, err := pool.Exec(ctx, `update nodes set path=null,parent=null,key=null,name='y.jpg'
			where owner_id=$1::uuid and id=$2::uuid`, owner, file)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("relink through a NULL cache was not refused: %v", err)
		}
	})

	t.Run("broken ancestry falls back to the stale cache", func(t *testing.T) {
		owner := identityAuditOwner(t, pool, "")
		photos := identityAuditInsert(t, pool, owner, "Photos", "", "Photos", true)
		_ = identityAuditInsert(t, pool, owner, "Photos/2024", "Photos", "2024", true)
		photo := identityAuditInsert(t, pool, owner, "Photos/2024/x.jpg", "Photos/2024", "x.jpg", false)
		_ = identityAuditInsert(t, pool, owner, "Media", "", "Media", true)
		if _, err := pool.Exec(ctx, `update nodes set key=$3,path='Media/Photos',parent='Media',name='Photos'
			where owner_id=$1::uuid and id=$2::uuid`, owner, photos, "drive/"+owner+"/Media/Photos/"); err != nil {
			t.Fatalf("tree move: %v", err)
		}

		// No application path deletes a parent row and leaves its children behind, but the
		// view's contract is what a corrupt or partially-repaired index would ask. Delete the
		// moved ancestor only, as an external repair or a lost row would.
		if _, err := pool.Exec(ctx, `delete from nodes where owner_id=$1::uuid and id=$2::uuid`, owner, photos); err != nil {
			t.Fatalf("break ancestry: %v", err)
		}
		var derived *string
		if err := pool.QueryRow(ctx, `select file_tree_path($1::uuid,$2::uuid)`, owner, photo).Scan(&derived); err != nil {
			t.Fatal(err)
		}
		if derived != nil {
			t.Fatalf("ancestry still resolves (%q); the fallback probe no longer describes a broken link", *derived)
		}
		row := identityAuditRead(t, pool, owner, photo)
		if got := identityAuditString(t, row.ViewPath); got != "Photos/2024/x.jpg" {
			t.Fatalf("fallback path %q, want the stale cache", got)
		}
		// This string is exactly what the file API feeds fc.WithinScope for an ID selector,
		// so a scope that excludes Media would still admit this ID.
		scopePhotos := fc.Path("/Photos")
		current := fc.Path("/Media/Photos/2024/x.jpg")
		key := namecompare.Key(true)
		if fc.WithinScope(scopePhotos, current, key) {
			t.Fatal("the pre-move path is already outside /Photos; the authorization difference is not being probed")
		}
		if !fc.WithinScope(scopePhotos, fc.Path("/"+*row.ViewPath), key) {
			t.Fatal("the fallback path is not inside /Photos; the authorization difference is not being probed")
		}
		// If the cache is also gone, the view returns NULL rather than a path. FileResolveID
		// scans that into a string and fails, so there is no "root" fallback.
		if _, err := pool.Exec(ctx, `update nodes set path=null,parent=null,key=null
			where owner_id=$1::uuid and id=$2::uuid`, owner, photo); err != nil {
			t.Fatalf("null the orphan cache: %v", err)
		}
		var nullPath *string
		if err := pool.QueryRow(ctx, `select path from file_entries where owner_id=$1::uuid and id=$2::uuid`, owner, photo).Scan(&nullPath); err != nil {
			t.Fatal(err)
		}
		if nullPath != nil {
			t.Fatalf("expected NULL path with broken ancestry and NULL cache, got %q", *nullPath)
		}
	})

	t.Run("without the trigger the canonical columns and upserts stop working", func(t *testing.T) {
		// One transaction owns both the disabled trigger and the rows, so a failure cannot
		// leave the shared scratch schema in the disabled state.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `alter table nodes disable trigger file_canonical_node_write`); err != nil {
			t.Fatalf("disable trigger: %v", err)
		}
		var owner string
		if err := tx.QueryRow(ctx, `insert into users(email) values(gen_random_uuid()::text||'@identity-audit.test') returning id::text`).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size)
			values($1::uuid,$2,'Notes/a.txt','Notes','a.txt',false,3)`, owner, "drive/"+owner+"/Notes/a.txt"); err != nil {
			t.Fatalf("insert without trigger: %v", err)
		}
		var parentID, comparison *string
		if err := tx.QueryRow(ctx, `select parent_id::text,comparison_name from nodes where owner_id=$1::uuid`, owner).Scan(&parentID, &comparison); err != nil {
			t.Fatal(err)
		}
		if parentID != nil || comparison != nil {
			t.Fatalf("without the trigger identity is still written: parent_id=%v comparison_name=%v", parentID, comparison)
		}
		var resolved *string
		if err := tx.QueryRow(ctx, `select file_tree_resolve($1::uuid,'Notes/a.txt')::text`, owner).Scan(&resolved); err != nil {
			t.Fatal(err)
		}
		if resolved != nil {
			t.Fatalf("file_tree_resolve returned %s without canonical columns", *resolved)
		}
		// Every production upsert names (owner_id,parent_id,comparison_name). Without the
		// trigger those columns are NULL, so the conflict target cannot match and the write
		// fails on the still-unique key instead of updating.
		savepoint, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = savepoint.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size)
			values($1::uuid,$2,'Notes/a.txt','Notes','a.txt',false,4)
			on conflict(owner_id,parent_id,comparison_name) do update set size=excluded.size`,
			owner, "drive/"+owner+"/Notes/a.txt")
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("upsert without canonical columns did not fail on the unique key: %v", err)
		}
		_ = savepoint.Rollback(ctx)
	})

	t.Run("explicit comparison_name is overwritten by the trigger", func(t *testing.T) {
		// The future plan has Go pass the columns. Today the trigger recomputes rather than
		// validates them, so a divergent Go spelling would be silently corrected — and a
		// validation-only trigger would need to reject it instead.
		owner := identityAuditOwner(t, pool, "")
		root := identityAuditRoot(t, pool, owner)
		var id string
		err := pool.QueryRow(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size,parent_id,comparison_name)
			values($1::uuid,$2,'a.txt','','a.txt',false,3,$3::uuid,'not-the-key') returning id::text`,
			owner, "drive/"+owner+"/a.txt", root).Scan(&id)
		if err != nil {
			t.Fatalf("insert with explicit columns: %v", err)
		}
		row := identityAuditRead(t, pool, owner, id)
		if row.ComparisonName == nil || *row.ComparisonName != "a.txt" {
			t.Fatalf("comparison_name %v, want the trigger's value a.txt", row.ComparisonName)
		}
		if row.ParentID == nil || *row.ParentID != root {
			t.Fatalf("parent_id %v, want the explicit root %s", row.ParentID, root)
		}
	})
}
