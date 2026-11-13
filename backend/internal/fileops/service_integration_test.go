package fileops_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/localstorage"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests opt in to a disposable CREATEDB-capable PostgreSQL service, never
// application configuration. Every run creates/drops its own random database and
// applies every migration twice. The parent owns the shared Docker test service.
func coordinatorDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	scratch := testpg.CreateDatabase(t, "eam_coordinator_")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 12
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) < 17 {
		t.Fatalf("migration discovery: count=%d err=%v", len(migrations), err)
	}
	for pass := 1; pass <= 2; pass++ {
		for _, path := range migrations {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, string(data)); err != nil {
				t.Fatalf("migration %s pass %d: %v", filepath.Base(path), pass, err)
			}
		}
	}
	t.Logf("isolated PostgreSQL database %s; %d migrations applied twice", scratch.Name, len(migrations))
	return pool
}
func coordinatorUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

type coordinatorFixture struct {
	pool            *pgxpool.Pool
	journal         *fileops.Postgres
	root            fileops.Root
	actor           fileops.Actor
	driver          storage.Driver
	writer          *localstorage.StagedWriter
	content, system string
}

func newCoordinatorFixture(t *testing.T, pool *pgxpool.Pool) *coordinatorFixture {
	t.Helper()
	ctx := context.Background()
	owner := coordinatorUUID(t)
	base := t.TempDir()
	contentBase := filepath.Join(base, "content")
	systemBase := filepath.Join(base, "system")
	content := filepath.Join(contentBase, owner)
	system := filepath.Join(systemBase, owner)
	if err := os.MkdirAll(content, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(system, 0700); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `insert into users(id,email,password_hash) values($1,$2,'test-only')`, owner, owner+"@example.test")
	if err != nil {
		t.Fatal(err)
	}
	factory, err := localstorage.NewFactory(localstorage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	binding := storage.Config{OwnerID: owner, ConnectionID: "coordinator-local-fixture", Generation: 1, ContentRoot: content, SystemRoot: system}
	driver, err := factory.OpenOwner(ctx, binding, contentBase, systemBase)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := driver.Close(); err != nil {
			t.Errorf("close local driver: %v", err)
		}
	})
	writer, err := localstorage.NewStagedWriter(driver, localstorage.WriteOptions{CoordinatedExclusiveWriter: true})
	if err != nil {
		t.Fatal(err)
	}
	actualBinding := writer.StorageBinding()
	root := fileops.Root{OwnerID: actualBinding.OwnerID, ConnectionID: actualBinding.ConnectionID, Generation: actualBinding.Generation, ContentRoot: actualBinding.ContentRoot, SystemRoot: actualBinding.SystemRoot, CaseSensitive: writer.StorageNameRules().CaseSensitive}
	actor := fileops.Actor{ID: "token:coordinator-fixture", CanRead: true, CanWrite: true}
	journal := fileops.NewPostgres(pool)
	baseline := fileops.Baseline{LogicalLimit: 1 << 20, PhysicalLimit: 4 << 20, NativeComplete: true, IndexCompared: true, LegacyWritersDrained: true, Evidence: json.RawMessage(`{"native_scan":"empty isolated owner root","legacy_writer_drain":"fixture has no legacy writers"}`)}
	if err = journal.Initialize(ctx, root, baseline); err != nil {
		t.Fatal(err)
	}
	return &coordinatorFixture{pool: pool, journal: journal, root: root, actor: actor, driver: driver, writer: writer, content: content, system: system}
}
func (f *coordinatorFixture) service(t *testing.T, authorize fileops.Authorize, commit fileops.IndexCommit) *fileops.Service {
	t.Helper()
	if authorize == nil {
		authorize = func(context.Context, fileops.Operation) error { return nil }
	}
	if commit == nil {
		commit = nodes.CommitStagedFile
	}
	service, err := fileops.NewService(f.journal, fileops.ServiceOptions{Authorize: authorize, Commit: commit, LeaseTTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return service
}
func (f *coordinatorFixture) begin(t *testing.T, path storage.Path, size int64, digest *storage.Digest) fileops.Operation {
	t.Helper()
	op, replayed, err := f.journal.Begin(context.Background(), f.request(path, size, digest))
	if err != nil || replayed {
		t.Fatalf("admit replay=%v: %v", replayed, err)
	}
	return op
}
func (f *coordinatorFixture) request(path storage.Path, size int64, digest *storage.Digest) fileops.BeginRequest {
	intent, _ := json.Marshal(storage.WriteIntent{Target: path, Conditions: storage.Conditions{Absent: true}, Size: size, MediaType: "application/octet-stream", Digest: digest})
	return fileops.BeginRequest{Root: f.root, Actor: f.actor, Kind: "upload_commit", IdempotencyKey: "write:" + string(path), Intent: intent, Guards: []fileops.Guard{{Path: path}}, Reservation: fileops.ReservationRequest{NewBytes: size, PhysicalBytes: size}}
}
func (f *coordinatorFixture) saved(t *testing.T, id string) fileops.Operation {
	t.Helper()
	op, err := fileops.NewPostgres(f.pool).Get(context.Background(), f.root, f.actor, id)
	if err != nil {
		t.Fatal(err)
	}
	return op
}
func (f *coordinatorFixture) usage(t *testing.T) fileops.Usage {
	t.Helper()
	usage, err := fileops.NewPostgres(f.pool).Usage(context.Background(), f.root, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	return usage
}
func (f *coordinatorFixture) counts(t *testing.T, id string) (int, int) {
	t.Helper()
	var guards, indexed int
	if err := f.pool.QueryRow(context.Background(), `select count(*) from file_operation_guards where owner_id=$1 and operation_id=$2`, f.root.OwnerID, id).Scan(&guards); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `select count(*) from nodes where owner_id=$1`, f.root.OwnerID).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	return guards, indexed
}
func coordinatorBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("native read %q: got=%q want=%q err=%v", filepath.Base(path), got, want, err)
	}
}
func coordinatorAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native path should be absent: %s %v", filepath.Base(path), err)
	}
}
func coordinatorState(t *testing.T, op fileops.Operation, state fileops.State, phase fileops.Phase) {
	t.Helper()
	if op.State != state || op.Phase != phase {
		t.Fatalf("operation state=%s phase=%s; want %s/%s", op.State, op.Phase, state, phase)
	}
}

