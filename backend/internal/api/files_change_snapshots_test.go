package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/filechanges"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
	"github.com/jackc/pgx/v5"
)

type changeSnapshotFixture struct {
	s      *Server
	f      *fileRequest
	driver *filesReadTestDriver
}

func changeSnapshotDatabase(t *testing.T) *store.DB {
	t.Helper()
	scratch := testpg.CreateDatabase(t, "eam_snapshots_")
	ctx := context.Background()
	db, err := store.Open(ctx, scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
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
			if _, err = db.Pool().Exec(ctx, string(data)); err != nil {
				t.Fatalf("snapshot migration %s pass%d: %v", filepath.Base(migration), pass+1, err)
			}
		}
	}
	return db
}
func newChangeSnapshotFixture(t *testing.T, db *store.DB) *changeSnapshotFixture {
	t.Helper()
	ctx := context.Background()
	var owner string
	if err := db.Pool().QueryRow(ctx, `insert into users(email,password_hash) values(gen_random_uuid()::text||'@snapshot.test','test-only') returning id::text`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	// An unavailable native backend proves snapshots and every page are indexed.
	driver := &filesReadTestDriver{unavailable: true, descriptor: storage.Descriptor{Names: storage.NameRules{CaseSensitive: true}}}
	s := &Server{db: db, cfg: &config.Config{AppSecret: "persistent-snapshot-test-secret"}, filesIndex: nodes.New(db, nil)}
	f := &fileRequest{Principal: &auth.Principal{User: &auth.User{ID: owner}}, Source: fc.CookieCredential, Driver: driver, Scope: "/", ContentNamespace: "snapshot-root:" + owner, Namespace: "snapshot-owner:" + owner}
	fixture := &changeSnapshotFixture{s: s, f: f, driver: driver}
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = filechanges.BindRoot(ctx, tx, owner, f.ContentNamespace); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `select file_identity_root($1::uuid)`, owner); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return fixture
}
func (fx *changeSnapshotFixture) seed(t *testing.T, p string, folder bool) string {
	t.Helper()
	parent := path.Dir(p)
	if parent == "." {
		parent = ""
	}
	key := "drive/" + fx.f.Principal.User.ID + "/" + p
	if folder {
		key += "/"
	}
	var id string
	err := fx.s.db.Pool().QueryRow(context.Background(), `insert into nodes(owner_id,key,path,parent,name,is_dir,size,mime) values($1::uuid,$2,$3,$4,$5,$6,0,case when $6 then null else 'text/plain' end) returning id::text`, fx.f.Principal.User.ID, key, p, parent, path.Base(p), folder).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func (fx *changeSnapshotFixture) call(t *testing.T, method, id, query, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	target := fc.Base + "/change-snapshots"
	if id != "" {
		target += "/" + id
	}
	if query != "" {
		target += "?" + query
	}
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	var err error
	switch method {
	case http.MethodPost:
		err = fx.s.handleFilesCreateChangeSnapshot(w, r, fx.f)
	case http.MethodGet:
		err = fx.s.handleFilesGetChangeSnapshot(w, r, fx.f)
	case http.MethodDelete:
		err = fx.s.handleFilesDeleteChangeSnapshot(w, r, fx.f)
	}
	return w, err
}
func (fx *changeSnapshotFixture) create(t *testing.T, filter filesChangeFilter) filesChangeSnapshot {
	t.Helper()
	body, _ := json.Marshal(filter)
	w, err := fx.call(t, http.MethodPost, "", "", string(body))
	if err != nil || w.Code != 201 {
		t.Fatalf("create snapshot status%d: %v body%s", w.Code, err, w.Body.String())
	}
	var snapshot filesChangeSnapshot
	if err = json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ID == "" || snapshot.ChangesCursor == "" || snapshot.State != "ready" || w.Header().Get("Location") != snapshot.EntriesURL {
		t.Fatalf("incomplete snapshot resource: %s", w.Body.String())
	}
	return snapshot
}

type snapshotTestPage struct {
	filesChangeSnapshot
	Items      []fc.Metadata `json:"items"`
	NextCursor *string       `json:"nextCursor"`
}

func (fx *changeSnapshotFixture) page(t *testing.T, id, query string) snapshotTestPage {
	t.Helper()
	w, err := fx.call(t, http.MethodGet, id, query, "")
	if err != nil || w.Code != 200 {
		t.Fatalf("snapshot page status%d: %v body%s", w.Code, err, w.Body.String())
	}
	var page snapshotTestPage
	if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page
}
func (fx *changeSnapshotFixture) appendUpdate(t *testing.T, p string) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := fx.s.db.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	before, err := nodes.CaptureFileSnapshot(ctx, tx, fx.f.Principal.User.ID, fx.f.ContentNamespace, p, false, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `update nodes set size=size+1,updated_at=clock_timestamp() where owner_id=$1::uuid and id=$2::uuid`, fx.f.Principal.User.ID, before[0].ID); err != nil {
		t.Fatal(err)
	}
	after, err := nodes.CaptureFileSnapshot(ctx, tx, fx.f.Principal.User.ID, fx.f.ContentNamespace, p, false, 100)
	if err != nil {
		t.Fatal(err)
	}
	ancestors := []filechanges.Ancestor{}
	if before[0].ParentID != nil {
		ancestors = append(ancestors, filechanges.Ancestor{ID: *before[0].ParentID, Path: before[0].ParentPath})
	}
	position, err := filechanges.Append(ctx, tx, filechanges.Change{OwnerID: fx.f.Principal.User.ID, EffectKey: fmt.Sprintf("snapshot-update:%s:%d", before[0].ID, *after[0].Size), Kind: "updated", Before: &before[0], After: &after[0], BeforeAncestors: ancestors, AfterAncestors: ancestors, Origin: "snapshot-test"})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return position
}

