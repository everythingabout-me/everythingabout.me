package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
)

func filesOperationID(r *http.Request) string {
	p := strings.TrimPrefix(r.URL.Path, "/api/files/operations/")
	return strings.TrimSuffix(p, "/cancel")
}
func writeFilesOperation(w http.ResponseWriter, op fileops.Operation, status int, scopes ...*fileRequest) error {
	return writeFilesOperationWithRetry(w, op, status, nil, scopes...)
}

// writeFilesOperationWithRetry adds the retry record for an operation whose worker
// keeps failing. It is the only place a client can learn that an operation is not
// slow but stuck: without it the resource looks identical to one that is merely
// still running, and only the server log knows the difference.
func writeFilesOperationWithRetry(w http.ResponseWriter, op fileops.Operation, status int, retrying map[string]any, scopes ...*fileRequest) error {
	if len(scopes) > 0 {
		op.Result = filesScopedResult(op.Result, scopes[0])
	}
	w.Header().Set("Location", "/api/files/operations/"+op.ID)
	if !op.Terminal() {
		w.Header().Set("Retry-After", "2")
	}
	completed := 0
	for _, e := range op.Entries {
		if e.State == "applied" || e.State == "not_applied" {
			completed++
		}
	}
	var total any = len(op.Entries)
	unit := "entries"
	switch {
	case op.Kind == "upload_commit":
		unit = "bytes"
		total = op.Reservation.NewBytes
		if op.State == fileops.Succeeded {
			completed = int(op.Reservation.NewBytes)
		}
	case op.Purge != nil:
		// A recursive deletion has no manifest to count: the index settled at acceptance,
		// and what remains is a byte purge that walks the prefix. The counters are the
		// purge's own, and `total` is the extent of the walk it is currently on — null
		// only before the first walk has looked, never as a stand-in for zero. Reporting
		// 0/0 for the whole purge once took a live import down for 45 minutes of staring,
		// because next to `running` it is the same document as an operation that never
		// started.
		completed = int(op.Purge.Deleted + op.Purge.Adopted)
		unit = "objects"
		switch {
		case op.State == fileops.Succeeded:
			// A purge that stopped has settled everything it was going to settle, and what
			// it may not touch — a path the user re-created, a write newer than the
			// acceptance — is not work the operation still owes. Reporting the last walk's
			// extent here would end a finished deletion on "480 of 520", which its reader
			// cannot tell apart from a bug.
			total = completed
		case op.Purge.Total > 0:
			total = op.Purge.Total
		default:
			total = nil
		}
	}
	var result any
	if len(op.Result) > 0 {
		result = json.RawMessage(op.Result)
	}
	var problem any
	if len(op.Problem) > 0 {
		var p struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(op.Problem, &p) == nil {
			mapped := filesProblem(filesMutationProblem(fileops.E(p.Code, nil)))
			mapped.Detail = p.Detail
			mapped.RequestID = w.Header().Get("X-Request-Id")
			mapped.OperationID = op.ID
			problem = mapped
		}
	}
	// Only public workflow data crosses this boundary. Native checkpoints, intents,
	// guard paths and principal/root identities remain in the journal.
	return writeFilesJSON(w, status, map[string]any{"id": op.ID, "kind": op.Kind, "state": op.State, "progress": map[string]any{"completed": completed, "total": total, "unit": unit}, "result": result, "error": problem, "cancelRequested": op.CancelRequested, "retrying": retrying, "createdAt": op.CreatedAt.UTC(), "finishedAt": op.FinishedAt})
}
func filesOperationFailure(op fileops.Operation) error {
	var saved struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(op.Problem, &saved)
	if saved.Code == "" {
		saved.Code = "operation_failed"
	}
	if saved.Detail == "" {
		saved.Detail = "The file operation did not complete."
	}
	p := filesProblem(filesMutationProblem(fileops.E(saved.Code, nil)))
	p.Detail = saved.Detail
	p.OperationID = op.ID
	return p
}
func (s *Server) handleFilesGetOperation(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	native, root, actor, err := s.fileMutationContext(r.Context(), f)
	if err != nil {
		return err
	}
	op, err := s.filesJournal.Get(r.Context(), root, actor, filesOperationID(r))
	if err != nil {
		return filesMutationProblem(err)
	}
	if !op.Terminal() {
		if upload, e := s.filesJournal.GetUpload(r.Context(), root, actor, op.ID); e == nil {
			if upload.State != "open" {
				if e := s.queueFileOperation(op); e != nil && !errors.Is(e, errFilesWorkerBusy) {
					slog.Warn("file operation could not be queued", "operation", op.ID, "cause", errorCauseChain(e))
				}
			}
			return s.writeFilesOperation(w, op, 200, f)
		} else if !fileops.IsCode(e, "not_found") {
			return filesMutationProblem(e)
		}
		if op.Kind == "upload_commit" {
			service, e := s.filesPutRecovery(r, f, native)
			if e != nil {
				return e
			}
			updated, e := service.Recover(r.Context(), root, actor, op.ID, native)
			if e == nil {
				op = updated
			} else if !fileops.IsCode(e, "operation_in_progress") && !fileops.IsCode(e, "lease_lost") && !fileops.IsCode(e, "outcome_unknown") {
				slog.Warn("file operation inspection incomplete", "operation", op.ID, "code", fileOperationErrorCode(e), "cause", errorCauseChain(e))
			} else if updated.ID != "" {
				op = updated
			}
		} else if err = s.startFilesAction(r, f, op); err != nil && !errors.Is(err, errFilesWorkerBusy) {
			return err
		}
	}
	return s.writeFilesOperation(w, op, 200, f)
}

