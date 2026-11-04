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
	if op.Kind == "upload_commit" {
		unit = "bytes"
		total = op.Reservation.NewBytes
		if op.State == fileops.Succeeded {
			completed = int(op.Reservation.NewBytes)
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
	return writeFilesJSON(w, status, map[string]any{"id": op.ID, "kind": op.Kind, "state": op.State, "progress": map[string]any{"completed": completed, "total": total, "unit": unit}, "result": result, "error": problem, "cancelRequested": op.CancelRequested, "createdAt": op.CreatedAt.UTC(), "finishedAt": op.FinishedAt})
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
				_ = s.queueFileOperation(op)
			}
			return writeFilesOperation(w, op, 200, f)
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
				slog.Warn("file operation inspection incomplete", "operation", op.ID, "error", e)
			} else if updated.ID != "" {
				op = updated
			}
		} else if err = s.startFilesAction(r, f, op); err != nil && !errors.Is(err, errFilesWorkerBusy) {
			return err
		}
	}
	return writeFilesOperation(w, op, 200, f)
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
			_ = s.queueFileOperation(op)
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
	return writeFilesOperation(w, op, status, f)
}

var errFilesWorkerBusy = errors.New("file operation workers are busy")

func (s *Server) startFilesAction(_ *http.Request, _ *fileRequest, op fileops.Operation) error {
	return s.queueFileOperation(op)
}
