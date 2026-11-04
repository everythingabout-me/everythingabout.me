package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"unicode/utf8"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func TestIdentityMetadataUsesStoredUUIDs(t *testing.T) {
	id, parent := "4ec39462-503b-4f40-9e42-aa77807621e2", "08e5b8f3-88af-47c7-882b-f457882f7573"
	record := FileBrowseRecord{Row: Row{Node: Node{ID: id, Path: "native/cafe\u0301.txt", Parent: "native", Name: "cafe\u0301.txt"}}, ParentID: &parent}
	m, err := IndexedMetadata("root", record)
	if err != nil || m.ID != id || m.ParentID == nil || *m.ParentID != parent || m.Name != record.Row.Name {
		t.Fatalf("metadata %+v error %v", m, err)
	}
	root, err := IndexedMetadata("root", FileBrowseRecord{Row: Row{Node: Node{ID: parent, IsDir: true}}})
	if err != nil || root.ID != parent || root.ParentID != nil {
		t.Fatalf("root %+v error %v", root, err)
	}
}

func TestFileIdentityResolutionParentsAndIsolation(t *testing.T) {
	db, ctx := observationTestDB(t)
	pool := db.Pool()
	owner, other := actionOwner(t, pool), actionOwner(t, pool)
	service := New(db, nil)
	root, parent, err := service.FileIdentity(ctx, owner.OwnerID, "")
	if err != nil || root == "" || parent != "" {
		t.Fatalf("root %q parent %q error %v", root, parent, err)
	}
	again, _, err := service.FileIdentity(ctx, owner.OwnerID, "")
	if err != nil || again != root {
		t.Fatalf("unstable root %q %q %v", root, again, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `select count(*) from nodes where owner_id=$1::uuid`, owner.OwnerID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("synthetic root node count=%d error=%v", count, err)
	}
	missingID, missingParent, err := service.FileIdentity(ctx, owner.OwnerID, "new/file.txt")
	if err != nil || missingID != "" || missingParent != "" {
		t.Fatalf("new destination identity %q/%q error=%v", missingID, missingParent, err)
	}
	id := indexActionEntry(t, pool, owner, "Native/Sub/cafe\u0301.txt", false)
	folderID, _, err := service.FileIdentity(ctx, owner.OwnerID, "Native/Sub")
	if err != nil {
		t.Fatal(err)
	}
	gotID, gotParent, err := service.FileIdentity(ctx, owner.OwnerID, "Native/Sub/café.txt")
	if err != nil || gotID != id || gotParent != folderID {
		t.Fatalf("identity %q/%q want %q/%q error=%v", gotID, gotParent, id, folderID, err)
	}
	for _, tc := range []struct {
		owner, id, path string
		found           bool
	}{
		{owner.OwnerID, root, "", true}, {owner.OwnerID, id, "Native/Sub/cafe\u0301.txt", true},
		{other.OwnerID, root, "", false}, {other.OwnerID, id, "", false},
		{owner.OwnerID, "n_" + id, "", false}, {owner.OwnerID, "invalid", "", false},
	} {
		got, found, err := service.FileResolveID(ctx, tc.owner, tc.id)
		if err != nil || found != tc.found || got != tc.path {
			t.Fatalf("resolve %q => %q %v %v", tc.id, got, found, err)
		}
	}
	browse, err := service.FileBrowseMetadata(ctx, owner.OwnerID, "root", "Native/Sub/café.txt")
	if err != nil || browse == nil || browse.Row.ID != id || browse.ParentID == nil || *browse.ParentID != folderID || browse.Row.Name != "cafe\u0301.txt" {
		t.Fatalf("browse %+v error=%v", browse, err)
	}
	page, err := service.FileBrowsePage(ctx, owner.OwnerID, "root", FileBrowseQuery{List: true, Path: "Native/Sub", Limit: 10})
	if err != nil || len(page) != 1 || page[0].ParentID == nil || *page[0].ParentID != folderID {
		t.Fatalf("browse page %+v error=%v", page, err)
	}
	otherRoot, _, err := service.FileIdentity(ctx, other.OwnerID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`update nodes set parent_id=$2::uuid where id=$1::uuid`,
		`update nodes set id=$2::uuid where id=$1::uuid`,
		`update nodes set owner_id=$2::uuid where id=$1::uuid`,
	} {
		arg := otherRoot
		if query == `update nodes set owner_id=$2::uuid where id=$1::uuid` {
			arg = other.OwnerID
		}
		if _, err := pool.Exec(ctx, query, id, arg); err == nil {
			t.Fatalf("unsafe identity update accepted: %s", query)
		}
	}
	if _, err := pool.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,parent_id) values($1::uuid,$2,'foreign.txt','','foreign.txt',$3::uuid)`, owner.OwnerID, paths.KeyFor("foreign.txt", owner.OwnerID), otherRoot); err == nil {
		t.Fatal("foreign parent accepted")
	}
	if _, err := pool.Exec(ctx, `delete from nodes where id=$1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	if _, found, err := service.FileResolveID(ctx, owner.OwnerID, id); err != nil || found {
		t.Fatalf("deleted identity found=%v error=%v", found, err)
	}
}