func TestFilesChangeSnapshotCursorAndValidation(t *testing.T) {
	s, f, _, _ := filesReadFixture()
	expires := time.Now().Add(time.Hour).Unix()
	id := mutationFolderID
	token, err := s.filesEncodeSnapshotCursor(f, filesSnapshotPageCursor{SnapshotID: id, After: 7, Expires: expires})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := s.filesDecodeSnapshotCursor(f, id, token)
	if err != nil || decoded.After != 7 {
		t.Fatalf("valid snapshot cursor: %+v %v", decoded, err)
	}
	other, err := s.filesEncodeSnapshotCursor(f, filesSnapshotPageCursor{SnapshotID: id, After: 99999, Expires: expires})
	if err != nil || len(other) != len(token) {
		t.Fatal("page ordinal leaked through cursor length")
	}
	if _, err = s.filesDecodeSnapshotCursor(f, mutationOtherID, token); err == nil {
		t.Fatal("snapshot cursor accepted for another resource")
	}
	f.Scope = "/changed"
	if _, err = s.filesDecodeSnapshotCursor(f, id, token); err == nil {
		t.Fatal("page cursor survived policy change")
	}
	for _, query := range []string{"cursor=", "limit=0", "limit=1001", "limit=1&limit=2", "path=%2F", "cursor=%zz"} {
		if _, _, err = filesSnapshotQuery(httptest.NewRequest("GET", "/api/files/change-snapshots/id?"+query, nil)); err == nil {
			t.Fatalf("invalid page query accepted: %s", query)
		}
	}
	f.Principal.Token = &auth.Token{CanRead: true}
	if err = f.requireChanges(false); err == nil || filesProblem(err).Status != 403 {
		t.Fatal("snapshot grant defaults permitted access")
	}
}

