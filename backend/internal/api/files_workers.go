package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storage"
)

type fileWorkers struct {
	mu      sync.Mutex
	active  map[string]bool
	retries map[string]*fileWorkerRetry
	slots   chan struct{}
	// wake carries "a slot just freed" from a finishing operation to the scanner.
	// Without it the only thing that admits deferred work is the periodic sweep, so
	// every completed commit leaves its successor waiting up to one sweep interval
	// even though the pool has room again.
	wake chan struct{}
	ctx  context.Context
}

// fileOperationSlots bounds how many operations may be in progress at once. It is a
// concurrency bound, not a throughput one: settling one small upload is a few provider round
// trips plus a short index transaction, all I/O, so raising this count does not make the
// work cheaper — it only decides where the queue forms.
//
// It used to be 32, sized as "four clients × eight chains", which quietly made this pool the
// gate for one client's import: the client's settlement pages drained it exactly, and every
// batch beyond that waited here where the browser could not see it. The pool is 1024 now, so
// a single import never waits on it. What waits instead, in this order:
//
//   - the provider transport, which keeps MaxConnsPerHost (64) requests in flight and queues
//     the rest in the socket layer;
//   - PostgreSQL: 40 pool connections, each held only for the duration of one short
//     transaction, so a thousand operations interleave over forty connections;
//   - the per-owner advisory lock, which serialises every settlement transaction of one
//     owner regardless of pool size. That is the bound on a single user's settlement rate,
//     and it is what makes a deeper pool cost latency instead of buying throughput.
//
// Each operation is cheap to hold — a goroutine, one spool root handle and a small struct —
// and it does not take a connection between transactions, so 1024 admitted operations cost
// memory and file descriptors, not database capacity. The bound stays process-wide rather
// than per owner: one import can still starve another owner's commit, which is a scheduling
// question this constant does not answer.
const fileOperationSlots = 1024

// pendingScanPage is how many pending operations the scanner reads per query. It is a page
// size, not the pool size: the two were one constant until the pool grew past the journal's
// bound and every scan was refused, so nothing deferred or interrupted was resumed.
//
// The scan runs on every freed slot, when there is usually one operation to admit, and on
// the periodic sweep, when a restart may have the whole pool to refill. 256 rows is about
// 1.3 MB of snapshot JSON per round trip (an upload commit is roughly 5 KB), which keeps the
// common one-admission scan cheap and still refills an empty pool in four pages.
const pendingScanPage = 256

// The journal refuses a page above its bound, and a refused scan fails every five seconds
// without stopping the server. Fail the build instead: a negative constant cannot be a uint.
const _ = uint(fileops.PendingPageMax - pendingScanPage)

// fileWorkerRetry is the consecutive-failure record for one operation. The journal
// already knows a retry happened (it is the expiry of the lease), but it cannot say
// how many times the same attempt failed or when the next one is due; that is the
// difference between an operation that is slow and one that can never settle.
type fileWorkerRetry struct {
	attempts int
	code     string
	lastAt   time.Time
	nextTry  time.Time
}

// An operation that keeps failing backs off instead of retrying once per lease TTL
// forever. The first attempt after a failure waits the floor; each consecutive
// failure doubles the wait up to the ceiling, so a stuck operation costs a handful of
// log lines an hour instead of thousands a day.
const (
	fileOperationRetryFloor   = 30 * time.Second
	fileOperationRetryCeiling = 30 * time.Minute
)

func fileOperationBackoff(attempts int) time.Duration {
	if attempts < 1 {
		return fileOperationRetryFloor
	}
	wait := fileOperationRetryFloor
	for i := 1; i < attempts && wait < fileOperationRetryCeiling; i++ {
		wait *= 2
	}
	if wait > fileOperationRetryCeiling {
		wait = fileOperationRetryCeiling
	}
	return wait
}

func (s *Server) workers() *fileWorkers {
	s.fileWorkerMu.Lock()
	defer s.fileWorkerMu.Unlock()
	if s.fileWorkers == nil {
		s.fileWorkers = &fileWorkers{
			active:  make(map[string]bool),
			retries: make(map[string]*fileWorkerRetry),
			slots:   make(chan struct{}, fileOperationSlots),
			wake:    make(chan struct{}, 1),
			ctx:     context.Background(),
		}
	}
	return s.fileWorkers
}
func (s *Server) queueFileOperation(op fileops.Operation) error {
	_, _, err := s.startFileOperation(op)
	return err
}

