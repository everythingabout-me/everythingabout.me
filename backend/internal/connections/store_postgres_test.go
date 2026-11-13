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

// A caller that has to place a connection's on-disk state before the INSERT generates
// the row id, so the database must keep it instead of generating a second one; adoption
// then rewrites the row to the paths it actually moved the directories to.
func TestPostgresInsertKeepsTheCallersIDAndRecordsAdoptedPaths(t *testing.T) {
	db := connectionTestDB(t)
	ctx := context.Background()
	st := NewPostgresStore(db)
	cfg := testCOSConfig("AKIDseed", "seed-secret", "data-125", "ap-hongkong")
	row := legacyRow(t, cfg, "0f0e9d5a-3f2b-4b1a-9f8e-2d7c6a5b4c3d")
	row.Name = "nas"
	row.Config.ObjectAdmissionFile = "/srv/eam/state/admission-nas.json"
	row.Config.ObjectSpoolRoot = "/srv/eam/object-spool/nas"
	if err := st.Insert(ctx, row); err != nil {
		t.Fatalf("insert with a caller-supplied id: %v", err)
	}
	got, ok, err := st.ByName(ctx, "", "nas")
	if err != nil || !ok {
		t.Fatalf("read the row: ok=%t err=%v", ok, err)
	}
	if got.ID != row.ID {
		t.Fatalf("row id = %q, want the id the caller placed the state under (%q)", got.ID, row.ID)
	}
	admission, spool := "/srv/eam/state/admission-"+got.ID+".json", "/srv/eam/object-spool/"+got.ID
	if err := st.SetObjectPaths(ctx, got.ID, admission, spool); err != nil {
		t.Fatalf("record adopted paths: %v", err)
	}
	updated, _, err := st.ByName(ctx, "", "nas")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Config.ObjectAdmissionFile != admission || updated.Config.ObjectSpoolRoot != spool {
		t.Fatalf("stored paths = %q, %q; want %q, %q", updated.Config.ObjectAdmissionFile, updated.Config.ObjectSpoolRoot, admission, spool)
	}
	if err := st.SetObjectPaths(ctx, "11111111-1111-4111-8111-111111111111", admission, spool); err == nil {
		t.Fatal("SetObjectPaths accepted an id that names no row")
	}
}

// 036 lets two owners each call a connection "nas". Both rows store, and the resolver
// reads, disjoint id-keyed directories; a unique index on the identity still keeps the
// two physical trees distinct.
func TestPostgresSameNameDifferentOwnersKeepDisjointObjectPaths(t *testing.T) {
	db := connectionTestDB(t)
	ctx := context.Background()
	st := NewPostgresStore(db)
	deployment := adoptionDeployment(t)
	owners := make([]string, 2)
	for i, name := range []string{"first", "second"} {
		if err := db.Pool().QueryRow(ctx, `insert into users(email, password_hash) values($1,'test-only') returning id::text`,
			name+"-same-name@example.test").Scan(&owners[i]); err != nil {
			t.Fatal(err)
		}
	}
	rows := make([]Connection, 2)
	paths := make([][2]string, 2)
	for i := range rows {
		c := legacyRow(t, testCOSConfig("id-"+string(rune('a'+i)), "key", "bucket-"+string(rune('a'+i))+"-125", "ap-hongkong"), "")
		c.OwnerID, c.Name = owners[i], "nas"
		c.ID = []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"}[i]
		c.Identity = mustIdentity(t, c)
		c.SecretReference = SecretReference{}
		c.Secret = Secret{COS: COSCredential{SecretID: "id", SecretKey: "key"}}
		c.Config.ObjectAdmissionFile, c.Config.ObjectSpoolRoot, _ = ObjectPaths(deployment, c, "", "")
		paths[i] = [2]string{c.Config.ObjectAdmissionFile, c.Config.ObjectSpoolRoot}
		rows[i] = c
		if err := st.Insert(ctx, c); err != nil {
			t.Fatalf("insert owner %d's row: %v", i, err)
		}
	}
	if paths[0][1] == paths[1][1] || paths[0][0] == paths[1][0] {
		t.Fatalf("two owners still share on-disk paths: %q/%q and %q/%q", paths[0][0], paths[0][1], paths[1][0], paths[1][1])
	}
	for i, owner := range owners {
		got, ok, err := st.ByName(ctx, owner, "nas")
		if err != nil || !ok {
			t.Fatalf("read owner %d's row: ok=%t err=%v", i, ok, err)
		}
		if got.Config.ObjectSpoolRoot != paths[i][1] || got.Config.ObjectAdmissionFile != paths[i][0] {
			t.Fatalf("owner %d's stored paths = %q, %q; want %q, %q", i,
				got.Config.ObjectAdmissionFile, got.Config.ObjectSpoolRoot, paths[i][0], paths[i][1])
		}
	}
}
