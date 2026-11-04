package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"everythingabout.me/backend/internal/auth"
	fc "everythingabout.me/backend/internal/filecontract"
)

func TestFilesChangeSnapshotGrantAndAdmissionPostgres(t *testing.T) {
	db := changeSnapshotDatabase(t)
	ctx := context.Background()
	t.Run("current grants concealed parents and moved-out scope", func(t *testing.T) {
		fx := newChangeSnapshotFixture(t, db)
		folder := fx.seed(t, "Scope", true)
		fx.seed(t, "Scope/file", false)
		tokenID := "snapshot-grant-token"
		_, err := db.Pool().Exec(ctx, `insert into api_tokens(token_id,token_hash,user_id,name,prefix,can_read,can_write,can_changes)
   values($1,'test-only',$2::uuid,'snapshot test','Scope',true,false,true)`, tokenID, fx.f.Principal.User.ID)
		if err != nil {
			t.Fatal(err)
		}
		fx.f.Principal.Token = &auth.Token{ID: tokenID, Prefix: "Scope", CanRead: true, CanChanges: true, PolicyVersion: 1}
		fx.f.Source = fc.BearerCredential
		fx.f.Scope = "/Scope"
		snapshot := fx.create(t, filesChangeFilter{Target: fc.EntrySelector{ID: folder}, Kind: "folder", Recursive: true})
		page := fx.page(t, snapshot.ID, "")
		if len(page.Items) != 2 || page.Items[0].ParentID != nil || page.Items[0].ParentPath != "" {
			t.Fatalf("concealed target parent leaked: %+v", page.Items)
		}
		if page.Items[1].ParentID == nil || *page.Items[1].ParentID != folder {
			t.Fatal("visible child parent incorrectly hidden")
		}
		if _, err = db.Pool().Exec(ctx, `update nodes set path='Gone',parent='',name='Gone',key='drive/'||owner_id::text||'/Gone/' where owner_id=$1::uuid and id=$2::uuid`, fx.f.Principal.User.ID, folder); err != nil {
			t.Fatal(err)
		}
		if _, err = fx.call(t, "GET", snapshot.ID, "", ""); err == nil || filesProblem(err).Status != 404 {
			t.Fatalf("ID snapshot followed identity outside path grant: %v", err)
		}
		if _, err = db.Pool().Exec(ctx, `update nodes set path='Scope',parent='',name='Scope',key='drive/'||owner_id::text||'/Scope/' where owner_id=$1::uuid and id=$2::uuid`, fx.f.Principal.User.ID, folder); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Pool().Exec(ctx, `update api_tokens set revoked_at=clock_timestamp() where token_id=$1`, tokenID); err != nil {
			t.Fatal(err)
		}
		w, err := fx.call(t, "GET", snapshot.ID, "", "")
		if err == nil || filesProblem(err).Status != 403 || w.Body.Len() != 0 {
			t.Fatalf("revoked snapshot grant returned metadata: %v %s", err, w.Body.String())
		}
		if _, err = db.Pool().Exec(ctx, `update api_tokens set revoked_at=null,policy_version=policy_version+1 where token_id=$1`, tokenID); err != nil {
			t.Fatal(err)
		}
		fx.f.Principal.Token.PolicyVersion = 2
		if _, err = fx.call(t, "GET", snapshot.ID, "", ""); err == nil || filesProblem(err).Status != 404 {
			t.Fatalf("restored grant revived old snapshot: %v", err)
		}
	})
	t.Run("concurrent admission cannot exceed the persisted resource bound", func(t *testing.T) {
		fx := newChangeSnapshotFixture(t, db)
		fx.seed(t, "file", false)
		filter := filesChangeFilter{Target: fc.EntrySelector{Path: "/file"}, Kind: "file"}
		first := fx.create(t, filter)
		for i := 1; i < filesSnapshotPrincipalLimit-1; i++ {
			fx.create(t, filter)
		}
		body, _ := json.Marshal(filter)
		type result struct {
			status int
			err    error
		}
		results := make(chan result, 2)
		start := make(chan struct{})
		for i := 0; i < 2; i++ {
			go func() {
				<-start
				w, err := fx.call(t, http.MethodPost, "", "", string(body))
				results <- result{w.Code, err}
			}()
		}
		close(start)
		successes := 0
		for i := 0; i < 2; i++ {
			outcome := <-results
			if outcome.err == nil && outcome.status == 201 {
				successes++
				continue
			}
			if outcome.err == nil || (filesProblem(outcome.err).Status != 409 && filesProblem(outcome.err).Status != 503) {
				t.Fatalf("concurrent snapshot admission: %+v", outcome)
			}
		}
		if successes != 1 {
			t.Fatalf("admitted %d concurrent snapshots, want exactly one", successes)
		}
		var count int
		if err := db.Pool().QueryRow(ctx, `select count(*) from file_change_snapshots where owner_id=$1::uuid and expires_at>clock_timestamp()`, fx.f.Principal.User.ID).Scan(&count); err != nil || count != filesSnapshotPrincipalLimit {
			t.Fatalf("snapshot bound count=%d %v", count, err)
		}
		if _, err := db.Pool().Exec(ctx, `update file_change_snapshot_entries set metadata=metadata||'{"name":"tampered"}'::jsonb where snapshot_id=$1::uuid`, first.ID); err == nil {
			t.Fatal("immutable snapshot entries accepted an update")
		}
		if _, err := fx.call(t, http.MethodPost, "", "", string(body)); err == nil || filesProblem(err).Code != "snapshot_limit" {
			t.Fatalf("snapshot limit not enforced: %v", err)
		}
		if _, err := fx.call(t, http.MethodDelete, first.ID, "", ""); err != nil {
			t.Fatal(err)
		}
		replacement := fx.create(t, filter)
		if replacement.ID == first.ID {
			t.Fatal("snapshot ID reused after deletion")
		}
	})
}
