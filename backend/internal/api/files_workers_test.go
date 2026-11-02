package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

type fileWorkerFixture struct {
	ctx     context.Context
	db      *store.DB
	cfg     *config.Config
	backend *storagebackend.Backend
	journal *fileops.Postgres
	auth    *auth.Service
}

func newFileWorkerFixture(t *testing.T) *fileWorkerFixture {
	t.Helper()
	address := os.Getenv("EAM_TEST_DATABASE_URL")
	if address == "" {
		t.Skip("EAM_TEST_DATABASE_URL unset; disposable P4 worker/auth integration not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	var entropy [8]byte
	if _, err = rand.Read(entropy[:]); err != nil {
		t.Fatal(err)
	}
	name := "eam_workers_test_" + hex.EncodeToString(entropy[:])
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "create database "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "drop database "+quoted+" with (force)"); err != nil {
			t.Error(err)
		}
	})
	conn, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	conn.Path = "/" + name
	db, err := store.Open(ctx, conn.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	var actual string
	if err = db.Pool().QueryRow(ctx, `select current_database()`).Scan(&actual); err != nil || actual != name {
		t.Fatalf("test database not isolated: %q want %q: %v", actual, name, err)
	}
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) < 20 {
		t.Fatalf("P4 migrations unavailable: %d %v", len(migrations), err)
	}
	for pass := 0; pass < 2; pass++ {
		for _, path := range migrations {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Pool().Exec(ctx, string(data)); err != nil {
				t.Fatalf("migration %s pass%d: %v", filepath.Base(path), pass+1, err)
			}
		}
	}
	cfg := &config.Config{AppSecret: "independent-worker-test-secret", SessionTTLDays: 1, Storage: config.Storage{Backend: "local", ConnectionID: "worker-test", Generation: 1, CoordinatedWrites: true, MaxSinglePutBytes: 4096, Local: config.Local{ContentRoot: t.TempDir(), SystemRoot: t.TempDir()}}}
	backend, err := storagebackend.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	return &fileWorkerFixture{ctx: ctx, db: db, cfg: cfg, backend: backend, journal: fileops.NewPostgres(db.Pool()), auth: auth.New(db, cfg)}
}
func (f *fileWorkerFixture) owner(t *testing.T) (fileops.Root, fileops.NativeActions) {
	t.Helper()
	var owner string
	if err := f.db.Pool().QueryRow(f.ctx, `insert into users(email,password_hash) values($1,'') returning id::text`, fmt.Sprintf("worker-%d@example.test", time.Now().UnixNano())).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{f.cfg.Storage.Local.ContentRoot, f.cfg.Storage.Local.SystemRoot} {
		if err := os.Mkdir(filepath.Join(base, owner), 0700); err != nil {
			t.Fatal(err)
		}
	}
	driver, err := f.backend.Open(f.ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Close() })
	native := driver.(fileops.NativeActions)
	binding := native.StorageBinding()
	root := fileops.Root{OwnerID: owner, ConnectionID: binding.ConnectionID, Generation: binding.Generation, ContentRoot: binding.ContentRoot, SystemRoot: binding.SystemRoot, CaseSensitive: native.StorageNameRules().CaseSensitive}
	if err = f.journal.Initialize(f.ctx, root, fileops.Baseline{LogicalLimit: 1 << 20, PhysicalLimit: 2 << 20, NativeComplete: true, IndexCompared: true, LegacyWritersDrained: true, Evidence: json.RawMessage(`{"test":"fresh empty native root, no legacy writers"}`)}); err != nil {
		t.Fatal(err)
	}
	return root, native
}
func (f *fileWorkerFixture) token(t *testing.T, owner, scope string, read, write bool) (string, *auth.Token) {
	t.Helper()
	plain, token, err := f.auth.CreateToken(f.ctx, auth.NewTokenInput{UserID: owner, Name: "durable-worker", Kind: auth.KindApp, Prefix: scope, CanRead: read, CanWrite: write})
	if err != nil {
		t.Fatal(err)
	}
	return plain, token
}
func workerActor(root fileops.Root) fileops.Actor {
	return fileops.Actor{ID: "owner:" + root.OwnerID, CanRead: true, CanWrite: true, ManageOwner: true}
}
func (f *fileWorkerFixture) server() *Server {
	s := New(f.cfg, f.db, f.backend.Legacy(), nil)
	s.SetStorageFactory(f.backend)
	return s
}
func (f *fileWorkerFixture) admittedPut(t *testing.T, root fileops.Root, native fileops.NativeActions, actor fileops.Actor, target string, data []byte) (fileops.Operation, storage.WriteIntent) {
	t.Helper()
	parent := storage.Path("")
	if index := strings.LastIndexByte(target, '/'); index >= 0 {
		parent = storage.Path(target[:index])
	}
	entry, err := native.Stat(f.ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	intent := storage.WriteIntent{Target: storage.Path(target), Conditions: storage.Conditions{Absent: true}, Size: int64(len(data)), MediaType: "application/octet-stream", ParentPath: parent, ParentVersion: entry.Version, ClientCondition: "create:default"}
	encoded, _ := json.Marshal(intent)
	op, _, err := f.journal.Begin(f.ctx, fileops.BeginRequest{Root: root, Actor: actor, Kind: "upload_commit", Intent: encoded, Guards: []fileops.Guard{{Path: actor.Scope, Subtree: true}}, Reservation: fileops.ReservationRequest{NewBytes: int64(len(data)), PhysicalBytes: int64(len(data))}})
	if err != nil {
		t.Fatal(err)
	}
	return op, intent
}
func (f *fileWorkerFixture) stage(t *testing.T, op fileops.Operation, intent storage.WriteIntent, native fileops.NativeActions, data []byte) (fileops.Operation, storage.Staged) {
	t.Helper()
	claimed, err := f.journal.Claim(f.ctx, op.Root, workerActor(op.Root), op.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	intent.ID = op.ID
	stage, err := native.Stage(f.ctx, intent, bytes.NewReader(data), func(ctx context.Context, checkpoint []byte) error {
		return f.journal.SaveCheckpoint(ctx, claimed.Lease, checkpoint)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.journal.SaveStage(f.ctx, claimed.Lease, stage); err != nil {
		t.Fatal(err)
	}
	if err = f.journal.Release(f.ctx, claimed.Lease); err != nil {
		t.Fatal(err)
	}
	saved, err := f.journal.Get(f.ctx, op.Root, workerActor(op.Root), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return saved, stage
}
func (f *fileWorkerFixture) checkNoLiability(t *testing.T, root fileops.Root) {
	t.Helper()
	usage, err := f.journal.Usage(f.ctx, root, workerActor(root))
	if err != nil {
		t.Fatal(err)
	}
	var guards int
	if err = f.db.Pool().QueryRow(f.ctx, `select count(*) from file_operation_guards where owner_id=$1::uuid`, root.OwnerID).Scan(&guards); err != nil {
		t.Fatal(err)
	}
	if usage.LogicalReserved != 0 || usage.PhysicalReserved != 0 || guards != 0 {
		t.Fatalf("unused admission retained: %+v guards%d", usage, guards)
	}
}

func TestFilesWorkersPostgresRestartAndPublicationAuthorization(t *testing.T) {
	f := newFileWorkerFixture(t)
	t.Run("stage survives server restart without stored bearer secret", func(t *testing.T) {
		root, native := f.owner(t)
		plain, token := f.token(t, root.OwnerID, "", true, true)
		actor := fileops.Actor{ID: "token:" + token.ID, CanRead: true, CanWrite: true}
		data := []byte("durable native staging\x00\xff")
		op, intent := f.admittedPut(t, root, native, actor, "restart.bin", data)
		op, _ = f.stage(t, op, intent, native, data)
		var snapshot []byte
		if err := f.db.Pool().QueryRow(f.ctx, `select snapshot from file_operations where id=$1`, op.ID).Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(snapshot, []byte(plain)) || bytes.Contains(snapshot, []byte(strings.SplitN(plain, "_", 3)[2])) {
			t.Fatal("queued operation persisted a raw credential")
		}
		if err := f.server().runFileOperation(f.ctx, op); err != nil {
			t.Fatal(err)
		}
		result, err := f.journal.Get(f.ctx, root, workerActor(root), op.ID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(root.ContentRoot, "restart.bin"))
		if err != nil || !bytes.Equal(got, data) || result.State != fileops.Succeeded || result.CleanedAt == nil {
			t.Fatalf("restart settlement %+v bytes%q err%v", result, got, err)
		}
		f.checkNoLiability(t, root)
	})
	t.Run("changed staged parent is never retargeted", func(t *testing.T) {
		root, native := f.owner(t)
		parent := filepath.Join(root.ContentRoot, "Parent")
		if err := os.Mkdir(parent, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Pool().Exec(f.ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir) values($1::uuid,$2,'Parent','','Parent',true)`, root.OwnerID, "drive/"+root.OwnerID+"/Parent/"); err != nil {
			t.Fatal(err)
		}
		op, intent := f.admittedPut(t, root, native, workerActor(root), "Parent/file", []byte("old-parent"))
		op, _ = f.stage(t, op, intent, native, []byte("old-parent"))
		moved := filepath.Join(root.ContentRoot, "Moved")
		if err := os.Rename(parent, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(parent, 0700); err != nil {
			t.Fatal(err)
		}
		_ = f.server().runFileOperation(f.ctx, op)
		result, err := f.journal.Get(f.ctx, root, workerActor(root), op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.State != fileops.Failed && result.State != fileops.Cancelled {
			t.Fatalf("changed parent never retired unused stage: state%s phase%s", result.State, result.Phase)
		}
		for _, dir := range []string{parent, moved} {
			if _, err := os.Stat(filepath.Join(dir, "file")); !os.IsNotExist(err) {
				t.Fatalf("publication retargeted %s: %v", dir, err)
			}
		}
		f.checkNoLiability(t, root)
	})
	t.Run("revoked staged creator retires unused native bytes", func(t *testing.T) {
		root, native := f.owner(t)
		_, token := f.token(t, root.OwnerID, "", true, true)
		op, intent := f.admittedPut(t, root, native, fileops.Actor{ID: "token:" + token.ID, CanRead: true, CanWrite: true}, "revoked-staged", []byte("never published"))
		op, _ = f.stage(t, op, intent, native, []byte("never published"))
		if _, err := f.db.Pool().Exec(f.ctx, `update api_tokens set revoked_at=now() where user_id=$1::uuid and token_id=$2`, root.OwnerID, token.ID); err != nil {
			t.Fatal(err)
		}
		_ = f.server().runFileOperation(f.ctx, op)
		result, err := f.journal.Get(f.ctx, root, workerActor(root), op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.State != fileops.Failed && result.State != fileops.Cancelled {
			t.Fatalf("revoked stage never retired: %s/%s", result.State, result.Phase)
		}
		if _, err = os.Stat(filepath.Join(root.ContentRoot, "revoked-staged")); !os.IsNotExist(err) {
			t.Fatal("revoked stage published", err)
		}
		f.checkNoLiability(t, root)
	})
	t.Run("revoked token blocks queued native action", func(t *testing.T) {
		root, native := f.owner(t)
		_, token := f.token(t, root.OwnerID, "", true, true)
		actions, err := fileops.NewActions(f.journal, fileops.ActionsOptions{Authorize: f.auth.AuthorizeFileOperation, Validate: func(context.Context, fileops.NativeActions, fileops.ActionRequest) error { return nil }, Commit: nodes.CommitFileAction})
		if err != nil {
			t.Fatal(err)
		}
		op, _, err := actions.Prepare(f.ctx, fileops.ActionRequest{Root: root, Actor: fileops.Actor{ID: "token:" + token.ID, CanRead: true, CanWrite: true}, Kind: "mkdir", To: "revoked-folder", IdempotencyKey: "revoked-queued"}, native)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.db.Pool().Exec(f.ctx, `update api_tokens set revoked_at=now() where user_id=$1::uuid and token_id=$2`, root.OwnerID, token.ID); err != nil {
			t.Fatal(err)
		}
		_ = f.server().runFileOperation(f.ctx, op)
		result, err := f.journal.Get(f.ctx, root, workerActor(root), op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.State != fileops.Failed && result.State != fileops.Cancelled {
			t.Fatalf("revoked queued action state%s phase%s", result.State, result.Phase)
		}
		if _, err = os.Stat(filepath.Join(root.ContentRoot, "revoked-folder")); !os.IsNotExist(err) {
			t.Fatal("revoked creator published a folder", err)
		}
		f.checkNoLiability(t, root)
	})
	t.Run("frozen queued action resumes on a new server", func(t *testing.T) {
		root, native := f.owner(t)
		actions, err := fileops.NewActions(f.journal, fileops.ActionsOptions{Authorize: f.auth.AuthorizeFileOperation, Validate: func(context.Context, fileops.NativeActions, fileops.ActionRequest) error { return nil }, Commit: nodes.CommitFileAction})
		if err != nil {
			t.Fatal(err)
		}
		op, _, err := actions.Prepare(f.ctx, fileops.ActionRequest{Root: root, Actor: workerActor(root), Kind: "mkdir", To: "restart-folder", IdempotencyKey: "queued-manifest"}, native)
		if err != nil {
			t.Fatal(err)
		}
		server := f.server()
		if err = server.runFileOperation(f.ctx, op); err != nil {
			t.Fatal(err)
		}
		if err = server.runFileOperation(f.ctx, op); err != nil {
			t.Fatal("action worker replay", err)
		}
		saved, err := f.journal.Get(f.ctx, root, workerActor(root), op.ID)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(root.ContentRoot, "restart-folder"))
		if err != nil || !info.IsDir() || saved.State != fileops.Succeeded {
			t.Fatalf("queued action not applied: %s %v", saved.State, err)
		}
		f.checkNoLiability(t, root)
	})
	t.Run("known native application settles after creator revocation", func(t *testing.T) {
		root, native := f.owner(t)
		_, token := f.token(t, root.OwnerID, "", true, true)
		op, intent := f.admittedPut(t, root, native, fileops.Actor{ID: "token:" + token.ID, CanRead: true, CanWrite: true}, "applied.bin", []byte("applied"))
		op, stage := f.stage(t, op, intent, native, []byte("applied"))
		claimed, err := f.journal.Claim(f.ctx, root, workerActor(root), op.ID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err = f.journal.StartEffect(f.ctx, claimed.Lease); err != nil {
			t.Fatal(err)
		}
		result, err := native.Publish(f.ctx, stage, func(ctx context.Context, checkpoint []byte) error {
			return f.journal.SaveCheckpoint(ctx, claimed.Lease, checkpoint)
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = f.journal.SaveApplied(f.ctx, claimed.Lease, result); err != nil {
			t.Fatal(err)
		}
		if err = f.journal.Release(f.ctx, claimed.Lease); err != nil {
			t.Fatal(err)
		}
		if _, err = f.db.Pool().Exec(f.ctx, `update api_tokens set revoked_at=now() where user_id=$1::uuid and token_id=$2`, root.OwnerID, token.ID); err != nil {
			t.Fatal(err)
		}
		op, err = f.journal.Get(f.ctx, root, workerActor(root), op.ID)
		if err != nil {
			t.Fatal(err)
		}
		server := f.server()
		if err = server.runFileOperation(f.ctx, op); err != nil {
			t.Fatal(err)
		}
		if err = server.runFileOperation(f.ctx, op); err != nil {
			t.Fatal("idempotent worker replay", err)
		}
		saved, err := f.journal.Get(f.ctx, root, workerActor(root), op.ID)
		if err != nil {
			t.Fatal(err)
		}
		usage, err := f.journal.Usage(f.ctx, root, workerActor(root))
		if err != nil {
			t.Fatal(err)
		}
		var indexed int64
		if err = f.db.Pool().QueryRow(f.ctx, `select size from nodes where owner_id=$1::uuid and path='applied.bin'`, root.OwnerID).Scan(&indexed); err != nil {
			t.Fatal(err)
		}
		if saved.State != fileops.Succeeded || saved.CleanedAt == nil || usage.LogicalBytes != 7 || indexed != 7 {
			t.Fatalf("applied result uncharged/unindexed %+v %+v", saved, usage)
		}
		f.checkNoLiability(t, root)
	})
}

func TestFilesWorkersPostgresPendingAndCreatorRights(t *testing.T) {
	f := newFileWorkerFixture(t)
	root, native := f.owner(t)
	actor := workerActor(root)
	t.Run("pending scanner avoids fresh ingress and active leases", func(t *testing.T) {
		fresh, _ := f.admittedPut(t, root, native, actor, "fresh", nil)
		pending, err := f.journal.Pending(f.ctx, 32)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 0 {
			t.Fatal("fresh ingress became worker input", pending)
		}
		if _, err = f.db.Pool().Exec(f.ctx, `update file_operations set snapshot=jsonb_set(snapshot,'{CreatedAt}',to_jsonb($2::text)) where id=$1`, fresh.ID, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		lease, err := f.journal.Claim(f.ctx, root, actor, fresh.ID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		pending, err = f.journal.Pending(f.ctx, 32)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 0 {
			t.Fatal("active lease selected by worker", pending)
		}
		if err = f.journal.Release(f.ctx, lease.Lease); err != nil {
			t.Fatal(err)
		}
		pending, err = f.journal.Pending(f.ctx, 32)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 1 || pending[0].ID != fresh.ID {
			t.Fatal("expired owner claim not discoverable", pending)
		}
	})
	t.Run("creator rights use durable principal and current policy", func(t *testing.T) {
		scope := "Photos"
		_, readWrite := f.token(t, root.OwnerID, scope, true, true)
		_, writeOnly := f.token(t, root.OwnerID, scope, false, true)
		_, readOnly := f.token(t, root.OwnerID, scope, true, false)
		op := fileops.Operation{Root: root, Kind: "copy", Guards: []fileops.Guard{{Path: "Photos/a"}, {Path: "Photos/b"}}}
		for _, tc := range []struct {
			name, id, kind string
			guards         []fileops.Guard
			ok             bool
		}{
			{"owner", actor.ID, "copy", op.Guards, true},
			{"read write", "token:" + readWrite.ID, "copy", op.Guards, true},
			{"write only copy", "token:" + writeOnly.ID, "copy", op.Guards, false},
			{"write only upload", "token:" + writeOnly.ID, "upload_commit", op.Guards, true},
			{"read only", "token:" + readOnly.ID, "upload_commit", op.Guards, false},
			{"prefix sibling", "token:" + readWrite.ID, "copy", []fileops.Guard{{Path: "Photos-old/a"}}, false},
		} {
			op.PrincipalID, op.Kind, op.Guards = tc.id, tc.kind, tc.guards
			err := f.auth.AuthorizeFileOperation(f.ctx, op)
			if (err == nil) != tc.ok {
				t.Fatalf("%s: %v", tc.name, err)
			}
		}
		link, err := f.auth.CreateSaveLink(f.ctx, root.OwnerID)
		if err != nil {
			t.Fatal(err)
		}
		principal, err := f.auth.VerifySaveLink(f.ctx, link.Code)
		if err != nil || principal == nil {
			t.Fatal(err)
		}
		op.PrincipalID = "token:" + principal.Token.ID
		op.Kind = "mkdir"
		op.Guards = []fileops.Guard{{Path: "anywhere"}}
		if err = f.auth.AuthorizeFileOperation(f.ctx, op); err != nil {
			t.Fatal(err)
		}
		if err = f.auth.RevokeSaveLink(f.ctx, root.OwnerID, link.ID); err != nil {
			t.Fatal(err)
		}
		if err = f.auth.AuthorizeFileOperation(f.ctx, op); !fileops.IsCode(err, "forbidden") {
			t.Fatal("revoked save link remained authorized", err)
		}
		op.PrincipalID = "token:" + readWrite.ID
		op.Guards = []fileops.Guard{{Path: "Photos/a"}}
		if _, err = f.db.Pool().Exec(f.ctx, `update api_tokens set revoked_at=now() where user_id=$1::uuid and token_id=$2`, root.OwnerID, readWrite.ID); err != nil {
			t.Fatal(err)
		}
		if err = f.auth.AuthorizeFileOperation(f.ctx, op); !fileops.IsCode(err, "forbidden") {
			t.Fatal("revoked queued token remained authorized", err)
		}
	})
}