func TestFileIdentityNameComparisonAndFullFold(t *testing.T) {
	db, ctx := observationTestDB(t)
	pool := db.Pool()
	service := New(db, nil)
	owner := actionOwner(t, pool)
	indexActionEntry(t, pool, owner, "A.txt", false)
	indexActionEntry(t, pool, owner, "a.txt", false)
	if err := service.FileConfigureNameComparison(ctx, owner.OwnerID, false); err == nil {
		t.Fatal("existing case collision accepted")
	}
	var version string
	if err := pool.QueryRow(ctx, `select name_comparison_version from file_identity_roots where owner_id=$1::uuid`, owner.OwnerID).Scan(&version); err != nil || version != "nfc-sensitive-v1" {
		t.Fatalf("comparison rollback %q %v", version, err)
	}
	indexActionEntry(t, pool, owner, "cafe\u0301.txt", false)
	_, err := pool.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name) values($1::uuid,$2,'café.txt','','café.txt')`, owner.OwnerID, paths.KeyFor("café.txt", owner.OwnerID))
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "23505" {
		t.Fatalf("NFC/NFD duplicate error=%v", err)
	}
	foldOwner := actionOwner(t, pool)
	if err := service.FileConfigureNameComparison(ctx, foldOwner.OwnerID, false); err != nil {
		t.Fatal(err)
	}
	foldID := indexActionEntry(t, pool, foldOwner, "Straße", false)
	folded, err := service.FileBrowseMetadata(ctx, foldOwner.OwnerID, "root", "STRASSE")
	if err != nil || folded == nil || folded.Row.ID != foldID || folded.Row.Path != "Straße" {
		t.Fatalf("folded metadata %+v error=%v", folded, err)
	}
	indexActionEntry(t, pool, foldOwner, "Folder/Leaf.txt", false)
	foldedPage, err := service.FileBrowsePage(ctx, foldOwner.OwnerID, "root", FileBrowseQuery{Path: "FOLDER", List: true, Limit: 10})
	if err != nil || len(foldedPage) != 1 || foldedPage[0].Row.Path != "Folder/Leaf.txt" {
		t.Fatalf("folded page %+v error=%v", foldedPage, err)
	}
	hidden, err := service.FileBrowseMetadata(ctx, foldOwner.OwnerID, "root", "Folder/Leaf.txt", "FOLDER")
	if err != nil || hidden != nil {
		t.Fatalf("folded hidden metadata %+v error=%v", hidden, err)
	}
	bytes, files, err := service.FileBrowseUsage(ctx, foldOwner.OwnerID, "FOLDER")
	if err != nil || bytes != 3 || files != 1 {
		t.Fatalf("folded usage %d/%d error=%v", bytes, files, err)
	}
	if _, err := pool.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name) values($1::uuid,$2,'STRASSE','','STRASSE')`, foldOwner.OwnerID, paths.KeyFor("STRASSE", foldOwner.OwnerID)); err == nil {
		t.Fatal("Unicode full fold duplicate accepted")
	}
	for _, value := range []string{"", "Straße/STRASSE", "Σίσυφος/ΣΊΣΥΦΟΣ", "İ/i\u0307", "cafe\u0301/CAFÉ", "ﬀ/ff", "Ꭰ/ꭰ"} {
		var got string
		if err := pool.QueryRow(ctx, `select file_identity_comparison($1,'nfc-fold-v1')`, value).Scan(&got); err != nil {
			t.Fatal(err)
		}
		want := norm.NFC.String(cases.Fold().String(norm.NFC.String(value)))
		if got != want {
			t.Fatalf("SQL fold %q => %q want %q", value, got, want)
		}
	}
	rows, err := pool.Query(ctx, `select source,folded from file_identity_casefold_v1`)
	if err != nil {
		t.Fatal(err)
	}
	mapping := map[string]string{}
	for rows.Next() {
		var source, folded string
		if err := rows.Scan(&source, &folded); err != nil {
			t.Fatal(err)
		}
		mapping[source] = folded
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	fold := cases.Fold()
	for r := rune(1); r <= utf8.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		s := string(r)
		want := fold.String(s)
		got := s
		if mapped, ok := mapping[s]; ok {
			got = mapped
		}
		if got != want {
			t.Fatalf("SQL mapping differs from native full folding at U+%04X", r)
		}
	}
}