// writeFilesOperation attaches the retry record only while the operation is still
// nonterminal: a terminal operation that failed already explains itself through its
// Problem, and a stale record must not outlive it.
func (s *Server) writeFilesOperation(w http.ResponseWriter, op fileops.Operation, status int, scopes ...*fileRequest) error {
	var retrying map[string]any
	if !op.Terminal() {
		retrying = s.fileOperationRetry(op.ID)
	}
	return writeFilesOperationWithRetry(w, op, status, retrying, scopes...)
}
func (s *Server) handleFilesCancelOperation(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	_, root, actor, err := s.fileMutationContext(r.Context(), f)
	if err != nil {
		return err
	}
	op, err := s.filesJournal.RequestCancel(r.Context(), root, actor, filesOperationID(r))
	if err != nil {
		return filesMutationProblem(err)
	}
	if op.State == fileops.Succeeded {
		return fc.NewProblem(409, "already_committed", "The operation has already completed.")
	}
	if !op.Terminal() && op.Kind == "upload_commit" {
		if upload, e := s.filesJournal.GetUpload(r.Context(), root, actor, op.ID); e == nil {
			if upload.State == "open" {
				upload, e = s.filesJournal.AbortUpload(r.Context(), root, actor, op.ID)
				if e != nil {
					return filesMutationProblem(e)
				}
				op = upload.Operation
			}
			if e := s.queueFileOperation(op); e != nil && !errors.Is(e, errFilesWorkerBusy) {
				slog.Warn("file operation could not be queued", "operation", op.ID, "cause", errorCauseChain(e))
			}
		} else if !fileops.IsCode(e, "not_found") {
			return filesMutationProblem(e)
		}
	}
	if !op.Terminal() && op.Kind != "upload_commit" {
		if err = s.startFilesAction(r, f, op); err != nil && !errors.Is(err, errFilesWorkerBusy) {
			return err
		}
	}
	status := 200
	if !op.Terminal() {
		status = 202
	}
	return s.writeFilesOperation(w, op, status, f)
}

var errFilesWorkerBusy = errors.New("file operation workers are busy")

func (s *Server) startFilesAction(_ *http.Request, _ *fileRequest, op fileops.Operation) error {
	return s.queueFileOperation(op)
}