// startFileOperation admits one operation into the pool. started reports that this call took
// a slot and launched it; when it did, done closes once the operation has finished, so a
// request that admitted the work can wait for the settlement it just started instead of
// returning a token the client has to poll. A caller that gets started=false (already owned
// by the scanner or another request, waiting out a retry, or no free slot) falls back to the
// durable Operation.
func (s *Server) startFileOperation(op fileops.Operation) (bool, <-chan struct{}, error) {
	if s.filesJournal == nil || s.trees == nil || !s.cfg.Storage.CoordinatedWrites {
		return false, nil, fc.NewProblem(503, "temporarily_unavailable", "File mutation workers are not configured.")
	}
	worker := s.workers()
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if worker.active[op.ID] {
		return false, nil, nil
	}
	if retry, ok := worker.retries[op.ID]; ok && time.Now().Before(retry.nextTry) {
		// Deliberately quiet: the deferred attempt is reported where it is scheduled,
		// with the cause chain, so the retry itself is not news.
		slog.Debug("file operation retry deferred", "operation", op.ID, "attempt", retry.attempts, "code", retry.code, "retryIn", time.Until(retry.nextTry).Round(time.Second))
		return false, nil, nil
	}
	if err := worker.ctx.Err(); err != nil {
		return false, nil, err
	}
	select {
	case worker.slots <- struct{}{}:
	default:
		return false, nil, errFilesWorkerBusy
	}
	worker.active[op.ID] = true
	parentCtx := worker.ctx
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			worker.mu.Lock()
			delete(worker.active, op.ID)
			worker.mu.Unlock()
			<-worker.slots
			// The scan that would have admitted the next operation already stopped at
			// the busy pool, so a freed slot is the only moment that work can start.
			// Tell the scanner instead of leaving it to the periodic sweep.
			select {
			case worker.wake <- struct{}{}:
			default:
			}
		}()
		// No wall clock here: the operation is resumable from its journal, every
		// native request is bounded by the provider's own timeout, and a folder move
		// of one terabyte legitimately outlives any deadline picked in advance. A
		// timer on top only ever fires on the operation large enough to matter, and
		// the client then sees a failure the response cannot explain.
		err := s.runFileOperation(parentCtx, op)
		switch {
		case err == nil:
			s.fileOperationProgressed(op.ID)
		case errors.Is(err, context.Canceled):
			slog.Debug("file operation stopped with the server", "operation", op.ID)
		default:
			s.fileOperationRetained(op, err)
		}
	}()
	return true, done, nil
}

// fileOperationProgressed clears the failure record: the next attempt starts from a
// clean slate instead of inheriting an old backoff.
func (s *Server) fileOperationProgressed(id string) {
	worker := s.workers()
	worker.mu.Lock()
	defer worker.mu.Unlock()
	delete(worker.retries, id)
}

// fileOperationRetained reports one failed attempt with everything needed to debug it
// without reproducing it: which operation, which kind, how far its manifest got, the
// code of the failing step, the whole cause chain and when the next attempt is due.
// A failure that repeats is the only way to diagnose a stuck operation, so the record
// escalates to Error after a few attempts instead of staying one unremarkable warning.
func (s *Server) fileOperationRetained(op fileops.Operation, err error) {
	worker := s.workers()
	worker.mu.Lock()
	retry := worker.retries[op.ID]
	if retry == nil {
		retry = &fileWorkerRetry{}
		worker.retries[op.ID] = retry
	}
	retry.attempts++
	retry.code = fileOperationErrorCode(err)
	retry.lastAt = time.Now()
	retry.nextTry = retry.lastAt.Add(fileOperationBackoff(retry.attempts))
	if len(worker.retries) > 4096 {
		// Terminal operations never call this path; a map this large means the scan is
		// replaying operations that are not making progress at all.
		slog.Warn("file operation retry table is large", "entries", len(worker.retries))
	}
	attempts, code, nextTry := retry.attempts, retry.code, retry.nextTry
	worker.mu.Unlock()

	applied, total := 0, len(op.Entries)
	for _, entry := range op.Entries {
		if entry.State == "applied" {
			applied++
		}
	}
	attributes := []any{
		"operation", op.ID, "kind", op.Kind, "state", op.State, "phase", op.Phase,
		"owner", op.Root.OwnerID, "attempt", attempts, "code", code,
		"cause", errorCauseChain(err), "entriesApplied", applied, "entriesTotal", total,
		"retryIn", time.Until(nextTry).Round(time.Second),
	}
	if attempts > 3 {
		slog.Error("file operation cannot make progress", attributes...)
		return
	}
	slog.Warn("file operation retained for recovery", attributes...)
}