type countedCoordinatorWriter struct {
	storage.StagedWriter
	stages, publishes     atomic.Int32
	inspections, discards atomic.Int32
	afterStage            func(context.Context, storage.Staged) error
	publish               func(context.Context, storage.Staged, storage.Checkpoint) (storage.MutationResult, error)
	inspect               func(context.Context, storage.WriteIntent, []byte) (storage.Inspection, error)
}

func (w *countedCoordinatorWriter) StorageBinding() storage.Config {
	return w.StagedWriter.(storage.BoundStagedWriter).StorageBinding()
}
func (w *countedCoordinatorWriter) StorageNameRules() storage.NameRules {
	return w.StagedWriter.(storage.BoundStagedWriter).StorageNameRules()
}
func (w *countedCoordinatorWriter) Discard(ctx context.Context, in storage.WriteIntent, evidence []byte) error {
	w.discards.Add(1)
	return w.StagedWriter.Discard(ctx, in, evidence)
}

func (w *countedCoordinatorWriter) Stage(ctx context.Context, in storage.WriteIntent, r io.Reader, cp storage.Checkpoint) (storage.Staged, error) {
	w.stages.Add(1)
	out, err := w.StagedWriter.Stage(ctx, in, r, cp)
	if err == nil && w.afterStage != nil {
		err = w.afterStage(ctx, out)
	}
	return out, err
}
func (w *countedCoordinatorWriter) Publish(ctx context.Context, s storage.Staged, cp storage.Checkpoint) (storage.MutationResult, error) {
	w.publishes.Add(1)
	if w.publish != nil {
		return w.publish(ctx, s, cp)
	}
	return w.StagedWriter.Publish(ctx, s, cp)
}
func (w *countedCoordinatorWriter) Inspect(ctx context.Context, in storage.WriteIntent, b []byte) (storage.Inspection, error) {
	w.inspections.Add(1)
	if w.inspect != nil {
		return w.inspect(ctx, in, b)
	}
	return w.StagedWriter.Inspect(ctx, in, b)
}

// Erasing the binding methods emulates an unsafe adapter boundary. Embedding only
// StagedWriter must not grant permission to publish using an arbitrary owner root.
type unboundCoordinatorWriter struct{ storage.StagedWriter }

func coordinatorSameJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	aa, _ := json.Marshal(x)
	bb, _ := json.Marshal(y)
	return bytes.Equal(aa, bb)
}