func TestFilesChangeSnapshotsPostgres(t *testing.T) {
	db := changeSnapshotDatabase(t)
	ctx := context.Background()
	t.Run("immutable pages survive writes moves deletion and restart", func(t *testing.T) {
		fx := newChangeSnapshotFixture(t, db)
		folder := fx.seed(t, "Scope", true)
		fx.seed(t, "Scope/a", false)
		child := fx.seed(t, "Scope/Nested", true)
		fx.seed(t, "Scope/Nested/b", false)
		snapshot := fx.create(t, filesChangeFilter{Target: fc.EntrySelector{ID: folder}, Kind: "folder", Recursive: true})
		if snapshot.TotalEntries != 4 {
			t.Fatalf("snapshot count=%d", snapshot.TotalEntries)
		}
		first := fx.page(t, snapshot.ID, "limit=1")
		if len(first.Items) != 1 || first.Items[0].ID != folder || first.NextCursor == nil {
			t.Fatalf("first snapshot page: %+v", first)
		}
		checkpoint, err := fx.s.filesDecodeChangeCursor(fx.f, snapshot.ChangesCursor)
		if err != nil || checkpoint.After != 0 {
			t.Fatalf("snapshot checkpoint: %+v %v", checkpoint, err)
		}
		fx.appendUpdate(t, "Scope/a")
		fx.seed(t, "Scope/after-capture", false)
		if _, err = db.Pool().Exec(ctx, `update nodes set path='Moved',parent='',name='Moved',key='drive/'||owner_id::text||'/Moved/' where owner_id=$1::uuid and id=$2::uuid`, fx.f.Principal.User.ID, folder); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Pool().Exec(ctx, `delete from nodes where owner_id=$1::uuid and id=$2::uuid`, fx.f.Principal.User.ID, child); err != nil {
			t.Fatal(err)
		}
		restarted := &Server{db: db, cfg: fx.s.cfg, filesIndex: nodes.New(db, nil)}
		fx.s = restarted
		all := append([]fc.Metadata{}, first.Items...)
		next := first.NextCursor
		for next != nil {
			page := fx.page(t, snapshot.ID, "limit=1&cursor="+url.QueryEscape(*next))
			if page.ChangesCursor != snapshot.ChangesCursor {
				t.Fatal("snapshot checkpoint moved across pages")
			}
			all = append(all, page.Items...)
			next = page.NextCursor
		}
		if len(all) != 4 {
			t.Fatalf("mutable live view changed snapshot size: %d", len(all))
		}
		seen := map[fc.Path]bool{}
		for _, item := range all {
			if seen[item.Path] {
				t.Fatal("duplicate immutable snapshot item")
			}
			seen[item.Path] = true
			if item.Path == "/Scope/a" && *item.Size != 0 {
				t.Fatal("snapshot metadata followed a later write")
			}
			if !strings.HasPrefix(string(item.Path), "/Scope") {
				t.Fatalf("snapshot followed later move: %s", item.Path)
			}
		}
		changes, err := fx.s.filesReadChanges(ctx, fx.f, checkpoint, 10)
		if err != nil || len(changes.Items) != 1 {
			t.Fatalf("snapshot feed checkpoint lost subsequent update: %d %v", len(changes.Items), err)
		}
		if fx.driver.statCalls != 0 || fx.driver.listCalls != 0 || fx.driver.openCalls != 0 {
			t.Fatal("snapshot touched native storage")
		}
		w, err := fx.call(t, "DELETE", snapshot.ID, "", "")
		if err != nil || w.Code != 204 {
			t.Fatalf("delete snapshot: %d %v", w.Code, err)
		}
		var count int
		if err = db.Pool().QueryRow(ctx, `select count(*) from file_change_snapshot_entries where owner_id=$1::uuid and snapshot_id=$2::uuid`, fx.f.Principal.User.ID, snapshot.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("snapshot entries not deleted: %d %v", count, err)
		}
	})
	t.Run("head and index share the same repeatable-read observation", func(t *testing.T) {
		fx := newChangeSnapshotFixture(t, db)
		fx.seed(t, "file", false)
		tx, err := db.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		head, err := filechanges.ReadHead(ctx, tx, fx.f.Principal.User.ID)
		if err != nil {
			t.Fatal(err)
		}
		fx.appendUpdate(t, "file")
		items, err := nodes.CaptureFileSnapshot(ctx, tx, fx.f.Principal.User.ID, fx.f.ContentNamespace, "file", false, 100)
		if err != nil || head.Position != 0 || *items[0].Size != 0 {
			t.Fatalf("mixed snapshot window head%d items%+v %v", head.Position, items, err)
		}
		token, err := fx.s.filesChangeCheckpoint(fx.f, filesChangeFilter{Target: fc.EntrySelector{Path: "/file"}, Kind: "file"}, "/file", head, time.Now().Add(filesSnapshotLifetime))
		if err != nil {
			t.Fatal(err)
		}
		checkpoint, err := fx.s.filesDecodeChangeCursor(fx.f, token)
		if err != nil {
			t.Fatal(err)
		}
		page, err := fx.s.filesReadChanges(ctx, fx.f, checkpoint, 10)
		if err != nil || len(page.Items) != 1 || *page.Items[0].After.Size != 1 {
			t.Fatalf("concurrent commit fell between baseline and checkpoint: %+v %v", page, err)
		}
	})
	t.Run("root shallow recursive and file baselines are complete", func(t *testing.T) {
		fx := newChangeSnapshotFixture(t, db)
		fx.seed(t, "Scope", true)
		fx.seed(t, "Scope/nested-file", false)
		fileID := fx.seed(t, "root-file", false)
		shallow := fx.create(t, filesChangeFilter{Target: fc.EntrySelector{Path: "/"}, Kind: "folder"})
		if shallow.TotalEntries != 3 {
			t.Fatalf("shallow root count=%d", shallow.TotalEntries)
		}
		rootPage := fx.page(t, shallow.ID, "")
		if rootPage.Items[0].Path != "/" || rootPage.Items[0].ParentID != nil || !fc.ValidEntryID(rootPage.Items[0].ID) {
			t.Fatalf("root metadata: %+v", rootPage.Items[0])
		}
		recursive := fx.create(t, filesChangeFilter{Target: fc.EntrySelector{ID: rootPage.Items[0].ID}, Kind: "folder", Recursive: true})
		if recursive.TotalEntries != 4 {
			t.Fatalf("recursive root count=%d", recursive.TotalEntries)
		}
		single := fx.create(t, filesChangeFilter{Target: fc.EntrySelector{ID: fileID}, Kind: "file"})
		if page := fx.page(t, single.ID, ""); len(page.Items) != 1 || page.Items[0].ID != fileID || page.NextCursor != nil {
			t.Fatalf("file baseline: %+v", page)
		}
		if _, err := db.Pool().Exec(ctx, `delete from file_change_snapshot_entries where snapshot_id=$1::uuid and ordinal=1`, shallow.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := fx.call(t, "GET", shallow.ID, "", ""); err == nil || filesProblem(err).Code != "snapshot_incomplete" {
			t.Fatalf("missing saved entries returned a partial baseline: %v", err)
		}
	})
	t.Run("entry bounds reject whole baselines without truncation", func(t *testing.T) {
		fx := newChangeSnapshotFixture(t, db)
		fx.seed(t, "Scope", true)
		fx.seed(t, "Scope/a", false)
		fx.seed(t, "Scope/b", false)
		tx, err := db.Pool().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = nodes.CaptureFileSnapshot(ctx, tx, fx.f.Principal.User.ID, fx.f.ContentNamespace, "Scope", true, 2); err == nil || filesProblem(err).Code != "snapshot_too_large" {
			t.Fatalf("oversized baseline silently truncated: %v", err)
		}
	})
	t.Run("policy epoch expiry and cross-owner isolation", func(t *testing.T) {
		fx := newChangeSnapshotFixture(t, db)
		fx.seed(t, "Scope", true)
		snapshot := fx.create(t, filesChangeFilter{Target: fc.EntrySelector{Path: "/Scope"}, Kind: "folder"})
		original := fx.f.ContentNamespace
		fx.f.ContentNamespace = "changed-root"
		if _, err := fx.call(t, "GET", snapshot.ID, "", ""); err == nil || filesProblem(err).Status != 404 {
			t.Fatalf("snapshot survived root change: %v", err)
		}
		fx.f.ContentNamespace = original
		other := newChangeSnapshotFixture(t, db)
		if _, err := other.call(t, "GET", snapshot.ID, "", ""); err == nil || filesProblem(err).Status != 404 {
			t.Fatalf("foreign snapshot disclosed: %v", err)
		}
		if _, err := db.Pool().Exec(ctx, `update file_change_heads set root_epoch=gen_random_uuid() where owner_id=$1::uuid`, fx.f.Principal.User.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := fx.call(t, "GET", snapshot.ID, "", ""); err == nil || filesProblem(err).Status != 410 {
			t.Fatalf("snapshot survived epoch reset: %v", err)
		}
		newer := fx.create(t, filesChangeFilter{Target: fc.EntrySelector{Path: "/Scope"}, Kind: "folder"})
		if _, err := db.Pool().Exec(ctx, `update file_change_snapshots set expires_at=clock_timestamp()-interval '1 second' where id=$1::uuid`, newer.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := fx.call(t, "GET", newer.ID, "", ""); err == nil || filesProblem(err).Code != "snapshot_expired" {
			t.Fatalf("expired snapshot returned metadata: %v", err)
		}
		if _, err := fx.call(t, "DELETE", newer.ID, "", ""); err != nil {
			t.Fatal("expired snapshot could not be released", err)
		}
	})
}

func TestFilesChangeSnapshotGuardIntersectionPostgres(t *testing.T) {
	db := changeSnapshotDatabase(t)
	fx := newChangeSnapshotFixture(t, db)
	ctx := context.Background()
	fx.seed(t, "Scope", true)
	fx.seed(t, "Scope/Nested", true)
	fx.seed(t, "Scope/Nested/file", false)
	var op string
	// Only the durable snapshot and path guards matter; no native driver is opened.
	root := map[string]any{"OwnerID": fx.f.Principal.User.ID, "CaseSensitive": true}
	raw, _ := json.Marshal(map[string]any{"Root": root, "State": "outcome_unknown", "Phase": "effect_started"})
	err := db.Pool().QueryRow(ctx, `insert into file_operations(id,owner_id,principal_id,action,fingerprint,snapshot) values(gen_random_uuid(),$1::uuid,$2,'move','snapshot-guard',$3::jsonb) returning id::text`, fx.f.Principal.User.ID, filePrincipalID(fx.f.Principal), raw).Scan(&op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool().Exec(ctx, `insert into file_operation_guards(owner_id,operation_id,ordinal,native_path,comparison_path,subtree) values($1::uuid,$2::uuid,0,'Scope/Nested','Scope/Nested',true)`, fx.f.Principal.User.ID, op); err != nil {
		t.Fatal(err)
	}
	body := `{"target":{"path":"/Scope"},"kind":"folder","recursive":true}`
	if _, err = fx.call(t, "POST", "", "", body); err == nil || filesProblem(err).Code != "operation_in_progress" {
		t.Fatalf("descendant unknown guard missed: %v", err)
	}
	var count int
	if err = db.Pool().QueryRow(ctx, `select count(*) from file_change_snapshots where owner_id=$1::uuid`, fx.f.Principal.User.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("blocked snapshot partly persisted: %d %v", count, err)
	}
}
