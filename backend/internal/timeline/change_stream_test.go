package timeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/events"
	"everythingabout.me/backend/internal/filechanges"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
)

func changeStreamDB(t *testing.T) (*store.DB, context.Context) {
	t.Helper()
	scratch := testpg.CreateDatabase(t, "eam_timeline_changes_")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	db, err := store.Open(ctx, scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatal("migrations unavailable", err)
	}
	for _, migration := range migrations {
		data, err := os.ReadFile(migration)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Pool().Exec(ctx, string(data)); err != nil {
			t.Fatalf("migration %s: %v", filepath.Base(migration), err)
		}
	}
	return db, ctx
}

type changeFixture struct {
	db    *store.DB
	ctx   context.Context
	s     *Service
	owner string
	photo string
}

const fixtureModified = "2026-05-12T10:00:00Z"

func newChangeFixture(t *testing.T) *changeFixture {
	t.Helper()
	db, ctx := changeStreamDB(t)
	var owner string
	if err := db.Pool().QueryRow(ctx, `insert into users(email,password_hash) values($1,'test') returning id::text`, "changes-"+fmt.Sprint(time.Now().UnixNano())+"@example.test").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	index := nodes.New(db, nil)
	s := New(db, nil, config.Timeline{Enabled: true, Workers: 1, MonthBudget: 24, Timezone: "UTC"}, events.New(db, index))
	insert := func(path string, folder bool) string {
		t.Helper()
		key := paths.KeyFor(path, owner)
		if folder {
			key = paths.DirKeyFor(path, owner)
		}
		var id string
		if err := db.Pool().QueryRow(ctx, `insert into nodes(owner_id,path,key,parent,name,is_dir,size,mime,etag,source_modified_at)
 values($1::uuid,$2,$3,$4,$5,$6,$7,'image/jpeg','etag-1',$8::timestamptz) returning id::text`,
			owner, path, key, paths.ParentOf(path), paths.NameOf(path), folder, 1234, fixtureModified).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	insert("Photos", true)
	photo := insert("Photos/a.jpg", false)
	return &changeFixture{db: db, ctx: ctx, s: s, owner: owner, photo: photo}
}

// changeMetadata is the After snapshot a settlement would record. The consumer
// never trusts it for the file facts (it re-reads the index by ID); revision and
// kind are what the trigger needs.
func changeMetadata(t *testing.T, id, path, revision string) fc.Metadata {
	t.Helper()
	size := int64(1234)
	media := "image/jpeg"
	modified, err := time.Parse(time.RFC3339, fixtureModified)
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := fc.FromRelative(path)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := fc.FromRelative(paths.ParentOf(path))
	if err != nil {
		t.Fatal(err)
	}
	return fc.Metadata{
		ID:               id,
		Path:             absolute,
		ParentPath:       parent,
		Name:             paths.NameOf(path),
		Kind:             "file",
		Revision:         revision,
		ContentRevision:  &revision,
		Size:             &size,
		MediaType:        &media,
		SourceModifiedAt: &modified,
		CreatedAt:        modified,
		UpdatedAt:        modified,
	}
}

func (f *changeFixture) appendChange(t *testing.T, kind string, before, after *fc.Metadata, key string) int64 {
	t.Helper()
	tx, err := f.db.Pool().Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	position, err := filechanges.Append(f.ctx, tx, filechanges.Change{
		OwnerID: f.owner, EffectKey: key, Kind: kind,
		Before: before, After: after, Origin: "api",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	return position
}

func (f *changeFixture) asset(t *testing.T) (revision string, narrative, screenshot bool, analyzedAt time.Time) {
	t.Helper()
	if err := f.db.Pool().QueryRow(f.ctx,
		`select source_revision, is_narrative, is_screenshot, analyzed_at
		   from timeline_assets where user_id=$1::uuid and node_id=$2::uuid`,
		f.owner, f.photo).Scan(&revision, &narrative, &screenshot, &analyzedAt); err != nil {
		t.Fatal(err)
	}
	return
}

// TestChangesJobAnalyzesMediaAndPublishesEvents is the end-to-end rescue: one
// settled fact is enough for the stream to analyze the photo and publish an event,
// without any scan of the tree.
func TestChangesJobAnalyzesMediaAndPublishesEvents(t *testing.T) {
	f := newChangeFixture(t)
	after := changeMetadata(t, f.photo, "Photos/a.jpg", "nr_photo_v1")
	head := f.appendChange(t, "created", nil, &after, "photo:create")

	if err := f.s.runChanges(f.ctx, &job{UserID: f.owner}); err != nil {
		t.Fatal(err)
	}

	revision, narrative, _, _ := f.asset(t)
	if revision != "nr_photo_v1" || !narrative {
		t.Fatalf("change did not reach the asset: revision=%q narrative=%t", revision, narrative)
	}
	var position int64
	if err := f.db.Pool().QueryRow(f.ctx, `select position from timeline_change_positions where owner_id=$1::uuid`, f.owner).Scan(&position); err != nil || position != head {
		t.Fatalf("checkpoint %d, want %d: %v", position, head, err)
	}
	var months int
	if err := f.db.Pool().QueryRow(f.ctx, `select count(*)::int from timeline_jobs where user_id=$1::uuid and kind='month'`, f.owner).Scan(&months); err != nil || months != 1 {
		t.Fatalf("month job count %d: %v", months, err)
	}

	if err := f.s.runMonth(f.ctx, &job{UserID: f.owner, Month: "2026-05"}); err != nil {
		t.Fatal(err)
	}
	var auto int
	if err := f.db.Pool().QueryRow(f.ctx, `select count(*)::int from events where user_id=$1::uuid and origin='auto'`, f.owner).Scan(&auto); err != nil || auto != 1 {
		t.Fatalf("auto events %d: %v", auto, err)
	}
}

// TestChangesJobIsIdempotentPerEntryRevision replays one fact and then moves the
// file with unchanged native evidence. The replay must not rewrite the asset, and
// the move must re-analyze it because path-dependent facts changed even though the
// content signature did not.
func TestChangesJobIsIdempotentPerEntryRevision(t *testing.T) {
	f := newChangeFixture(t)
	after := changeMetadata(t, f.photo, "Photos/a.jpg", "nr_photo_v1")
	f.appendChange(t, "created", nil, &after, "photo:create")
	if err := f.s.runChanges(f.ctx, &job{UserID: f.owner}); err != nil {
		t.Fatal(err)
	}
	_, _, screenshot, analyzedAt := f.asset(t)
	if screenshot {
		t.Fatal("Photos/a.jpg should not look like a screenshot")
	}

	changes, err := filechanges.Read(f.ctx, f.db.Pool(), f.owner, 0, 10, 10)
	if err != nil || len(changes) != 1 {
		t.Fatalf("read feed: %d %v", len(changes), err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := f.s.analyzeChanges(f.ctx, f.owner, changes); err != nil {
		t.Fatal(err)
	}
	if _, _, _, replayedAt := f.asset(t); !replayedAt.Equal(analyzedAt) {
		t.Fatalf("replayed revision re-analyzed the asset: %v -> %v", analyzedAt, replayedAt)
	}

	if _, err := f.db.Pool().Exec(f.ctx,
		`update nodes set path='Screenshots/a.jpg',parent='Screenshots',name='a.jpg',key=$3
		  where owner_id=$1::uuid and id=$2::uuid`,
		f.owner, f.photo, paths.KeyFor("Screenshots/a.jpg", f.owner)); err != nil {
		t.Fatal(err)
	}
	moved := changeMetadata(t, f.photo, "Screenshots/a.jpg", "nr_photo_v2")
	f.appendChange(t, "moved", &after, &moved, "photo:move")
	if err := f.s.runChanges(f.ctx, &job{UserID: f.owner}); err != nil {
		t.Fatal(err)
	}
	revision, _, screenshot, _ := f.asset(t)
	if revision != "nr_photo_v2" || !screenshot {
		t.Fatalf("move was not re-analyzed: revision=%q screenshot=%t", revision, screenshot)
	}
}

// TestChangesJobBackfillsWhenTheFeedNoLongerCarriesTheCheckpoint covers pruning,
// restore and reset: the retained feed cannot reconstruct the skipped positions, so
// the organizer must queue the existing full backfill instead of advancing silently.
func TestChangesJobBackfillsWhenTheFeedNoLongerCarriesTheCheckpoint(t *testing.T) {
	f := newChangeFixture(t)
	after := changeMetadata(t, f.photo, "Photos/a.jpg", "nr_photo_v1")
	f.appendChange(t, "created", nil, &after, "photo:create")
	if _, err := f.db.Pool().Exec(f.ctx,
		`update file_change_heads set retained_after=head_position where owner_id=$1::uuid`, f.owner); err != nil {
		t.Fatal(err)
	}

	if err := f.s.runChanges(f.ctx, &job{UserID: f.owner}); err != nil {
		t.Fatal(err)
	}
	var backfills int
	if err := f.db.Pool().QueryRow(f.ctx,
		`select count(*)::int from timeline_jobs where user_id=$1::uuid and kind='backfill'`, f.owner).Scan(&backfills); err != nil || backfills != 1 {
		t.Fatalf("backfill jobs %d: %v", backfills, err)
	}
	var position int64
	if err := f.db.Pool().QueryRow(f.ctx, `select position from timeline_change_positions where owner_id=$1::uuid`, f.owner).Scan(&position); err != nil || position != 1 {
		t.Fatalf("checkpoint did not move to the head: %d %v", position, err)
	}
}

// TestChangeFeederCoalescesOneJobPerOwner checks the only schedule: a head beyond
// the checkpoint creates one coalescing job, and a caught-up owner creates none.
func TestChangeFeederCoalescesOneJobPerOwner(t *testing.T) {
	f := newChangeFixture(t)
	after := changeMetadata(t, f.photo, "Photos/a.jpg", "nr_photo_v1")
	head := f.appendChange(t, "created", nil, &after, "photo:create")

	f.s.enqueueChangedOwners(f.ctx)
	f.s.enqueueChangedOwners(f.ctx)
	var pending int
	if err := f.db.Pool().QueryRow(f.ctx,
		`select count(*)::int from timeline_jobs where user_id=$1::uuid and kind='changes' and status='pending'`, f.owner).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("changes jobs %d: %v", pending, err)
	}

	if _, err := f.db.Pool().Exec(f.ctx,
		`update timeline_jobs set status='done' where user_id=$1::uuid and kind='changes'`, f.owner); err != nil {
		t.Fatal(err)
	}
	if err := f.s.saveChangePosition(f.ctx, f.owner, head); err != nil {
		t.Fatal(err)
	}
	f.s.enqueueChangedOwners(f.ctx)
	if err := f.db.Pool().QueryRow(f.ctx,
		`select count(*)::int from timeline_jobs where user_id=$1::uuid and kind='changes' and status='pending'`, f.owner).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("caught-up owner was scheduled again: %d %v", pending, err)
	}
}
