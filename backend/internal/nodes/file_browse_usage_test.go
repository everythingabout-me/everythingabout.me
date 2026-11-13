package nodes

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"

	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
)

// TestFileBrowseUsageScopeIsBoundedByTheSubtree guards the subfolder usage read against
// regressing to a whole-owner scan.
//
// The scope predicate used to compare the derived file_entries.path per row
// (file_identity_comparison(path, version) = scope or like scope||'/%'), which cannot use
// an index because file_tree_path() walks the ancestry for every scanned row. Measured on
// a seeded 50k-entry owner with 25 files under f0000, GET /api/files/me?path=/f0000
// scanned 50,000 rows in 1,620ms and 1,181,208 shared buffer hits. The fix resolves the
// anchor with file_tree_resolve and carries size/is_dir down the canonical parent link, so
// the read visits exactly the 26 rows of that subtree (0.8ms, 115 buffers) and the cost no
// longer grows with the drive.
//
// The plan assertion is on rows visited, not on node type: every scan of the nodes table
// may visit only a small constant of rows, so any plan that walks the owner to answer a
// subfolder scope fails here regardless of the scan operator it picks. The fixture inserts
// canonical rows directly with the write triggers disabled, because 50k rows through the
// plpgsql identity trigger costs minutes and nothing under test is the writer; the anchor
// and comparison rules are the ones file_tree_resolve applies.
func TestFileBrowseUsageScopeIsBoundedByTheSubtree(t *testing.T) {
	pool := actionTestDatabase(t)
	ctx := context.Background()
	owner := actionOwner(t, pool)
	testURL, err := url.Parse(testpg.Require(t))
	if err != nil {
		t.Fatal(err)
	}
	testURL.Path = "/" + pool.Config().ConnConfig.Database
	db, err := store.Open(ctx, testURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service := New(db, nil)
	ownerID := owner.OwnerID

	// 2,000 folders of 25 files each: a 50,000-file owner where the scoped read must
	// return 25 of them. The size is the same for every file so a wrong subtree is visible
	// in the bytes as well as the count.
	const folders, perFolder, size = 2000, 25, 10
	rootID := ""
	if err = pool.QueryRow(ctx, `select file_identity_root($1::uuid)::text`, ownerID).Scan(&rootID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `alter table nodes disable trigger user`); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,parent_id,comparison_name)
	 select $1::uuid,'fixture/'||$1::text||'/f'||lpad(f::text,4,'0')||'/','f'||lpad(f::text,4,'0'),'','f'||lpad(f::text,4,'0'),true,
	   $2::uuid,file_identity_comparison('f'||lpad(f::text,4,'0'),(select name_comparison_version from file_identity_roots where owner_id=$1::uuid))
	 from generate_series(0,$3) f`, ownerID, rootID, folders-1); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size,mime,parent_id,comparison_name)
	 select $1::uuid,'fixture/'||$1::text||'/f'||lpad(f::text,4,'0')||'/file'||j||'.txt',
	   'f'||lpad(f::text,4,'0')||'/file'||j||'.txt','f'||lpad(f::text,4,'0'),'file'||j||'.txt',false,$3,'text/plain',
	   folder.id,file_identity_comparison('file'||j||'.txt',(select name_comparison_version from file_identity_roots where owner_id=$1::uuid))
	 from generate_series(0,$2) f, generate_series(0,$4) j
	 cross join lateral (select id from nodes where owner_id=$1::uuid and path='f'||lpad(f::text,4,'0') and is_dir) folder`,
		ownerID, folders-1, size, perFolder-1); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `alter table nodes enable trigger user`); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `analyze nodes`); err != nil {
		t.Fatal(err)
	}

	usage := func(path string, hidden ...string) (int64, int64) {
		t.Helper()
		bytes, files, err := service.FileBrowseUsage(ctx, ownerID, path, hidden...)
		if err != nil {
			t.Fatalf("usage %q hidden %v: %v", path, hidden, err)
		}
		return bytes, files
	}
	expect := func(label string, wantBytes, wantFiles int64, path string, hidden ...string) {
		t.Helper()
		gotBytes, gotFiles := usage(path, hidden...)
		if gotBytes != wantBytes || gotFiles != wantFiles {
			t.Fatalf("%s: bytes=%d files=%d, want bytes=%d files=%d", label, gotBytes, gotFiles, wantBytes, wantFiles)
		}
	}

	// "/" is the whole owner; a subfolder is exactly its subtree; concealment still
	// removes the captured pending-delete rows from either.
	expect("whole owner", folders*perFolder*size, folders*perFolder, "")
	expect("whole owner concealing one folder", (folders*perFolder-perFolder)*size, folders*perFolder-perFolder, "", "f0000")
	expect("subfolder", perFolder*size, perFolder, "f0000")
	expect("subfolder with the same path concealed", 0, 0, "f0000", "f0000")
	expect("subfolder with one concealed file", (perFolder-1)*size, perFolder-1, "f0000", "f0000/file7.txt")
	expect("missing scope", 0, 0, "no-such-folder")
	// A file scope keeps counting that file: the anchor is included and `not is_dir`
	// decides, which is what the old path equality also did.
	expect("file scope", size, 1, "f0001/file0.txt")

	// The query must not visit the owner to answer one subfolder. A per-row path
	// comparison reports the whole scan here even though the aggregate returns 25 rows,
	// because the scan node either outputs or removes every owner entry.
	const maxVisited = 1000
	for _, hidden := range [][]string{nil, {"f0001"}} {
		for _, node := range usageScopePlan(t, ctx, db, ownerID, hidden, "f0000") {
			if node.RelationName != "nodes" {
				continue
			}
			visited := (node.ActualRows + node.RemovedByFilter) * node.ActualLoops
			if visited > maxVisited {
				t.Fatalf("scope usage explained with hidden %v: %s on nodes visited %.0f rows (limit %d); "+
					"the subfolder predicate is scanning the owner again instead of walking the resolved subtree",
					hidden, node.NodeType, visited, maxVisited)
			}
		}
	}

	// Identity, not spelling: the anchor resolves through the owner's name comparison, so
	// an NFD spelling reaches the NFC-indexed subtree. The trigger builds the canonical
	// link here, as it does for real writes. The nested folder also proves the walk reaches
	// more than one level.
	indexActionEntry(t, pool, owner, "f0000/nested", true)
	if _, err = pool.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size,mime)
	 values($1::uuid,'fixture/'||$1::text||'/f0000/nested/deep.txt','f0000/nested/deep.txt','f0000/nested','deep.txt',false,4,'text/plain')`, ownerID); err != nil {
		t.Fatal(err)
	}
	expect("subfolder includes a nested folder", perFolder*size+4, perFolder+1, "f0000")
	indexActionEntry(t, pool, owner, "café", true)
	if _, err = pool.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size,mime)
	 values($1::uuid,'fixture/'||$1::text||'/café/x.txt','café/x.txt','café','x.txt',false,4,'text/plain')`, ownerID); err != nil {
		t.Fatal(err)
	}
	expect("NFD scope resolves the NFC subtree", 4, 1, "cafe\u0301")
}

