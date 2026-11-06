package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/uploadspool"
)

type uploadEarlyStageFailure struct {
	fileops.NativeActions
	stageCalls   int
	discardCalls int
	publishCalls int
}

func (w *uploadEarlyStageFailure) Stage(context.Context, storage.WriteIntent, io.Reader, storage.Checkpoint) (storage.Staged, error) {
	w.stageCalls++
	// No body read and no checkpoint: the journal must still be Admitted when
	// Service attempts cleanup, precisely the case that previously got stuck.
	return storage.Staged{}, storage.NewError("unavailable", "", errors.New("injected before first checkpoint"))
}
func (w *uploadEarlyStageFailure) Discard(ctx context.Context, intent storage.WriteIntent, checkpoint []byte) error {
	w.discardCalls++
	return w.NativeActions.Discard(ctx, intent, checkpoint)
}
func (w *uploadEarlyStageFailure) Publish(context.Context, storage.Staged, storage.Checkpoint) (storage.MutationResult, error) {
	w.publishCalls++
	return storage.MutationResult{}, errors.New("unexpected publication after failed stage")
}

func uploadFailureFixture(t *testing.T, f *fileWorkerFixture) (*Server, fileops.Upload, fileops.NativeActions, *uploadspool.Store, []uploadspool.Part, *fileops.Service) {
	t.Helper()
	root, native := f.owner(t)
	actor := workerActor(root)
	parent, err := native.Stat(f.ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	client := storage.WriteIntent{Target: "early-failure", Size: 4, MediaType: "application/octet-stream", ClientCondition: "create:default"}
	intent := client
	intent.Conditions = storage.Conditions{Absent: true}
	intent.ParentVersion = parent.Version
	encoded, _ := json.Marshal(intent)
	upload, _, err := f.journal.CreateUpload(f.ctx, fileops.BeginRequest{Root: root, Actor: actor, Kind: "upload_commit", Intent: encoded, Reservation: fileops.ReservationRequest{NewBytes: 4, PhysicalBytes: 24}}, fileops.UploadOptions{PartSize: 4, ClientIntent: client})
	if err != nil {
		t.Fatal(err)
	}
	spool, err := uploadspool.New(root.SystemRoot, native.StorageBinding())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { spool.Close() })
	part, err := spool.Receive(f.ctx, upload.ID, 1, 4, nil, strings.NewReader("safe"), func(p uploadspool.Part) (uploadspool.Part, error) {
		_, _, err := f.journal.AcceptUploadPart(f.ctx, root, actor, upload.ID, fileops.PartReceipt{Number: p.Number, Size: p.Size, SHA256: p.SHA256, Handle: p.Handle})
		return p, err
	})
	if err != nil {
		t.Fatal(err)
	}
	upload, err = f.journal.FreezeUpload(f.ctx, root, actor, upload.ID)
	if err != nil {
		t.Fatal(err)
	}
	service, err := fileops.NewService(f.journal, fileops.ServiceOptions{Authorize: f.auth.AuthorizeFileOperation, Commit: nodes.CommitStagedFile})
	if err != nil {
		t.Fatal(err)
	}
	return f.server(), upload, native, spool, []uploadspool.Part{part}, service
}

func TestFilesUploadPostgresEarlyStageFailureCleanup(t *testing.T) {
	f := newFileWorkerFixture(t)
	t.Run("own assembled reader closes before failed stage cleanup", func(t *testing.T) {
		server, upload, native, spool, parts, service := uploadFailureFixture(t, f)
		failing := &uploadEarlyStageFailure{NativeActions: native}
		op, err := server.runUploadOperation(f.ctx, upload.Operation, workerActor(upload.Operation.Root), failing, service)
		if err != nil {
			t.Fatalf("early stage error did not settle safely: %v", err)
		}
		// runUploadOperation returns the pre-cleanup snapshot; inspect the journal
		// below for the durable cleanup acknowledgement.
		if op.State != fileops.Failed {
			t.Fatalf("operation remained %s/%s", op.State, op.Phase)
		}
		saved, err := f.journal.Get(f.ctx, op.Root, workerActor(op.Root), op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if saved.State != fileops.Failed || saved.CleanedAt == nil {
			t.Fatalf("failed attempt not durably cleaned: %+v", saved)
		}
		if failing.stageCalls != 1 || failing.discardCalls < 1 || failing.publishCalls != 0 {
			t.Fatalf("unexpected native calls: %+v", failing)
		}
		var checkpoints int
		if err := f.db.Pool().QueryRow(f.ctx, `select count(*) from file_operation_checkpoints where operation_id=$1`, op.ID).Scan(&checkpoints); err != nil || checkpoints != 0 {
			t.Fatalf("test did not exercise pre-checkpoint failure: %d %v", checkpoints, err)
		}
		if _, err := os.Stat(filepath.Join(op.Root.ContentRoot, "early-failure")); !os.IsNotExist(err) {
			t.Fatalf("failed stage published content: %v", err)
		}
		if body, err := spool.Open(f.ctx, op.ID, parts); !errors.Is(err, uploadspool.ErrDiscarded) {
			if body != nil {
				body.Close()
			}
			t.Fatalf("failed stage spool was not fenced: %v", err)
		}
		f.checkNoLiability(t, op.Root)
	})
	t.Run("independent reader keeps liability then tombstone retries recover", func(t *testing.T) {
		server, upload, native, spool, parts, service := uploadFailureFixture(t, f)
		actor := workerActor(upload.Operation.Root)
		reader, err := spool.Open(f.ctx, upload.ID, parts)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		failing := &uploadEarlyStageFailure{NativeActions: native}
		_, err = server.runUploadOperation(f.ctx, upload.Operation, actor, failing, service)
		if !fileops.IsCode(err, "recovery_required") {
			t.Fatalf("busy cleanup should retain recovery: %v", err)
		}
		pending, err := f.journal.Get(f.ctx, upload.Operation.Root, actor, upload.ID)
		if err != nil {
			t.Fatal(err)
		}
		if pending.Terminal() || pending.Phase != fileops.Admitted || pending.CleanedAt != nil {
			t.Fatalf("active independent reader prematurely settled: %+v", pending)
		}
		usage, err := f.journal.Usage(f.ctx, pending.Root, actor)
		if err != nil {
			t.Fatal(err)
		}
		if usage.PhysicalReserved != 24 || usage.LogicalReserved != 4 || failing.discardCalls != 0 {
			t.Fatalf("busy cleanup released liability/native state: %+v calls%d", usage, failing.discardCalls)
		}
		data, err := io.ReadAll(reader)
		if err != nil || string(data) != "safe" {
			t.Fatalf("independent reader lost protected bytes: %q %v", data, err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		// The durable tombstone now blocks Open even though the journal never got a
		// first checkpoint. Recovery must inspect the native intent rather than try
		// staging again or remain stuck at ErrDiscarded on every worker retry.
		if _, err := server.runUploadOperation(f.ctx, pending, actor, failing, service); err != nil {
			t.Fatalf("tombstoned admitted session failed recovery: %v", err)
		}
		saved, err := f.journal.Get(f.ctx, pending.Root, actor, pending.ID)
		if err != nil {
			t.Fatal(err)
		}
		if saved.State != fileops.Failed || saved.CleanedAt == nil || failing.stageCalls != 1 || failing.publishCalls != 0 {
			t.Fatalf("tombstoned retry did not safely retire: %+v calls%+v", saved, failing)
		}
		f.checkNoLiability(t, pending.Root)
	})
}