func TestServicePostgresLocalCoordinator(t *testing.T) {
	pool := coordinatorDatabase(t)
	ctx := context.Background()
	t.Run("mismatched and unbound native writers never cross owner roots", func(t *testing.T) {
		alice := newCoordinatorFixture(t, pool)
		bob := newCoordinatorFixture(t, pool)
		op := alice.begin(t, "isolated.bin", 4, nil)
		service := alice.service(t, nil, nil)
		bobWriter := &countedCoordinatorWriter{StagedWriter: bob.writer}
		for _, action := range []string{"write", "recover", "cleanup"} {
			t.Run(action, func(t *testing.T) {
				var err error
				switch action {
				case "write":
					body := strings.NewReader("data")
					_, err = service.Write(ctx, alice.root, alice.actor, op.ID, bobWriter, body)
					if body.Len() != 4 {
						t.Fatal("wrong-root writer consumed client body")
					}
				case "recover":
					_, err = service.Recover(ctx, alice.root, alice.actor, op.ID, bobWriter)
				case "cleanup":
					err = service.Cleanup(ctx, alice.root, alice.actor, op.ID, bobWriter)
				}
				if !fileops.IsCode(err, "root_conflict") {
					t.Fatalf("wrong-root %s not rejected: %v", action, err)
				}
			})
		}
		if bobWriter.stages.Load() != 0 || bobWriter.publishes.Load() != 0 || bobWriter.inspections.Load() != 0 || bobWriter.discards.Load() != 0 {
			t.Fatal("wrong owner reached native writer")
		}
		goodCounter := &countedCoordinatorWriter{StagedWriter: alice.writer}
		unbound := &unboundCoordinatorWriter{StagedWriter: goodCounter}
		if _, err := service.Write(ctx, alice.root, alice.actor, op.ID, unbound, strings.NewReader("data")); !fileops.IsCode(err, "unbound_writer") {
			t.Fatal("unbound writer admitted to Write", err)
		}
		if _, err := service.Recover(ctx, alice.root, alice.actor, op.ID, unbound); !fileops.IsCode(err, "unbound_writer") {
			t.Fatal("unbound writer admitted to Recover", err)
		}
		if err := service.Cleanup(ctx, alice.root, alice.actor, op.ID, unbound); !fileops.IsCode(err, "unbound_writer") {
			t.Fatal("unbound writer admitted to Cleanup", err)
		}
		if goodCounter.stages.Load() != 0 || goodCounter.publishes.Load() != 0 || goodCounter.inspections.Load() != 0 || goodCounter.discards.Load() != 0 {
			t.Fatal("unbound writer reached native I/O")
		}
		saved := alice.saved(t, op.ID)
		coordinatorState(t, saved, fileops.Queued, fileops.Admitted)
		guards, indexed := alice.counts(t, op.ID)
		if guards != 1 || indexed != 0 {
			t.Fatal("rejected writer changed journal/index")
		}
		u := alice.usage(t)
		if u.LogicalBytes != 0 || u.PhysicalBytes != 0 || u.LogicalReserved != 4 || u.PhysicalReserved != 4 {
			t.Fatal("rejected writer released Alice admission", u)
		}
		if bob.usage(t).LogicalReserved != 0 || bob.usage(t).PhysicalReserved != 0 {
			t.Fatal("Alice liability was charged to Bob")
		}
		coordinatorAbsent(t, filepath.Join(alice.content, "isolated.bin"))
		coordinatorAbsent(t, filepath.Join(bob.content, "isolated.bin"))
	})
	t.Run("native publication and durable terminal body replay", func(t *testing.T) {
		f := newCoordinatorFixture(t, pool)
		payload := []byte("original\x00bytes\xff\r\n")
		target := storage.Path(" accepted +?#%.bin ")
		sum := sha256.Sum256(payload)
		op := f.begin(t, target, int64(len(payload)), &storage.Digest{Algorithm: "sha-256", Value: sum[:]})
		service := f.service(t, nil, nil)
		writer := &countedCoordinatorWriter{StagedWriter: f.writer}
		result, err := service.Write(ctx, f.root, f.actor, op.ID, writer, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		coordinatorState(t, result, fileops.Succeeded, fileops.Settled)
		coordinatorBytes(t, filepath.Join(f.content, string(target)), payload)
		if result.Stage == nil || result.Stage.Digest == nil || result.Stage.Digest.Algorithm != "sha-256" || !bytes.Equal(result.Stage.Digest.Value, sum[:]) {
			t.Fatal("durable stage receipt does not match received bytes")
		}
		var wire map[string]any
		if err = json.Unmarshal(result.Result, &wire); err != nil {
			t.Fatal(err)
		}
		if wire["path"] != "/"+string(target) || wire["id"] == nil || wire["id"] == "" || wire["key"] != nil || wire["revision"] == "" {
			t.Fatalf("invalid public settlement result: %s", result.Result)
		}
		guards, indexed := f.counts(t, op.ID)
		if guards != 0 || indexed != 1 {
			t.Fatalf("guards=%d index=%d", guards, indexed)
		}
		usage := f.usage(t)
		if usage.LogicalBytes != int64(len(payload)) || usage.PhysicalBytes != int64(len(payload)) || usage.LogicalReserved != 0 || usage.PhysicalReserved != 0 {
			t.Fatalf("usage not settled exactly once: %+v", usage)
		}
		req := f.request(target, int64(len(payload)), &storage.Digest{Algorithm: "sha-256", Value: sum[:]})
		replayed, yes, err := fileops.NewPostgres(pool).Begin(ctx, req)
		if err != nil || !yes || replayed.ID != op.ID {
			t.Fatalf("lost admission replay: %v %v", yes, err)
		}
		fresh := f.service(t, nil, nil)
		body := bytes.NewReader(payload)
		again, err := fresh.Write(ctx, f.root, f.actor, op.ID, writer, body)
		if err != nil || body.Len() != 0 || !coordinatorSameJSON(t, again.Result, result.Result) {
			t.Fatalf("terminal body replay: consumed=%v err=%v", body.Len() == 0, err)
		}
		wrong := bytes.Repeat([]byte{'x'}, len(payload))
		_, err = fresh.Write(ctx, f.root, f.actor, op.ID, writer, bytes.NewReader(wrong))
		if !fileops.IsCode(err, "checksum_mismatch") && !fileops.IsCode(err, "idempotency_conflict") {
			t.Fatalf("different payload accepted: %v", err)
		}
		if writer.stages.Load() != 1 || writer.publishes.Load() != 1 || f.usage(t) != usage {
			t.Fatal("terminal replay repeated native effect or quota settlement")
		}
		// Cleanup uses saved temporary identities only; it cannot discover user files as garbage.
		stray := filepath.Join(f.content, "not-indexed-ordinary.txt")
		if err = os.WriteFile(stray, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		if err = fresh.Cleanup(ctx, f.root, f.actor, op.ID, f.writer); err != nil {
			t.Fatal(err)
		}
		if err = fresh.Cleanup(ctx, f.root, f.actor, op.ID, f.writer); err != nil {
			t.Fatal(err)
		}
		if f.saved(t, op.ID).CleanedAt == nil {
			t.Fatal("cleanup acknowledgement missing")
		}
		coordinatorBytes(t, stray, []byte("keep"))
		coordinatorBytes(t, filepath.Join(f.content, string(target)), payload)
		if err = filepath.WalkDir(f.system, func(_ string, e os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if strings.HasSuffix(e.Name(), ".data") {
				return fmt.Errorf("known staging payload survived terminal cleanup")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("digestless terminal replays use immutable server digest", func(t *testing.T) {
		f := newCoordinatorFixture(t, pool)
		op := f.begin(t, "digestless.bin", 4, nil)
		writer := &countedCoordinatorWriter{StagedWriter: f.writer}
		service := f.service(t, nil, nil)
		first, err := service.Write(ctx, f.root, f.actor, op.ID, writer, strings.NewReader("same"))
		if err != nil {
			t.Fatal(err)
		}
		correct := strings.NewReader("same")
		again, err := f.service(t, nil, nil).Write(ctx, f.root, f.actor, op.ID, writer, correct)
		if err != nil || correct.Len() != 0 || !coordinatorSameJSON(t, first.Result, again.Result) {
			t.Fatalf("digestless identical replay: %v", err)
		}
		changed := strings.NewReader("diff")
		_, err = service.Write(ctx, f.root, f.actor, op.ID, writer, changed)
		if !fileops.IsCode(err, "idempotency_conflict") || changed.Len() != 0 {
			t.Fatalf("digestless differing replay was not consumed and rejected: %v", err)
		}
		short := strings.NewReader("x")
		_, err = service.Write(ctx, f.root, f.actor, op.ID, writer, short)
		if !fileops.IsCode(err, "length_mismatch") {
			t.Fatalf("short terminal replay accepted: %v", err)
		}
		if writer.stages.Load() != 1 || writer.publishes.Load() != 1 {
			t.Fatal("terminal replay re-entered native writer")
		}
		coordinatorBytes(t, filepath.Join(f.content, "digestless.bin"), []byte("same"))
	})
	t.Run("native success followed by index rollback recovers once", func(t *testing.T) {
		f := newCoordinatorFixture(t, pool)
		payload := []byte("native-success")
		op := f.begin(t, "recover.bin", int64(len(payload)), nil)
		var commits atomic.Int32
		failing := f.service(t, nil, func(ctx context.Context, tx pgx.Tx, op fileops.Operation, result *storage.MutationResult) (json.RawMessage, error) {
			if commits.Add(1) == 1 {
				if _, err := tx.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size) values($1,$2,'transaction-only','','transaction-only',false,1)`, op.Root.OwnerID, "drive/"+op.Root.OwnerID+"/transaction-only"); err != nil {
					return nil, err
				}
				return nil, errors.New("injected index transaction failure")
			}
			return nodes.CommitStagedFile(ctx, tx, op, result)
		})
		writer := &countedCoordinatorWriter{StagedWriter: f.writer}
		_, err := failing.Write(ctx, f.root, f.actor, op.ID, writer, bytes.NewReader(payload))
		if err == nil {
			t.Fatal("injected settlement error was swallowed")
		}
		saved := f.saved(t, op.ID)
		if saved.Phase != fileops.NativeApplied || saved.NativeResult == nil || saved.Terminal() {
			t.Fatalf("applied native outcome was lost: %+v", saved)
		}
		coordinatorBytes(t, filepath.Join(f.content, "recover.bin"), payload)
		guards, indexed := f.counts(t, op.ID)
		if guards != 1 || indexed != 0 {
			t.Fatalf("guard released or failed index transaction committed: guards=%d index=%d", guards, indexed)
		}
		before := f.usage(t)
		if before.LogicalBytes != 0 || before.LogicalReserved != int64(len(payload)) || before.PhysicalReserved != int64(len(payload)) {
			t.Fatal("unsettled native liability released", before)
		}
		restarted := f.service(t, nil, nodes.CommitStagedFile)
		recovered, err := restarted.Recover(ctx, f.root, f.actor, op.ID, writer)
		if err != nil {
			t.Fatal(err)
		}
		coordinatorState(t, recovered, fileops.Succeeded, fileops.Settled)
		_, err = restarted.Recover(ctx, f.root, f.actor, op.ID, writer)
		if err != nil {
			t.Fatal(err)
		}
		if writer.publishes.Load() != 1 {
			t.Fatal("recovery published a second native effect")
		}
		guards, indexed = f.counts(t, op.ID)
		if guards != 0 || indexed != 1 {
			t.Fatalf("settlement did not repair exact index: %d %d", guards, indexed)
		}
		usage := f.usage(t)
		if usage.LogicalBytes != int64(len(payload)) || usage.LogicalReserved != 0 || usage.PhysicalReserved != 0 {
			t.Fatal("recover quota did not settle once", usage)
		}
	})
	t.Run("invalid body never publishes and zero bytes are valid", func(t *testing.T) {
		for _, tc := range []struct {
			name, body string
			size       int64
			digest     *storage.Digest
		}{{name: "short", body: "x", size: 2}, {name: "long", body: "xxx", size: 2}, {name: "checksum", body: "xx", size: 2, digest: &storage.Digest{Algorithm: "sha-256", Value: make([]byte, 32)}}} {
			t.Run(tc.name, func(t *testing.T) {
				f := newCoordinatorFixture(t, pool)
				op := f.begin(t, "failed.bin", tc.size, tc.digest)
				writer := &countedCoordinatorWriter{StagedWriter: f.writer}
				out, err := f.service(t, nil, nil).Write(ctx, f.root, f.actor, op.ID, writer, strings.NewReader(tc.body))
				if err != nil {
					t.Fatal(err)
				}
				coordinatorState(t, out, fileops.Failed, fileops.Settled)
				coordinatorAbsent(t, filepath.Join(f.content, "failed.bin"))
				if writer.publishes.Load() != 0 {
					t.Fatal("failed receiver published")
				}
				u := f.usage(t)
				if u.LogicalBytes != 0 || u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
					t.Fatal("failed reception retained unneeded liability", u)
				}
			})
		}
		f := newCoordinatorFixture(t, pool)
		op := f.begin(t, "zero", 0, nil)
		out, err := f.service(t, nil, nil).Write(ctx, f.root, f.actor, op.ID, f.writer, bytes.NewReader(nil))
		if err != nil {
			t.Fatal(err)
		}
		coordinatorState(t, out, fileops.Succeeded, fileops.Settled)
		coordinatorBytes(t, filepath.Join(f.content, "zero"), nil)
	})
	t.Run("authorization revocation and cancellation precede final effect", func(t *testing.T) {
		for _, cancelRequest := range []bool{false, true} {
			t.Run(fmt.Sprintf("cancel=%t", cancelRequest), func(t *testing.T) {
				f := newCoordinatorFixture(t, pool)
				op := f.begin(t, "blocked.bin", 4, nil)
				var revoked atomic.Bool
				writer := &countedCoordinatorWriter{StagedWriter: f.writer}
				writer.afterStage = func(context.Context, storage.Staged) error {
					if cancelRequest {
						_, err := f.journal.RequestCancel(ctx, f.root, f.actor, op.ID)
						return err
					}
					revoked.Store(true)
					return nil
				}
				authorize := func(context.Context, fileops.Operation) error {
					if revoked.Load() {
						return errors.New("credential revoked")
					}
					return nil
				}
				result, err := f.service(t, authorize, nil).Write(ctx, f.root, f.actor, op.ID, writer, strings.NewReader("data"))
				if err != nil {
					t.Fatal(err)
				}
				want := fileops.Failed
				if cancelRequest {
					want = fileops.Cancelled
				}
				coordinatorState(t, result, want, fileops.Settled)
				if writer.publishes.Load() != 0 {
					t.Fatal("revoked/cancelled operation published")
				}
				coordinatorAbsent(t, filepath.Join(f.content, "blocked.bin"))
				u := f.usage(t)
				if u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
					t.Fatal("safe pre-effect cancellation leaked quota", u)
				}
			})
		}
	})
	t.Run("receiving crash recovers proven non-application without final effect", func(t *testing.T) {
		for _, cancelRequest := range []bool{false, true} {
			t.Run(fmt.Sprintf("cancel=%t", cancelRequest), func(t *testing.T) {
				f := newCoordinatorFixture(t, pool)
				op := f.begin(t, "receive-crash.bin", 4, nil)
				claimed, err := f.journal.Claim(ctx, f.root, f.actor, op.ID, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				if err = f.journal.SaveCheckpoint(ctx, claimed.Lease, nil); err != nil {
					t.Fatal(err)
				}
				if err = f.journal.Release(ctx, claimed.Lease); err != nil {
					t.Fatal(err)
				}
				if cancelRequest {
					if _, err = f.journal.RequestCancel(ctx, f.root, f.actor, op.ID); err != nil {
						t.Fatal(err)
					}
				}
				writer := &countedCoordinatorWriter{StagedWriter: f.writer}
				var failInspect atomic.Bool
				failInspect.Store(true)
				writer.inspect = func(ctx context.Context, in storage.WriteIntent, evidence []byte) (storage.Inspection, error) {
					if failInspect.Swap(false) {
						return storage.Inspection{}, errors.New("injected temporary inspection failure")
					}
					return f.writer.Inspect(ctx, in, evidence)
				}
				service := f.service(t, nil, nil)
				if _, err = service.Recover(ctx, f.root, f.actor, op.ID, writer); err == nil {
					t.Fatal("inspection failure was reported complete")
				}
				saved := f.saved(t, op.ID)
				if saved.Terminal() || saved.Phase != fileops.Receiving {
					t.Fatal("receiving uncertainty changed final-effect history")
				}
				guards, indexed := f.counts(t, op.ID)
				if guards != 1 || indexed != 0 || f.usage(t).LogicalReserved != 4 {
					t.Fatal("failed inspection released evidence or liability")
				}
				recovered, err := f.service(t, nil, nil).Recover(ctx, f.root, f.actor, op.ID, writer)
				if err != nil {
					t.Fatal(err)
				}
				want := fileops.Failed
				if cancelRequest {
					want = fileops.Cancelled
				}
				coordinatorState(t, recovered, want, fileops.Settled)
				if writer.publishes.Load() != 0 {
					t.Fatal("receiving recovery issued native publication")
				}
				coordinatorAbsent(t, filepath.Join(f.content, "receive-crash.bin"))
				u := f.usage(t)
				if u.LogicalBytes != 0 || u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
					t.Fatal("proven unused receiving reservation retained", u)
				}
			})
		}
	})
	t.Run("paused native publisher survives lease loss as unknown", func(t *testing.T) {
		f := newCoordinatorFixture(t, pool)
		op := f.begin(t, "late.bin", 4, nil)
		entered := make(chan struct{})
		release := make(chan struct{})
		leaseCancelled := make(chan struct{})
		var once sync.Once
		writer := &countedCoordinatorWriter{StagedWriter: f.writer}
		writer.publish = func(ctx context.Context, s storage.Staged, cp storage.Checkpoint) (storage.MutationResult, error) {
			return f.writer.Publish(ctx, s, func(nativeCtx context.Context, b []byte) error {
				if err := cp(nativeCtx, b); err != nil {
					return err
				}
				once.Do(func() {
					close(entered)
					go func() { <-nativeCtx.Done(); close(leaseCancelled) }()
					<-release
				})
				return nil
			})
		}
		type writeResult struct {
			op  fileops.Operation
			err error
		}
		done := make(chan writeResult, 1)
		finished := make(chan struct{})
		service := f.service(t, nil, nil)
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(func() {
			unblock()
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Error("paused publisher did not stop during cleanup")
			}
		})
		go func() {
			defer close(finished)
			out, err := service.Write(ctx, f.root, f.actor, op.ID, writer, strings.NewReader("late"))
			done <- writeResult{out, err}
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("publisher did not reach native checkpoint")
		}
		beforeUnknown := bytes.Clone(f.saved(t, op.ID).Checkpoint)
		if len(beforeUnknown) == 0 {
			t.Fatal("native publication checkpoint was not persisted")
		}
		// Fault injection expires the old DB claim while its real native flock remains held.
		if _, err := pool.Exec(ctx, `update file_operations set lease_expires_at=stamp.at,
 snapshot=jsonb_set(snapshot,'{Lease,ExpiresAt}',to_jsonb(stamp.at))
 from (select clock_timestamp()-interval '1 second' as at) stamp where id=$1`, op.ID); err != nil {
			unblock()
			t.Fatal(err)
		}
		restarted := f.service(t, nil, nil)
		unknown, err := restarted.Recover(ctx, f.root, f.actor, op.ID, f.writer)
		if !fileops.IsCode(err, "outcome_unknown") || unknown.State != fileops.Unknown || unknown.Terminal() {
			unblock()
			t.Fatalf("active native lock was reported terminal: %s %s %v", unknown.State, unknown.Phase, err)
		}
		if !bytes.Equal(unknown.Checkpoint, beforeUnknown) || !bytes.Equal(f.saved(t, op.ID).Checkpoint, beforeUnknown) {
			t.Fatal("nil unknown inspection erased durable native checkpoint")
		}
		newer := f.request("late.bin", 1, nil)
		newer.IdempotencyKey = "newer-writer"
		_, _, err = f.journal.Begin(ctx, newer)
		if !fileops.IsCode(err, "operation_in_progress") {
			unblock()
			t.Fatalf("new writer admitted across unresolved guard: %v", err)
		}
		cancelled, err := f.journal.RequestCancel(ctx, f.root, f.actor, op.ID)
		if err != nil {
			unblock()
			t.Fatal(err)
		}
		if cancelled.Terminal() {
			unblock()
			t.Fatal("in-flight cancellation falsely terminated native effect")
		}
		coordinatorAbsent(t, filepath.Join(f.content, "late.bin"))
		u := f.usage(t)
		if u.LogicalReserved != 4 || u.PhysicalReserved != 4 {
			unblock()
			t.Fatal("unknown native liability released", u)
		}
		select {
		case <-leaseCancelled:
		case <-time.After(5 * time.Second):
			unblock()
			t.Fatal("stale worker lease heartbeat did not stop native work")
		}
		unblock()
		select {
		case out := <-done:
			if out.err == nil {
				t.Fatal("stale publisher falsely succeeded")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("stale writer did not stop")
		}
		recovered, err := restarted.Recover(ctx, f.root, f.actor, op.ID, f.writer)
		if err != nil {
			t.Fatal(err)
		}
		coordinatorState(t, recovered, fileops.Cancelled, fileops.Settled)
		coordinatorAbsent(t, filepath.Join(f.content, "late.bin"))
		if writer.publishes.Load() != 1 {
			t.Fatal("recovery retried final publication")
		}
		u = f.usage(t)
		if u.LogicalBytes != 0 || u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
			t.Fatal("proven non-application did not release unused liability", u)
		}
	})
	t.Run("applied native effect with lost reply remains guarded until inspection", func(t *testing.T) {
		f := newCoordinatorFixture(t, pool)
		op := f.begin(t, "lost-reply.bin", 4, nil)
		var unresolved atomic.Bool
		unresolved.Store(true)
		writer := &countedCoordinatorWriter{StagedWriter: f.writer}
		writer.publish = func(ctx context.Context, s storage.Staged, cp storage.Checkpoint) (storage.MutationResult, error) {
			_, err := f.writer.Publish(ctx, s, cp)
			if err != nil {
				return storage.MutationResult{}, err
			}
			return storage.MutationResult{}, io.ErrUnexpectedEOF
		}
		writer.inspect = func(ctx context.Context, in storage.WriteIntent, evidence []byte) (storage.Inspection, error) {
			if unresolved.Load() {
				return storage.Inspection{State: storage.OutcomeUnknown, Evidence: evidence}, nil
			}
			return f.writer.Inspect(ctx, in, evidence)
		}
		service := f.service(t, nil, nil)
		unknown, err := service.Write(ctx, f.root, f.actor, op.ID, writer, strings.NewReader("keep"))
		if !fileops.IsCode(err, "outcome_unknown") || unknown.State != fileops.Unknown || unknown.Terminal() {
			t.Fatalf("lost native response became terminal: %s %v", unknown.State, err)
		}
		coordinatorBytes(t, filepath.Join(f.content, "lost-reply.bin"), []byte("keep"))
		guards, indexed := f.counts(t, op.ID)
		if guards != 1 || indexed != 0 {
			t.Fatal("unknown native file was hidden or guard dropped")
		}
		cancelled, err := f.journal.RequestCancel(ctx, f.root, f.actor, op.ID)
		if err != nil || cancelled.Terminal() {
			t.Fatalf("cancel claimed unknown native rollback: %s %v", cancelled.State, err)
		}
		next := f.request("lost-reply.bin", 1, nil)
		next.IdempotencyKey = "new-after-lost-reply"
		if _, _, err = f.journal.Begin(ctx, next); !fileops.IsCode(err, "operation_in_progress") {
			t.Fatal("unknown final effect did not block conflicting mutation", err)
		}
		if err = service.Cleanup(ctx, f.root, f.actor, op.ID, writer); !fileops.IsCode(err, "operation_in_progress") {
			t.Fatal("unknown operation cleanup admitted", err)
		}
		unresolved.Store(false)
		result, err := f.service(t, nil, nil).Recover(ctx, f.root, f.actor, op.ID, writer)
		if err != nil {
			t.Fatal(err)
		}
		coordinatorState(t, result, fileops.Succeeded, fileops.Settled)
		if !result.CancelRequested || writer.publishes.Load() != 1 {
			t.Fatal("recovery discarded cancellation history or republished")
		}
		u := f.usage(t)
		if u.LogicalBytes != 4 || u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
			t.Fatal("late success not charged exactly once", u)
		}
		coordinatorBytes(t, filepath.Join(f.content, "lost-reply.bin"), []byte("keep"))
	})
	t.Run("revocation after native publication does not prevent settlement or cleanup", func(t *testing.T) {
		f := newCoordinatorFixture(t, pool)
		op := f.begin(t, "settle-revoked.bin", 4, nil)
		var revoked atomic.Bool
		var checks atomic.Int32
		authorize := func(context.Context, fileops.Operation) error {
			checks.Add(1)
			if revoked.Load() {
				return errors.New("creator revoked")
			}
			return nil
		}
		writer := &countedCoordinatorWriter{StagedWriter: f.writer}
		writer.publish = func(ctx context.Context, s storage.Staged, cp storage.Checkpoint) (storage.MutationResult, error) {
			result, err := f.writer.Publish(ctx, s, cp)
			if err == nil {
				revoked.Store(true)
			}
			return result, err
		}
		service := f.service(t, authorize, nil)
		result, err := service.Write(ctx, f.root, f.actor, op.ID, writer, strings.NewReader("keep"))
		if err != nil {
			t.Fatal(err)
		}
		coordinatorState(t, result, fileops.Succeeded, fileops.Settled)
		if checks.Load() != 2 || writer.publishes.Load() != 1 {
			t.Fatalf("authorization/publish checks=%d publishes=%d", checks.Load(), writer.publishes.Load())
		}
		if _, err = service.Write(ctx, f.root, f.actor, op.ID, writer, strings.NewReader("keep")); err == nil {
			t.Fatal("a new client action bypassed revocation")
		}
		beforeCleanup := checks.Load()
		if err = service.Cleanup(ctx, f.root, f.actor, op.ID, writer); err != nil {
			t.Fatal(err)
		}
		if checks.Load() != beforeCleanup {
			t.Fatal("safe temporary cleanup re-authorized publication")
		}
		u := f.usage(t)
		if u.LogicalBytes != 4 || u.PhysicalBytes != 4 || u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
			t.Fatal("revocation left applied bytes uncharged", u)
		}
		guards, indexed := f.counts(t, op.ID)
		if guards != 0 || indexed != 1 || f.saved(t, op.ID).CleanedAt == nil {
			t.Fatal("applied repair/cleanup not settled")
		}
		coordinatorBytes(t, filepath.Join(f.content, "settle-revoked.bin"), []byte("keep"))
	})
	t.Run("owner repair after index failure does not re-authorize native publication", func(t *testing.T) {
		f := newCoordinatorFixture(t, pool)
		op := f.begin(t, "owner-recover.bin", 4, nil)
		var revoked atomic.Bool
		authorize := func(context.Context, fileops.Operation) error {
			if revoked.Load() {
				return errors.New("creator revoked")
			}
			return nil
		}
		writer := &countedCoordinatorWriter{StagedWriter: f.writer}
		writer.publish = func(ctx context.Context, s storage.Staged, cp storage.Checkpoint) (storage.MutationResult, error) {
			result, err := f.writer.Publish(ctx, s, cp)
			if err == nil {
				revoked.Store(true)
			}
			return result, err
		}
		failing := f.service(t, authorize, func(context.Context, pgx.Tx, fileops.Operation, *storage.MutationResult) (json.RawMessage, error) {
			return nil, errors.New("injected index unavailable after native effect")
		})
		if _, err := failing.Write(ctx, f.root, f.actor, op.ID, writer, strings.NewReader("keep")); err == nil {
			t.Fatal("index failure swallowed")
		}
		saved := f.saved(t, op.ID)
		if saved.Phase != fileops.NativeApplied || saved.Terminal() {
			t.Fatal("applied native outcome was lost")
		}
		coordinatorBytes(t, filepath.Join(f.content, "owner-recover.bin"), []byte("keep"))
		var repairChecks atomic.Int32
		repair := f.service(t, func(context.Context, fileops.Operation) error {
			repairChecks.Add(1)
			return errors.New("creator publication authority remains revoked")
		}, nil)
		// Public token/session validity must be checked by the caller. Internal repair
		// uses the current recovery actor and cannot issue a new final native effect.
		other := f.actor
		other.ID = "token:unrelated"
		other.ManageOwner = true
		if _, err := repair.Recover(ctx, f.root, other, op.ID, writer); !fileops.IsCode(err, "forbidden") {
			t.Fatal("another app gained recovery access", err)
		}
		owner := fileops.Actor{ID: "owner:" + f.root.OwnerID, CanRead: true, CanWrite: true, ManageOwner: true}
		result, err := repair.Recover(ctx, f.root, owner, op.ID, writer)
		if err != nil {
			t.Fatal(err)
		}
		coordinatorState(t, result, fileops.Succeeded, fileops.Settled)
		if err = repair.Cleanup(ctx, f.root, owner, op.ID, writer); err != nil {
			t.Fatal(err)
		}
		if repairChecks.Load() != 0 || writer.publishes.Load() != 1 {
			t.Fatal("repair consulted publication authority or repeated native effect")
		}
		u := f.usage(t)
		if u.LogicalBytes != 4 || u.PhysicalBytes != 4 || u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
			t.Fatal("owner repair did not settle applied liability", u)
		}
	})
}
