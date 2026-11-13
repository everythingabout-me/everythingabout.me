package nodes

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"

	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
)

// TestPendingDeleteConcealmentIsBoundedByTheDeletedSubtree guards the read-side
// pending-delete filter against regressing to a per-row path comparison.
//
// The filter used to ask "is this candidate's current path equal to a root or under
// it?" with file_identity_comparison(path, version), which cannot use an index because
// file_entries.path is derived per row by file_tree_path() -- a walk up the ancestry.
// Measured on a 50k-entry owner with one 99-entry pending delete, GET /api/files/me
// scanned 49,500 rows in 1560ms and 1,487,982 shared buffer hits, against 9-11ms and
// ~3,200 buffers for the identity-set predicate (browseHidden). The difference is that
// the delete-specific work walks the rows under the captured root and nothing else, so
// a delete's cost no longer grows with the drive.
//
// The cost assertion is self-calibrating: the same read is explained once with no
// pending delete and once with one, and the buffers it touches may not grow by more than
// a constant plus a small factor. A per-row ancestry walk multiplies the drive scan by
// roughly 30 buffer hits per row (two walks), which this bound catches at any fixture
// size; the identity walk only adds the pages of the subtree.
func TestPendingDeleteConcealmentIsBoundedByTheDeletedSubtree(t *testing.T) {
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

	// 200 folders of 25 files each: 5,000 files is already ~30 buffer hits of ancestry
	// walking per row for the old predicate, so the regression signal is unambiguous
	// while the fixture stays cheap enough to insert inline.
	const folders, perFolder, size = 200, 25, 10
	if _, err = pool.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir)
	 select $1::uuid,'fixture/'||$1::text||'/f'||lpad(f::text,3,'0'),'f'||lpad(f::text,3,'0'),'','f'||lpad(f::text,3,'0'),true
	 from generate_series(0,$2) f`, owner.OwnerID, folders-1); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size,mime)
	 select $1::uuid,'fixture/'||$1::text||'/f'||lpad(f::text,3,'0')||'/file'||j||'.txt',
	   'f'||lpad(f::text,3,'0')||'/file'||j||'.txt','f'||lpad(f::text,3,'0'),'file'||j||'.txt',false,$3,'text/plain'
	 from generate_series(0,$2) f, generate_series(0,$4) j`, owner.OwnerID, folders-1, size, perFolder-1); err != nil {
		t.Fatal(err)
	}

	// The operation journal captured "f000" as the root of a delete that is still being
	// admitted, so its 25 files are concealed until the acceptance commits.
	hidden := []string{"f000"}
	bytes, files, err := service.FileBrowseUsage(ctx, owner.OwnerID, "")
	if err != nil || files != folders*perFolder || bytes != folders*perFolder*size {
		t.Fatalf("unfiltered usage bytes=%d files=%d err=%v", bytes, files, err)
	}
	bytes, files, err = service.FileBrowseUsage(ctx, owner.OwnerID, "", hidden...)
	if err != nil || files != folders*perFolder-perFolder || bytes != (folders*perFolder-perFolder)*size {
		t.Fatalf("concealed usage bytes=%d files=%d err=%v", bytes, files, err)
	}

	baseBuffers := browseBuffers(t, ctx, db, owner.OwnerID, "")
	pendingBuffers := browseBuffers(t, ctx, db, owner.OwnerID, "f000")
	if pendingBuffers > 3*baseBuffers+500 {
		t.Fatalf("one pending delete multiplied the read's buffer traffic: %d without, %d with %d concealed files; "+
			"the concealment predicate is scanning per entry again instead of walking the captured subtree",
			baseBuffers, pendingBuffers, perFolder)
	}

	// The settled decision (docs/design/deletes.md): acceptance removes the rows and
	// releases the guard in the same transaction, so the name is free again and a path
	// re-created afterwards is an ordinary new identity. The read filter sees no root
	// for it and must return it, not the old subtree.
	if _, err = pool.Exec(ctx, `delete from nodes where owner_id=$1::uuid and (path='f000' or path like 'f000/%')`, owner.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size,mime)
	 values($1::uuid,'fixture/'||$1::text||'/f000-recreated','f000/recreated.txt','f000','recreated.txt',false,7,'text/plain')`, owner.OwnerID); err != nil {
		t.Fatal(err)
	}
	if bytes, files, err = service.FileBrowseUsage(ctx, owner.OwnerID, "f000"); err != nil || files != 1 || bytes != 7 {
		t.Fatalf("re-created path stayed concealed: bytes=%d files=%d err=%v", bytes, files, err)
	}

	// Concealment is by identity, not spelling: the captured root is resolved through
	// the owner's comparison rules, so an NFD spelling hides the NFC-indexed subtree.
	if _, err = pool.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir)
	 values($1::uuid,'fixture/'||$1::text||'/café','café','','café',true)`, owner.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size,mime)
	 values($1::uuid,'fixture/'||$1::text||'/café/x.txt','café/x.txt','café','x.txt',false,4,'text/plain')`, owner.OwnerID); err != nil {
		t.Fatal(err)
	}
	if bytes, files, err = service.FileBrowseUsage(ctx, owner.OwnerID, "café", "cafe\u0301"); err != nil || files != 0 || bytes != 0 {
		t.Fatalf("NFD root did not conceal the NFC subtree: bytes=%d files=%d err=%v", bytes, files, err)
	}
}

// browseBuffers runs the production usage query under EXPLAIN (ANALYZE, BUFFERS) and
// sums every shared block it touched, so the cost of one pending delete can be compared
// against the same read without one.
func browseBuffers(t *testing.T, ctx context.Context, db *store.DB, owner, hidden string) int64 {
	t.Helper()
	var roots []string
	if hidden != "" {
		roots = []string{hidden}
	}
	var raw []byte
	sql := `explain (analyze, buffers, format json) ` + browseUsageHidden + browseUsageQuery
	if err := db.Pool().QueryRow(ctx, sql, owner, roots).Scan(&raw); err != nil {
		t.Fatalf("explain usage: %v", err)
	}
	var explained []struct {
		Plan json.RawMessage `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &explained); err != nil || len(explained) != 1 {
		t.Fatalf("explain output %s: %v", raw, err)
	}
	var total int64
	var walk func(raw json.RawMessage)
	walk = func(raw json.RawMessage) {
		// The JSON format puts the counters at each node's top level (the text format
		// groups them under "Buffers"), and they are per node, not cumulative.
		var node struct {
			Hit   float64           `json:"Shared Hit Blocks"`
			Read  float64           `json:"Shared Read Blocks"`
			Plans []json.RawMessage `json:"Plans"`
		}
		if err := json.Unmarshal(raw, &node); err != nil {
			t.Fatalf("plan node: %v", err)
		}
		total += int64(node.Hit + node.Read)
		for _, child := range node.Plans {
			walk(child)
		}
	}
	walk(explained[0].Plan)
	return total
}
