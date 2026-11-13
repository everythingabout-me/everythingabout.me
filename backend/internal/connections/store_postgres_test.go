package connections

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
)

// connectionTestDB creates a disposable database, applies every migration in
// filename order, and returns a store over it. The suite never loads application
// configuration or .env.
func connectionTestDB(t *testing.T) *store.DB {
	t.Helper()
	scratch := testpg.CreateDatabase(t, "eam_connections_test_")
	ctx := context.Background()
	db, err := store.Open(ctx, scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatal("migration discovery", err)
	}
	for _, path := range migrations {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool().Exec(ctx, string(data)); err != nil {
			t.Fatalf("migration %s: %v", filepath.Base(path), err)
		}
	}
	return db
}

// The conversion must write the reference to the row and empty the inline copy;
// migration 045's constraint would refuse a row that kept both.
func TestPostgresAdoptEnvironmentSecretRewritesTheRow(t *testing.T) {
	db := connectionTestDB(t)
	ctx := context.Background()
	st := NewPostgresStore(db)
	cfg := testCOSConfig("AKIDseed", "seed-secret", "data-125", "ap-hongkong")
	row := legacyRow(t, cfg, "")
	if err := st.Insert(ctx, row); err != nil {
		t.Fatalf("insert the legacy row: %v", err)
	}
	lookup := envFor(map[string]string{EnvCOSSecretID: "AKIDseed", EnvCOSSecretKey: "seed-secret"})
	if err := AdoptEnvironmentSecret(ctx, st, cfg, lookup); err != nil {
		t.Fatalf("convert the row: %v", err)
	}
	got, ok, err := st.ByName(ctx, "", "default")
	if err != nil || !ok {
		t.Fatalf("read the converted row: ok=%t err=%v", ok, err)
	}
	if !got.Secret.Empty() {
		t.Fatalf("the converted row still holds an inline secret: %+v", got.Secret)
	}
	if names := got.SecretReference.Names(); len(names) != 2 || names[0] != EnvCOSSecretID || names[1] != EnvCOSSecretKey {
		t.Fatalf("stored reference names %v", names)
	}
	if secret, err := got.ResolveSecret(lookup); err != nil || secret.COS.SecretKey != "seed-secret" {
		t.Fatalf("the converted row does not resolve: %v", err)
	}
	var storedSecret []byte
	if err := db.Pool().QueryRow(ctx, `select secret from storage_connections where id=$1::uuid`, got.ID).Scan(&storedSecret); err != nil {
		t.Fatal(err)
	}
	if string(storedSecret) != "{}" {
		t.Fatalf("stored secret is %s, want the empty object", storedSecret)
	}
	// A second start verifies, and does not rewrite, an already-converted row.
	if err := AdoptEnvironmentSecret(ctx, st, cfg, lookup); err != nil {
		t.Fatalf("verify the converted row: %v", err)
	}
}

func TestPostgresAdoptRefusesAMismatchAndLeavesTheRowAlone(t *testing.T) {
	db := connectionTestDB(t)
	ctx := context.Background()
	st := NewPostgresStore(db)
	rowCfg := testCOSConfig("row-id", "row-key", "row-bucket-125", "ap-hongkong")
	row := legacyRow(t, rowCfg, "")
	if err := st.Insert(ctx, row); err != nil {
		t.Fatalf("insert the legacy row: %v", err)
	}
	envCfg := testCOSConfig("env-id", "env-key", "env-bucket-125", "ap-beijing")
	err := AdoptEnvironmentSecret(ctx, st, envCfg, envFor(map[string]string{EnvCOSSecretID: "env-id", EnvCOSSecretKey: "env-key"}))
	if err == nil {
		t.Fatal("a mismatched environment converted the row")
	}
	got, ok, readErr := st.ByName(ctx, "", "default")
	if readErr != nil || !ok {
		t.Fatalf("read the refused row: ok=%t err=%v", ok, readErr)
	}
	if got.Secret.COS.SecretKey != "row-key" || !got.SecretReference.Empty() {
		t.Fatal("a refused conversion changed the row")
	}
}

// SetSecretReference is scoped to the deployment default. An account's own row
// keeps its inline secret even when the same command is aimed at it.
func TestPostgresSetSecretReferenceLeavesAccountRowsAlone(t *testing.T) {
	db := connectionTestDB(t)
	ctx := context.Background()
	var ownerID string
	if err := db.Pool().QueryRow(ctx, `insert into users(email, password_hash) values('owner@example.com','x') returning id::text`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	st := NewPostgresStore(db)
	cfg := testCOSConfig("AKIDseed", "seed-secret", "data-125", "ap-hongkong")
	shared := legacyRow(t, cfg, "")
	if err := st.Insert(ctx, shared); err != nil {
		t.Fatal(err)
	}
	ownerRow := shared
	ownerRow.ID, ownerRow.OwnerID, ownerRow.Name = "", ownerID, "mine"
	ownerRow.Config.COS.Bucket = "owner-bucket-125"
	ownerRow.Identity = mustIdentity(t, ownerRow)
	ownerRow.SecretReference = SecretReference{}
	ownerRow.Secret = Secret{COS: COSCredential{SecretID: "owner-id", SecretKey: "owner-key"}}
	if err := st.Insert(ctx, ownerRow); err != nil {
		t.Fatalf("insert the account row: %v", err)
	}
	got, ok, err := st.ByName(ctx, ownerID, "mine")
	if err != nil || !ok {
		t.Fatalf("read the account row: ok=%t err=%v", ok, err)
	}
	reference := EnvironmentReference(cfg)
	if err := st.SetSecretReference(ctx, got.ID, reference); err == nil {
		t.Fatal("SetSecretReference rewrote an account-owned row")
	}
	unchanged, _, err := st.ByName(ctx, ownerID, "mine")
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Secret.COS.SecretKey != "owner-key" || !unchanged.SecretReference.Empty() {
		t.Fatal("the account row's inline secret changed")
	}
}

// Migration 045 must survive a re-run (files from 044 on are re-executed when the
// version table is lost) and its check must reject a row that keeps both a
// reference and an inline credential.
func TestStorageConnectionSecretReferenceMigrationIsRerunnableAndEnforcesOneSource(t *testing.T) {
	db := connectionTestDB(t)
	ctx := context.Background()
	data, err := os.ReadFile("../../../sql/045_storage_connection_secret_reference.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.Pool().Exec(ctx, string(data)); err != nil {
			t.Fatalf("re-run migration 045 (attempt %d): %v", i+1, err)
		}
	}
	_, err = db.Pool().Exec(ctx, `insert into storage_connections(name, backend, role, identity, config, secret, secret_reference)
		values ('both', 'cos', 'authority', 'both-identity', '{}'::jsonb,
			'{"cos":{"secretId":"inline-id","secretKey":"inline-key"}}'::jsonb,
			'{"cos":{"secretId":"COS_SECRET_ID","secretKey":"COS_SECRET_KEY"}}'::jsonb)`)
	if err == nil {
		t.Fatal("a row with both an inline secret and an environment reference was accepted")
	}
}
