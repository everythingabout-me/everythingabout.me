package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/filerevision"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests only use an explicitly supplied disposable database service. No
// application config or environment file is loaded.
func actionTestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	scratch := testpg.CreateDatabase(t, "eam_nodes_actions_")
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		for _, migration := range migrations {
			data, err := os.ReadFile(migration)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, string(data)); err != nil {
				t.Fatalf("migration %s: %v", migration, err)
			}
		}
	}
	return pool
}

func actionOwner(t *testing.T, pool *pgxpool.Pool) fileops.Root {
	t.Helper()
	var owner string
	if err := pool.QueryRow(context.Background(), `insert into users(email,password_hash) values(gen_random_uuid()::text||'@example.test','test') returning id::text`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	return fileops.Root{OwnerID: owner, ConnectionID: "fixture-local", Generation: 1, ContentRoot: "/isolated/content", SystemRoot: "/isolated/system", CaseSensitive: true}
}
func indexActionEntry(t *testing.T, pool *pgxpool.Pool, root fileops.Root, p string, folder bool) string {
	t.Helper()
	key := paths.KeyFor(p, root.OwnerID)
	if folder {
		key = paths.DirKeyFor(p, root.OwnerID)
	}
	var id string
	if err := pool.QueryRow(context.Background(), `insert into nodes(owner_id,key,path,parent,name,is_dir,size,meta) values($1::uuid,$2,$3,$4,$5,$6,$7,'{"annotation":"retained"}') returning id::text`, root.OwnerID, key, p, paths.ParentOf(p), paths.NameOf(p), folder, 3).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
func actionEffect(number int, action, source, target string, kind storage.Kind) fileops.EntryEffect {
	effect := fileops.EntryEffect{ManifestEntry: fileops.ManifestEntry{Number: number, Action: action, Source: storage.Path(source), Target: storage.Path(target), SourceRevision: []byte("captured-version"), Size: 3}, State: "applied", Evidence: []byte("proven-native-effect")}
	result := storage.MutationResult{Evidence: effect.Evidence}
	if action != "delete" {
		entry := storage.Entry{Path: storage.Path(target), Kind: kind, Size: 3, MediaType: "text/plain", Revision: []byte("target-version"), ModifiedAt: time.Date(2026, 10, 31, 12, 0, 0, 123456000, time.UTC)}
		if kind == storage.Folder {
			entry.Size = 0
			entry.MediaType = ""
			effect.Size = 0
		}
		if action == "mkdir" {
			effect.SourceRevision = nil
		}
		result.Entry = &entry
	}
	effect.Result, _ = json.Marshal(result)
	return effect
}
func actionOperation(root fileops.Root, kind, from, to string, effects ...fileops.EntryEffect) fileops.Operation {
	request := fileops.ActionRequest{Root: root, Kind: kind, From: storage.Path(from), To: storage.Path(to)}
	intent, _ := json.Marshal(request)
	return fileops.Operation{Root: root, Kind: kind, Intent: intent, Entries: effects}
}
func runActionCommit(t *testing.T, pool *pgxpool.Pool, op fileops.Operation) json.RawMessage {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	result, err := CommitFileAction(ctx, tx, op, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return result
}
func assertActionID(t *testing.T, pool *pgxpool.Pool, root fileops.Root, p, want string) {
	t.Helper()
	var id, key, parent, name string
	err := pool.QueryRow(context.Background(), `select id::text,key,parent,name from nodes where owner_id=$1::uuid and path=$2`, root.OwnerID, p).Scan(&id, &key, &parent, &name)
	if want == "" {
		if err != pgx.ErrNoRows {
			t.Fatalf("path %q unexpectedly indexed: id=%q err=%v", p, id, err)
		}
		return
	}
	if err != nil || id != want || parent != paths.ParentOf(p) || name != paths.NameOf(p) {
		t.Fatalf("path %q: id=%q want=%q parent=%q name=%q err=%v", p, id, want, parent, name, err)
	}
	if key != paths.KeyFor(p, root.OwnerID) && key != paths.DirKeyFor(p, root.OwnerID) {
		t.Fatalf("stale storage key %q for %q", key, p)
	}
}
func attachActionEvent(t *testing.T, pool *pgxpool.Pool, root fileops.Root, id string) string {
	t.Helper()
	ctx := context.Background()
	var event string
	if err := pool.QueryRow(ctx, `insert into events(user_id,starts_at,tz,local_date,type,title) values($1::uuid,now(),'UTC',current_date,'photo','Fixture') returning id::text`, root.OwnerID).Scan(&event); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into event_media(event_id,idx,node_id) values($1::uuid,0,$2::uuid)`, event, id); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestCommitFileActionProvenEffects(t *testing.T) {
	pool := actionTestDatabase(t)
	t.Run("native move retains private identity and event", func(t *testing.T) {
		root := actionOwner(t, pool)
		id := indexActionEntry(t, pool, root, "before.txt", false)
		event := attachActionEvent(t, pool, root, id)
		result := runActionCommit(t, pool, actionOperation(root, "move", "before.txt", "after.txt", actionEffect(0, "move", "before.txt", "after.txt", storage.File)))
		assertActionID(t, pool, root, "before.txt", "")
		assertActionID(t, pool, root, "after.txt", id)
		var eventID string
		if err := pool.QueryRow(context.Background(), `select node_id::text from event_media where event_id=$1::uuid`, event).Scan(&eventID); err != nil || eventID != id {
			t.Fatalf("event detached: %s %v", eventID, err)
		}
		var m fc.Metadata
		if err := json.Unmarshal(result, &m); err != nil || m.Path != "/after.txt" || m.ContentRevision == nil {
			t.Fatalf("public result %s: %v", result, err)
		}
	})
	t.Run("copy creates independent private identity", func(t *testing.T) {
		root := actionOwner(t, pool)
		id := indexActionEntry(t, pool, root, "source.txt", false)
		runActionCommit(t, pool, actionOperation(root, "copy", "source.txt", "copy.txt", actionEffect(0, "copy", "source.txt", "copy.txt", storage.File)))
		assertActionID(t, pool, root, "source.txt", id)
		var target string
		if err := pool.QueryRow(context.Background(), `select id::text from nodes where owner_id=$1::uuid and path='copy.txt'`, root.OwnerID).Scan(&target); err != nil || target == id {
			t.Fatalf("copy identity=%s source=%s error=%v", target, id, err)
		}
	})
	t.Run("missing native source cannot move index", func(t *testing.T) {
		root := actionOwner(t, pool)
		id := indexActionEntry(t, pool, root, "missing.txt", false)
		effect := actionEffect(0, "move", "missing.txt", "target.txt", storage.File)
		effect.State = "not_applied"
		effect.Result = nil
		result := runActionCommit(t, pool, actionOperation(root, "move", "missing.txt", "target.txt", effect))
		if len(result) != 0 {
			t.Fatalf("missing source reported result %s", result)
		}
		assertActionID(t, pool, root, "missing.txt", id)
		assertActionID(t, pool, root, "target.txt", "")
	})
	t.Run("proven folder deletion clears stale indexed descendants", func(t *testing.T) {
		root := actionOwner(t, pool)
		indexActionEntry(t, pool, root, "folder", true)
		indexActionEntry(t, pool, root, "folder/uncaptured.txt", false)
		result := runActionCommit(t, pool, actionOperation(root, "delete", "folder", "", actionEffect(0, "delete", "folder", "", storage.Folder)))
		assertActionID(t, pool, root, "folder", "")
		assertActionID(t, pool, root, "folder/uncaptured.txt", "")
		if string(result) != "{\"deleted\":true}" {
			t.Fatalf("delete result %s", result)
		}
	})
	t.Run("copy delete folder move retains every proven identity", func(t *testing.T) {
		root := actionOwner(t, pool)
		folder := indexActionEntry(t, pool, root, "from", true)
		child := indexActionEntry(t, pool, root, "from/a.txt", false)
		indexActionEntry(t, pool, root, "from/not-captured.txt", false)
		event := attachActionEvent(t, pool, root, child)
		effects := []fileops.EntryEffect{actionEffect(0, "mkdir", "", "to", storage.Folder), actionEffect(1, "copy", "from/a.txt", "to/a.txt", storage.File), actionEffect(2, "delete", "from/a.txt", "", storage.File), actionEffect(3, "delete", "from", "", storage.Folder)}
		result := runActionCommit(t, pool, actionOperation(root, "move", "from", "to", effects...))
		assertActionID(t, pool, root, "to", folder)
		assertActionID(t, pool, root, "to/a.txt", child)
		assertActionID(t, pool, root, "from/a.txt", "")
		assertActionID(t, pool, root, "from/not-captured.txt", "")
		var linked string
		if err := pool.QueryRow(context.Background(), `select node_id::text from event_media where event_id=$1::uuid`, event).Scan(&linked); err != nil || linked != child {
			t.Fatalf("descendant event lost: %s %v", linked, err)
		}
		var m fc.Metadata
		if err := json.Unmarshal(result, &m); err != nil {
			t.Fatal(err)
		}
		var folderResult storage.MutationResult
		json.Unmarshal(effects[0].Result, &folderResult)
		record, err := fileBrowseMetadata(context.Background(), pool, root.OwnerID, fileRootNamespace(root), "to", nil)
		if err != nil || record == nil {
			t.Fatalf("folder membership %+v %v", record, err)
		}
		expected, err := IndexedMetadata(fileRootNamespace(root), *record)
		if err != nil {
			t.Fatal(err)
		}
		if m.Revision != expected.Revision || m.ChangeToken != expected.ChangeToken {
			t.Fatalf("folder metadata differs from browse: response=%s expected=%+v", result, expected)
		}
	})
	t.Run("new folder publication removes stale destination descendants only", func(t *testing.T) {
		root := actionOwner(t, pool)
		other := actionOwner(t, pool)
		indexActionEntry(t, pool, root, "target%_", true)
		indexActionEntry(t, pool, root, "target%_/ghost.txt", false)
		sibling := indexActionEntry(t, pool, root, "targetXX/keep.txt", false)
		otherID := indexActionEntry(t, pool, other, "target%_/ghost.txt", false)
		effects := []fileops.EntryEffect{actionEffect(0, "mkdir", "", "target%_", storage.Folder), actionEffect(1, "copy", "source.txt", "target%_/actual.txt", storage.File)}
		runActionCommit(t, pool, actionOperation(root, "copy", "source", "target%_", effects...))
		assertActionID(t, pool, root, "target%_/ghost.txt", "")
		assertActionID(t, pool, root, "targetXX/keep.txt", sibling)
		assertActionID(t, pool, other, "target%_/ghost.txt", otherID)
		record, err := fileBrowseMetadata(context.Background(), pool, root.OwnerID, fileRootNamespace(root), "target%_", nil)
		if err != nil || record == nil || len(record.Membership) != 1 || record.Membership[0] != "membership:1" {
			t.Fatalf("new folder membership %+v %v", record, err)
		}
	})
	t.Run("partial move preserves source and records only proven target", func(t *testing.T) {
		root := actionOwner(t, pool)
		source := indexActionEntry(t, pool, root, "from.txt", false)
		pending := actionEffect(1, "delete", "from.txt", "", storage.File)
		pending.State = "not_applied"
		pending.Result = nil
		result := runActionCommit(t, pool, actionOperation(root, "move", "from.txt", "to.txt", actionEffect(0, "copy", "from.txt", "to.txt", storage.File), pending))
		if len(result) != 0 {
			t.Fatalf("partial callback replaced explicit remaining progress: %s", result)
		}
		assertActionID(t, pool, root, "from.txt", source)
		var target string
		if err := pool.QueryRow(context.Background(), `select id::text from nodes where owner_id=$1::uuid and path='to.txt'`, root.OwnerID).Scan(&target); err != nil || target == source {
			t.Fatalf("partial copy identity=%s source=%s error=%v", target, source, err)
		}
	})
	t.Run("callback failure rolls back every index effect", func(t *testing.T) {
		root := actionOwner(t, pool)
		source := indexActionEntry(t, pool, root, "source.txt", false)
		if _, err := pool.Exec(context.Background(), `create function reject_action_target() returns trigger language plpgsql as $$ begin if new.path='reject.txt' then raise exception 'injected index failure'; end if; return new; end $$; create trigger reject_action_target before insert or update on nodes for each row execute function reject_action_target()`); err != nil {
			t.Fatal(err)
		}
		defer pool.Exec(context.Background(), `drop trigger reject_action_target on nodes; drop function reject_action_target()`)
		tx, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		op := actionOperation(root, "copy", "source.txt", "first.txt", actionEffect(0, "copy", "source.txt", "first.txt", storage.File), actionEffect(1, "copy", "source.txt", "reject.txt", storage.File))
		if _, err = CommitFileAction(context.Background(), tx, op, nil); err == nil {
			t.Fatal("injected callback failure was accepted")
		}
		tx.Rollback(context.Background())
		assertActionID(t, pool, root, "first.txt", "")
		assertActionID(t, pool, root, "source.txt", source)
	})
	t.Run("deletion requires captured version proof", func(t *testing.T) {
		root := actionOwner(t, pool)
		id := indexActionEntry(t, pool, root, "source.txt", false)
		effect := actionEffect(0, "delete", "source.txt", "", storage.File)
		effect.SourceRevision = nil
		tx, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err = CommitFileAction(context.Background(), tx, actionOperation(root, "delete", "source.txt", "", effect), nil); !fileops.IsCode(err, "invalid_native_result") {
			t.Fatalf("missing proof accepted: %v", err)
		}
		assertActionID(t, pool, root, "source.txt", id)
	})
}

func TestCommitStagedFilePublicRevision(t *testing.T) {
	pool := actionTestDatabase(t)
	root := actionOwner(t, pool)
	ctx := context.Background()
	intent := storage.WriteIntent{Target: "file.txt", Size: 3}
	encoded, _ := json.Marshal(intent)
	entry := storage.Entry{Path: "file.txt", Kind: storage.File, Size: 3, Revision: []byte("selected-version"), ContentIdentity: []byte("inode"), ModifiedAt: time.Date(2026, 10, 31, 12, 0, 0, 123456000, time.UTC)}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	raw, err := CommitStagedFile(ctx, tx, fileops.Operation{Root: root, Intent: encoded}, &storage.MutationResult{Entry: &entry})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var metadata fc.Metadata
	if err = json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	// Public reads use nodeColumns' timestamp representation, including milliseconds.
	row, err := scanNode(pool.QueryRow(ctx, `select `+nodeColumns+` from nodes where owner_id=$1::uuid and path='file.txt'`, root.OwnerID))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.CreatedAt.Format(time.RFC3339Nano) != row.CreatedAt || metadata.UpdatedAt.Format(time.RFC3339Nano) != row.UpdatedAt {
		created, _ := time.Parse(time.RFC3339Nano, row.CreatedAt)
		updated, _ := time.Parse(time.RFC3339Nano, row.UpdatedAt)
		if !metadata.CreatedAt.Equal(created) || !metadata.UpdatedAt.Equal(updated) {
			t.Fatalf("durable timestamps differ from read: metadata=%s row=%+v", raw, row)
		}
	}
	if metadata.ContentRevision == nil || *metadata.ContentRevision != *filerevision.Content(fileRootNamespace(root), entry) {
		t.Fatal("durable result omitted selected content revision")
	}
	if metadata.Revision != filerevision.Metadata(fileRootNamespace(root), metadata, nil, nil) {
		t.Fatalf("durable revision differs from read contract: %s", raw)
	}
	if metadata.MediaType == nil || *metadata.MediaType != "application/octet-stream" {
		t.Fatal(fmt.Sprintf("incorrect fallback media type: %+v", metadata.MediaType))
	}
}