func TestFileIdentityMigrationBackfillsTwiceWithoutChangingNativeSpelling(t *testing.T) {
	db, ctx := observationTestDB(t)
	pool := db.Pool()
	owner := actionOwner(t, pool)
	if _, err := pool.Exec(ctx, `alter table nodes disable trigger user`); err != nil {
		t.Fatal(err)
	}
	id := indexActionEntry(t, pool, owner, "Archive/Deep/cafe\u0301.txt", false)
	if _, err := pool.Exec(ctx, `alter table nodes enable trigger user`); err != nil {
		t.Fatal(err)
	}
	event := attachActionEvent(t, pool, owner, id)
	var before string
	if err := pool.QueryRow(ctx, `select jsonb_build_array(id,key,path,parent,name,size,meta,created_at,updated_at)::text from nodes where id=$1::uuid`, id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../../sql/026_file_identity.sql")
	if err != nil {
		t.Fatal(err)
	}
	canonicalMigration, err := os.ReadFile("../../../sql/027_canonical_file_tree.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration = append(migration, canonicalMigration...)
	for pass := 0; pass < 2; pass++ {
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("migration pass %d: %v", pass, err)
		}
		var after string
		if err := pool.QueryRow(ctx, `select jsonb_build_array(id,key,path,parent,name,size,meta,created_at,updated_at)::text from nodes where id=$1::uuid`, id).Scan(&after); err != nil || after != before {
			t.Fatalf("native index representation changed: %s => %s error=%v", before, after, err)
		}
		var linked string
		if err := pool.QueryRow(ctx, `select node_id::text from event_media where event_id=$1::uuid`, event).Scan(&linked); err != nil || linked != id {
			t.Fatalf("event changed %q %v", linked, err)
		}
		var count int
		if err := pool.QueryRow(ctx, `select count(*) from nodes where owner_id=$1::uuid`, owner.OwnerID).Scan(&count); err != nil || count != 3 {
			t.Fatalf("ancestor count %d %v", count, err)
		}
		service := New(db, nil)
		parent, _, err := service.FileIdentity(ctx, owner.OwnerID, "Archive/Deep")
		if err != nil {
			t.Fatal(err)
		}
		gotID, gotParent, err := service.FileIdentity(ctx, owner.OwnerID, "Archive/Deep/café.txt")
		if err != nil || gotID != id || gotParent != parent {
			t.Fatalf("backfill identity %q/%q %v", gotID, gotParent, err)
		}
	}
}

func TestFileIdentityRecreationAndReplacement(t *testing.T) {
	pool := actionTestDatabase(t)
	ctx := context.Background()
	for _, kind := range []string{"mkdir", "copy", "create", "replace", "move"} {
		t.Run(kind, func(t *testing.T) {
			root := actionOwner(t, pool)
			folder := kind == "mkdir"
			stale := indexActionEntry(t, pool, root, "target", folder)
			source := indexActionEntry(t, pool, root, "source", folder)
			event := attachActionEvent(t, pool, root, source)
			var raw json.RawMessage
			if kind == "create" || kind == "replace" {
				intent := storage.WriteIntent{Target: "target", Size: 3, Conditions: storage.Conditions{Absent: kind == "create"}}
				encoded, _ := json.Marshal(intent)
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				raw, err = CommitStagedFile(ctx, tx, fileops.Operation{Root: root, Intent: encoded}, &storage.MutationResult{Entry: &storage.Entry{Path: "target", Kind: storage.File, Size: 3}})
				if err != nil {
					t.Fatal(err)
				}
				if err = tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				from := "source"
				if kind == "mkdir" {
					from = ""
				}
				nativeKind := storage.File
				if folder {
					nativeKind = storage.Folder
				}
				raw = runActionCommit(t, pool, actionOperation(root, kind, from, "target", actionEffect(0, kind, from, "target", nativeKind)))
			}
			var metadata fc.Metadata
			if err := json.Unmarshal(raw, &metadata); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "replace":
				if metadata.ID != stale {
					t.Fatalf("replacement changed ID: %s", raw)
				}
			case "move":
				if metadata.ID != source {
					t.Fatalf("move lost source ID: %s", raw)
				}
			default:
				if metadata.ID == stale || metadata.ID == source || metadata.ID == "" {
					t.Fatalf("recreation inherited old ID: %s", raw)
				}
			}
			var linked string
			if err := pool.QueryRow(ctx, `select node_id::text from event_media where event_id=$1::uuid`, event).Scan(&linked); err != nil || linked != source {
				t.Fatalf("source event damaged %q %v", linked, err)
			}
		})
	}
}