// fileOperationErrorCode names the layer that refused, without the parameters a
// database or provider error can carry.
func fileOperationErrorCode(err error) string {
	var operation *fileops.Error
	if errors.As(err, &operation) {
		return operation.Code
	}
	var native *storage.Error
	if errors.As(err, &native) {
		return native.Code
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	return "error"
}

// errorCauseChain renders the whole chain, not just its outermost code: every
// wrapper the operation passes through names its own step, and the leaf is the
// message a person can act on.
func errorCauseChain(err error) string {
	var parts []string
	for current := err; current != nil; current = errors.Unwrap(current) {
		var part string
		switch typed := current.(type) {
		case *fileops.Error:
			part = typed.Code
		case *storage.Error:
			part = typed.Code
		default:
			part = current.Error()
		}
		if part == "" || len(parts) > 0 && parts[len(parts)-1] == part {
			continue
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " <- ")
}

// fileOperationRetry exposes the retry state on the operation resource, so a stuck
// operation is visible to a client instead of only to whoever reads the server log.
func (s *Server) fileOperationRetry(id string) map[string]any {
	worker := s.workers()
	worker.mu.Lock()
	defer worker.mu.Unlock()
	retry := worker.retries[id]
	if retry == nil {
		return nil
	}
	return map[string]any{"attempts": retry.attempts, "code": retry.code, "lastAttemptAt": retry.lastAt.UTC(), "nextAttemptAt": retry.nextTry.UTC()}
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
			// A promised parent has no native representation until its first child lands;
			// absence is the state the session was admitted in, not a change.
			if storage.IsCode(err, "not_found") && intent.Parents {
				return nil
			}
			if err != nil || parent.Kind != storage.Folder || !bytes.Equal(parent.Revision, intent.ParentRevision) {
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
	actions, err := fileops.NewActions(s.filesJournal, fileops.ActionsOptions{Authorize: authorize, Validate: func(context.Context, fileops.NativeActions, fileops.ActionRequest) error { return nil }, Commit: nodes.CommitFileAction, PrefixCommit: nodes.CommitPrefixDelete, LivePaths: s.filesLivePaths})
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
		// A wake says only "a slot is free", so it drains the queue and returns to wait;
		// expiry and cleanup stay on the ticker. Running all three on every completed
		// commit would spend two queries per file to learn nothing new.
		periodic := true
		for {
			if periodic {
				expired, e := s.filesJournal.ExpireUploads(ctx, 32)
				warnFileOperationScan(ctx, "upload expiry", "expired upload sessions keep their reservations", e)
				if e == nil {
					for _, u := range expired {
						_ = s.queueFileOperation(u.Operation)
					}
				}
			}
			s.drainPendingFileOperations(ctx)
			if periodic {
				cleanup, e := s.filesJournal.CleanupCandidates(ctx, 16)
				warnFileOperationScan(ctx, "cleanup", "finished uploads keep their staged data and physical reservation", e)
				if e == nil {
					for _, op := range cleanup {
						if e = s.queueFileOperation(op); errors.Is(e, errFilesWorkerBusy) {
							break
						}
					}
				}
			}
			periodic = true
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-worker.wake:
				periodic = false
			}
		}
	}()
}

// drainPendingFileOperations admits durable work that is waiting for a slot, oldest first,
// walking the whole queue a page at a time. A refused admission means the pool is full, so
// the walk stops there and lets the next wake (or sweep) retry; pushing the rest would only
// park them in the busy path again. An operation that is already running or waiting out a
// retry is passed over without ending the walk, so it cannot hide the work queued behind it.
func (s *Server) drainPendingFileOperations(ctx context.Context) {
	err := s.filesJournal.EachPending(ctx, pendingScanPage, func(op fileops.Operation) bool {
		return !errors.Is(s.queueFileOperation(op), errFilesWorkerBusy)
	})
	warnFileOperationScan(ctx, "pending", "deferred and interrupted operations are not resumed", err)
}

// warnFileOperationScan reports a background scan that could not read the journal. These
// scans retry forever and nothing else surfaces their failure, so the line has to stand on
// its own: which scan, what stays undone while it fails, and the whole cause chain — a bare
// code here read like an outage when it was a constant the journal refused.
func warnFileOperationScan(ctx context.Context, scan, stalled string, err error) {
	if err == nil || ctx.Err() != nil {
		return
	}
	slog.Warn("file operation scan failed", "scan", scan, "stalled", stalled, "code", fileOperationErrorCode(err), "cause", errorCauseChain(err))
}
