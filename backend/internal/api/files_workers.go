package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storage"
)

type fileWorkers struct {
	mu     sync.Mutex
	active map[string]bool
	slots  chan struct{}
	ctx    context.Context
}

func (s *Server) workers() *fileWorkers {
	s.fileWorkerMu.Lock()
	defer s.fileWorkerMu.Unlock()
	if s.fileWorkers == nil {
		s.fileWorkers = &fileWorkers{active: make(map[string]bool), slots: make(chan struct{}, 4), ctx: context.Background()}
	}
	return s.fileWorkers
}
func (s *Server) queueFileOperation(op fileops.Operation) error {
	if s.filesJournal == nil || s.trees == nil || !s.cfg.Storage.CoordinatedWrites {
		return fc.NewProblem(503, "temporarily_unavailable", "File mutation workers are not configured.")
	}
	worker := s.workers()
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if worker.active[op.ID] {
		return nil
	}
	if err := worker.ctx.Err(); err != nil {
		return err
	}
	select {
	case worker.slots <- struct{}{}:
	default:
		return errFilesWorkerBusy
	}
	worker.active[op.ID] = true
	parentCtx := worker.ctx
	go func() {
		defer func() { worker.mu.Lock(); delete(worker.active, op.ID); worker.mu.Unlock(); <-worker.slots }()
		// No wall clock here: the operation is resumable from its journal, every
		// native request is bounded by the provider's own timeout, and a folder move
		// of one terabyte legitimately outlives any deadline picked in advance. A
		// timer on top only ever fires on the operation large enough to matter, and
		// the client then sees a failure the response cannot explain.
		if err := s.runFileOperation(parentCtx, op); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("file operation retained for recovery", "operation", op.ID, "error", err)
		}
	}()
	return nil
}
func (s *Server) runFileOperation(ctx context.Context, op fileops.Operation) error {
	driver, err := s.trees.Open(ctx, op.Root.OwnerID)
	if err != nil {
		return err
	}
	defer driver.Close()
	native, ok := driver.(fileops.NativeActions)
	if !ok {
		return fileops.E("unsupported", nil)
	}
	binding := native.StorageBinding()
	if binding.OwnerID != op.Root.OwnerID || binding.ConnectionID != op.Root.ConnectionID || binding.Generation != op.Root.Generation || binding.ContentRoot != op.Root.ContentRoot || binding.SystemRoot != op.Root.SystemRoot || native.StorageNameRules().CaseSensitive != op.Root.CaseSensitive || native.Describe().RetainsVersions != op.Root.RetainsVersions {
		return fileops.E("root_conflict", nil)
	}
	// The journal captured provenance permits system recovery of effects while
	// publication authorization independently checks the original live credential.
	actor := fileops.Actor{ID: "owner:" + op.Root.OwnerID, CanRead: true, CanWrite: true, ManageOwner: true}
	authorize := func(ctx context.Context, op fileops.Operation) error {
		if s.auth == nil {
			return fileops.E("forbidden", nil)
		}
		if err := s.auth.AuthorizeFileOperation(ctx, op); err != nil {
			return err
		}
		if err := s.filesCheckIdentityBindings(ctx, op); err != nil {
			return err
		}
		if op.Kind == "upload_commit" {
			var intent storage.WriteIntent
			if json.Unmarshal(op.Intent, &intent) != nil {
				return fileops.E("invalid_intent", nil)
			}
			parent, err := native.Stat(ctx, intent.ParentPath)
			if err != nil || parent.Kind != storage.Folder || !bytes.Equal(parent.Version, intent.ParentVersion) {
				return fileops.E("target_changed", err)
			}
		}
		return nil
	}
	if op.Kind == "upload_commit" {
		service, err := fileops.NewService(s.filesJournal, fileops.ServiceOptions{Authorize: authorize, Commit: nodes.CommitStagedFile, PhysicalDelta: func(op fileops.Operation, result storage.MutationResult) (int64, error) {
			if result.Entry == nil {
				return 0, fileops.E("invalid_native_result", nil)
			}
			return fileops.PhysicalDelta(op.Root, result.Entry.Size, op.Reservation.OldBytes), nil
		}})
		if err != nil {
			return err
		}
		if _, e := s.filesJournal.GetUpload(ctx, op.Root, actor, op.ID); e == nil {
			_, e = s.runUploadOperation(ctx, op, actor, native, service)
			return e
		} else if !fileops.IsCode(e, "not_found") {
			return e
		}
		var updated fileops.Operation
		if op.Phase == fileops.StagedReady && op.State != fileops.Unknown {
			updated, err = service.Write(ctx, op.Root, actor, op.ID, native, nil)
			if fileops.IsCode(err, "forbidden") || fileops.IsCode(err, "target_changed") || fileops.IsCode(err, "precondition_failed") {
				latest, readErr := s.filesJournal.Get(ctx, op.Root, actor, op.ID)
				if readErr == nil && (latest.Phase == fileops.Admitted || latest.Phase == fileops.Receiving || latest.Phase == fileops.StagedReady) {
					if _, cancelErr := s.filesJournal.RequestCancel(ctx, op.Root, actor, op.ID); cancelErr == nil {
						updated, err = service.Recover(ctx, op.Root, actor, op.ID, native)
					}
				}
			}
		} else {
			updated, err = service.Recover(ctx, op.Root, actor, op.ID, native)
		}
		if err == nil && updated.Terminal() {
			err = service.Cleanup(ctx, op.Root, actor, op.ID, native)
		}
		return err
	}
	actions, err := fileops.NewActions(s.filesJournal, fileops.ActionsOptions{Authorize: authorize, Validate: func(context.Context, fileops.NativeActions, fileops.ActionRequest) error { return nil }, Commit: nodes.CommitFileAction})
	if err != nil {
		return err
	}
	_, err = actions.Run(ctx, op.Root, actor, op.ID, native)
	return err
}

// StartFileOperations retries durable known work after startup; unknown outcomes
// still inspect their original native identity and never blindly replay effects.
func (s *Server) StartFileOperations(ctx context.Context) {
	if !s.cfg.Storage.CoordinatedWrites || s.filesJournal == nil || s.trees == nil {
		return
	}
	worker := s.workers()
	worker.mu.Lock()
	worker.ctx = ctx
	worker.mu.Unlock()
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			if expired, e := s.filesJournal.ExpireUploads(ctx, 32); e == nil {
				for _, u := range expired {
					_ = s.queueFileOperation(u.Operation)
				}
			}
			pending, err := s.filesJournal.Pending(ctx, 32)
			if err != nil && ctx.Err() == nil {
				slog.Warn("file operation scan unavailable", "error", err)
			}
			for _, op := range pending {
				if err = s.queueFileOperation(op); errors.Is(err, errFilesWorkerBusy) {
					break
				}
			}
			if cleanup, e := s.filesJournal.CleanupCandidates(ctx, 16); e == nil {
				for _, op := range cleanup {
					if e = s.queueFileOperation(op); errors.Is(e, errFilesWorkerBusy) {
						break
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