type usagePlanNode struct {
	NodeType        string            `json:"Node Type"`
	RelationName    string            `json:"Relation Name"`
	ActualRows      float64           `json:"Actual Rows"`
	ActualLoops     float64           `json:"Actual Loops"`
	RemovedByFilter float64           `json:"Rows Removed by Filter"`
	Plans           []json.RawMessage `json:"Plans"`
}

// usageScopePlan explains exactly the statement FileBrowseUsage runs for a subfolder and
// returns every plan node, so a test can bound what the read visits.
func usageScopePlan(t *testing.T, ctx context.Context, db *store.DB, owner string, hidden []string, path string) []usagePlanNode {
	t.Helper()
	var raw []byte
	sql := `explain (analyze, buffers, format json) ` + browseUsageHidden + browseUsageScopeQuery
	if err := db.Pool().QueryRow(ctx, sql, owner, hidden, path).Scan(&raw); err != nil {
		t.Fatalf("explain scoped usage: %v", err)
	}
	var explained []struct {
		Plan json.RawMessage `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &explained); err != nil || len(explained) != 1 {
		t.Fatalf("explain output %s: %v", raw, err)
	}
	var nodes []usagePlanNode
	var walk func(json.RawMessage)
	walk = func(raw json.RawMessage) {
		var node usagePlanNode
		if err := json.Unmarshal(raw, &node); err != nil {
			t.Fatalf("plan node: %v", err)
		}
		nodes = append(nodes, node)
		for _, child := range node.Plans {
			walk(child)
		}
	}
	walk(explained[0].Plan)
	return nodes
}
