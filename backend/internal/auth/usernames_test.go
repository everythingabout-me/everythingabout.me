package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
)

// usernamePostgresService gives each run its own database with every migration applied,
// because the uniqueness this test is about is a schema property: the primary key on
// usernames.handle is what makes two accounts unable to own one address.
func usernamePostgresService(t *testing.T) (context.Context, *store.DB, *Service) {
	t.Helper()
	scratch := testpg.CreateDatabase(t, "eam_username_test_")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	db, err := store.Open(ctx, scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatal("repository migrations unavailable", err)
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
	return ctx, db, New(db, &config.Config{AppSecret: "isolated-username-test"})
}

func newAccount(t *testing.T, ctx context.Context, db *store.DB, email string) string {
	t.Helper()
	var id string
	if err := db.Pool().QueryRow(ctx,
		`insert into users(email,password_hash) values($1,'') returning id::text`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSetUsernameClaimsRenamesAndKeepsAliases(t *testing.T) {
	ctx, db, service := usernamePostgresService(t)
	owner := newAccount(t, ctx, db, "owner@example.test")
	other := newAccount(t, ctx, db, "other@example.test")

	row, err := service.SetUsername(ctx, owner, "iloahz")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if row.Username == nil || *row.Username != "iloahz" {
		t.Fatalf("claim did not set the handle: %+v", row.Username)
	}

	renamed, err := service.SetUsername(ctx, owner, "iloahz-drive")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed.Username == nil || *renamed.Username != "iloahz-drive" {
		t.Fatalf("rename did not move the handle: %+v", renamed.Username)
	}

	// The address a bookmark was minted from still resolves, and only for its owner.
	current, err := service.ResolveOwnUsername(ctx, owner, "iloahz")
	if err != nil || current != "iloahz-drive" {
		t.Fatalf("alias resolved to %q (%v), want iloahz-drive", current, err)
	}
	current, err = service.ResolveOwnUsername(ctx, owner, "iloahz-drive")
	if err != nil || current != "iloahz-drive" {
		t.Fatalf("current handle resolved to %q (%v)", current, err)
	}
	current, err = service.ResolveOwnUsername(ctx, other, "iloahz")
	if err != nil || current != "" {
		t.Fatalf("another account resolved a foreign alias to %q (%v)", current, err)
	}

	// A superseded handle is not free: another account may not take the name its old
	// links still redirect through.
	if _, err := service.SetUsername(ctx, other, "iloahz"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("claiming a live alias: %v, want ErrUsernameTaken", err)
	}

	// Renaming back is allowed, and the superseded name swaps sides.
	back, err := service.SetUsername(ctx, owner, "iloahz")
	if err != nil {
		t.Fatalf("rename back: %v", err)
	}
	if back.Username == nil || *back.Username != "iloahz" {
		t.Fatalf("rename back did not restore the handle: %+v", back.Username)
	}
	current, err = service.ResolveOwnUsername(ctx, owner, "iloahz-drive")
	if err != nil || current != "iloahz" {
		t.Fatalf("superseded handle resolved to %q (%v), want iloahz", current, err)
	}
}

func TestSetUsernamePublishesThroughEveryAccountRead(t *testing.T) {
	ctx, db, service := usernamePostgresService(t)
	owner := newAccount(t, ctx, db, "read-path@example.test")
	if _, err := service.SetUsername(ctx, owner, "read-path"); err != nil {
		t.Fatal(err)
	}
	// The handle is joined into the one account select, so an account read by id or by
	// email must carry it without a second query or a stale copy.
	byID, err := service.FindUserByID(ctx, owner)
	if err != nil || byID == nil || byID.Username == nil || *byID.Username != "read-path" {
		t.Fatalf("FindUserByID: %+v %v", byID, err)
	}
	byEmail, err := service.FindUserByEmail(ctx, "read-path@example.test")
	if err != nil || byEmail == nil || byEmail.Username == nil || *byEmail.Username != "read-path" {
		t.Fatalf("FindUserByEmail: %+v %v", byEmail, err)
	}
	user := ToUser(byID)
	if user.Username == nil || *user.Username != "read-path" {
		t.Fatalf("payload lost the handle: %+v", user.Username)
	}
}

func TestSetUsernameConcurrentClaimHasOneWinner(t *testing.T) {
	ctx, db, service := usernamePostgresService(t)
	first := newAccount(t, ctx, db, "first@example.test")
	second := newAccount(t, ctx, db, "second@example.test")

	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, id := range []string{first, second} {
		wait.Add(1)
		go func(userID string) {
			defer wait.Done()
			<-start
			_, err := service.SetUsername(ctx, userID, "contested")
			results <- err
		}(id)
	}
	close(start)
	wait.Wait()
	close(results)

	won, taken := 0, 0
	for err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrUsernameTaken):
			taken++
		default:
			t.Fatalf("unexpected claim error: %v", err)
		}
	}
	if won != 1 || taken != 1 {
		t.Fatalf("claims: %d winners, %d taken; want exactly one of each", won, taken)
	}
}
