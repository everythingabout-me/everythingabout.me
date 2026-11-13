package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
)

// Each run creates its own database on the explicitly selected disposable service.
func tokenPostgresService(t *testing.T) (context.Context, *store.DB, *Service, string) {
	t.Helper()
	scratch := testpg.CreateDatabase(t, "eam_token_test_")
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
	var owner string
	if err = db.Pool().QueryRow(ctx, `insert into users(email,password_hash) values('token-policy@example.test','') returning id::text`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	return ctx, db, New(db, &config.Config{AppSecret: "isolated-token-policy-test"}), owner
}

func TestTokenPostgresExplicitGrantsAndPolicyVersion(t *testing.T) {
	ctx, db, service, owner := tokenPostgresService(t)
	plain, token, err := service.CreateToken(ctx, NewTokenInput{UserID: owner, Name: "old shape", CanRead: true, CanWrite: true, CanEvents: true})
	if err != nil {
		t.Fatal(err)
	}
	if token.CanChanges || token.CanSubscribe || token.PolicyVersion != 1 {
		t.Fatalf("old input acquired grants or wrong version: %+v", token)
	}
	verified, err := service.VerifyToken(ctx, plain)
	if err != nil || verified == nil || verified.CanChanges || verified.CanSubscribe || verified.PolicyVersion != 1 {
		t.Fatalf("old token verification: %+v %v", verified, err)
	}
	// An insert with the old column list must retain the migration's deny-by-default policy.
	const oldID = "123456789abc"
	if _, err = db.Pool().Exec(ctx, `insert into api_tokens(token_id,token_hash,user_id,name,can_read,can_write,can_events) values($1,$2,$3::uuid,'legacy row',true,true,true)`, oldID, hashSecret("legacy-secret"), owner); err != nil {
		t.Fatal(err)
	}
	legacy, err := service.VerifyToken(ctx, TokenPrefix+oldID+"_legacy-secret")
	if err != nil || legacy == nil || legacy.CanChanges || legacy.CanSubscribe || legacy.PolicyVersion != 1 {
		t.Fatalf("legacy row acquired new rights: %+v %v", legacy, err)
	}
	policy := UpdateTokenInput{Prefix: "Apps/photos", CanRead: true, CanWrite: true, CanEvents: true, CanChanges: true, CanSubscribe: true}
	check := func(want int64) {
		t.Helper()
		current, err := service.VerifyToken(ctx, plain)
		if err != nil || current == nil || current.PolicyVersion != want || current.CanChanges != policy.CanChanges || current.CanSubscribe != policy.CanSubscribe || current.Prefix != policy.Prefix {
			t.Fatalf("verified policy version %d: %+v %v", want, current, err)
		}
	}
	updated, err := service.UpdateToken(ctx, owner, token.ID, policy)
	if err != nil || updated == nil || updated.PolicyVersion != 2 || !updated.CanChanges || !updated.CanSubscribe {
		t.Fatalf("explicit update: %+v %v", updated, err)
	}
	check(2)
	renamed, err := service.SetTokenName(ctx, owner, token.ID, "Photos app")
	if err != nil || renamed == nil || renamed.PolicyVersion != 2 || !renamed.CanChanges || !renamed.CanSubscribe {
		t.Fatalf("name changed policy: %+v %v", renamed, err)
	}
	same, err := service.UpdateToken(ctx, owner, token.ID, policy)
	if err != nil || same == nil || same.PolicyVersion != 2 {
		t.Fatalf("identical policy advanced version: %+v %v", same, err)
	}
	policy.Prefix = ""
	if _, err = service.UpdateToken(ctx, owner, token.ID, policy); err != nil {
		t.Fatal(err)
	}
	check(3)
	policy.Prefix = "Apps/photos"
	if _, err = service.UpdateToken(ctx, owner, token.ID, policy); err != nil {
		t.Fatal(err)
	}
	check(4) // Returning to an earlier policy must never revive its old cursors.
	policy.CanSubscribe = false
	if _, err = service.UpdateToken(ctx, owner, token.ID, policy); err != nil {
		t.Fatal(err)
	}
	check(5)
	policy.CanChanges = false
	if _, err = service.UpdateToken(ctx, owner, token.ID, policy); err != nil {
		t.Fatal(err)
	}
	check(6)
	expiry := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	policy.Expiry = &expiry
	if _, err = service.UpdateToken(ctx, owner, token.ID, policy); err != nil {
		t.Fatal(err)
	}
	check(7)
	if _, err = service.UpdateToken(ctx, owner, token.ID, policy); err != nil {
		t.Fatal(err)
	}
	check(7)
	policy.CanRead = false
	policy.CanEvents = false
	if _, err = service.UpdateToken(ctx, owner, token.ID, policy); err != nil {
		t.Fatal(err)
	}
	check(8)
	policy.CanWrite = false
	policy.CanEvents = true
	if _, err = service.UpdateToken(ctx, owner, token.ID, policy); err != nil {
		t.Fatal(err)
	}
	check(9)
	if err = service.RevokeToken(ctx, owner, token.ID); err != nil {
		t.Fatal(err)
	}
	revoked, err := service.VerifyToken(ctx, plain)
	if err != nil || revoked != nil {
		t.Fatalf("revoked token verified: %+v %v", revoked, err)
	}
	list, err := service.ListTokens(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range list {
		if row.ID == token.ID {
			found = true
			if row.PolicyVersion != 10 || !row.Revoked || row.Name != "Photos app" || row.CanChanges || row.CanSubscribe {
				t.Fatalf("audited revoked policy: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("revoked token missing from list")
	}
	_, granted, err := service.CreateToken(ctx, NewTokenInput{UserID: owner, CanRead: true, CanChanges: true, CanSubscribe: true})
	if err != nil || granted == nil || !granted.CanChanges || !granted.CanSubscribe || granted.PolicyVersion != 1 {
		t.Fatalf("explicit create: %+v %v", granted, err)
	}
	list, err = service.ListTokens(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, row := range list {
		if row.ID == granted.ID {
			found = row.CanChanges && row.CanSubscribe && row.PolicyVersion == 1
		}
	}
	if !found {
		t.Fatal("list lost explicit grants")
	}
	link, err := service.CreateSaveLink(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := service.VerifySaveLink(ctx, link.Code)
	if err != nil || principal == nil || principal.Token == nil || principal.Token.CanChanges || principal.Token.CanSubscribe || principal.FullAccess() {
		t.Fatalf("save link acquired new grants: %+v %v", principal, err)
	}
}
