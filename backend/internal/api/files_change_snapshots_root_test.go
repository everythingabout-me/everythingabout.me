package api

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"

	"everythingabout.me/backend/internal/filechanges"
	fc "everythingabout.me/backend/internal/filecontract"
	"github.com/jackc/pgx/v5"
)

func TestFilesChangeSnapshotRootConfigurationABAPostgres(t *testing.T) {
	db := changeSnapshotDatabase(t)
	fx := newChangeSnapshotFixture(t, db)
	ctx := context.Background()
	fx.seed(t, "Scope", true)
	fx.seed(t, "Scope/file", false)
	filter := filesChangeFilter{Target: fc.EntrySelector{Path: "/Scope"}, Kind: "folder", Recursive: true}
	originalNamespace := fx.f.ContentNamespace
	first := fx.create(t, filter)
	firstPage := fx.page(t, first.ID, "limit=1")
	if firstPage.NextCursor == nil {
		t.Fatal("fixture did not create a page continuation")
	}
	firstCheckpoint, err := fx.s.filesDecodeChangeCursor(fx.f, first.ChangesCursor)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(filter)
	fx.f.ContentNamespace = originalNamespace + ":replacement-root"
	if _, err = fx.call(t, "POST", "", "", string(body)); err == nil || filesProblem(err).Status != 503 || filesProblem(err).Code != "change_root_reset_required" {
		t.Fatalf("config change silently rebound snapshot root: %v", err)
	}
	head, err := filechanges.ReadHead(ctx, db.Pool(), fx.f.Principal.User.ID)
	if err != nil || head.RootNamespace != originalNamespace || head.RootEpoch != firstCheckpoint.Epoch {
		t.Fatalf("failed admission changed durable root: %+v %v", head, err)
	}
	var count int
	if err = db.Pool().QueryRow(ctx, `select count(*) from file_change_snapshots where owner_id=$1::uuid`, fx.f.Principal.User.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("root mismatch partly persisted snapshot: %d %v", count, err)
	}
	reset := func(namespace string) filechanges.Head {
		t.Helper()
		tx, err := db.Pool().BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		next, err := filechanges.ResetRoot(ctx, tx, fx.f.Principal.User.ID, namespace)
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return next
	}
	replacementHead := reset(fx.f.ContentNamespace)
	if replacementHead.RootEpoch == firstCheckpoint.Epoch {
		t.Fatal("explicit root reset reused old epoch")
	}
	fx.f.ContentNamespace = originalNamespace
	if _, err = fx.call(t, "POST", "", "", string(body)); err == nil || filesProblem(err).Code != "change_root_reset_required" {
		t.Fatalf("returning config A silently rebound B: %v", err)
	}
	if _, err = fx.call(t, "GET", first.ID, "limit=1&cursor="+url.QueryEscape(*firstPage.NextCursor), ""); err == nil || filesProblem(err).Status != 410 {
		t.Fatalf("old A page revived while durable root remains B: %v", err)
	}
	finalHead := reset(originalNamespace)
	if finalHead.RootEpoch == firstCheckpoint.Epoch || finalHead.RootEpoch == replacementHead.RootEpoch {
		t.Fatal("A to B to A reset reused a previous epoch")
	}
	// Even if retained metadata has a future expiry, the reset epoch must prevent
	// its old page cursor from becoming valid again under configuration A.
	if _, err = db.Pool().Exec(ctx, `update file_change_snapshots set expires_at=$2 where id=$1::uuid`, first.ID, first.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	if _, err = fx.call(t, "GET", first.ID, "limit=1&cursor="+url.QueryEscape(*firstPage.NextCursor), ""); err == nil || filesProblem(err).Status != 410 {
		t.Fatalf("A to B to A revived immutable baseline: %v", err)
	}
	fresh := fx.create(t, filter)
	currentCheckpoint, err := fx.s.filesDecodeChangeCursor(fx.f, fresh.ChangesCursor)
	if err != nil || currentCheckpoint.Epoch != finalHead.RootEpoch || currentCheckpoint.Epoch == firstCheckpoint.Epoch {
		t.Fatalf("fresh A snapshot did not bind current epoch: %+v %v", currentCheckpoint, err)
	}
}
