package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/uploadspool"
)

const filesUploadPartSize int64 = 8 << 20
const filesUploadMaxParts = 10000
const filesUploadParallelism = 4

// FileUploadSpool stores private durable chunks independently of final publication.
type FileUploadSpool interface {
	Receive(context.Context, string, int, int64, *storage.Digest, io.Reader, func(uploadspool.Part) (uploadspool.Part, error)) (uploadspool.Part, error)
	Open(context.Context, string, []uploadspool.Part) (io.ReadCloser, error)
	Discard(context.Context, string) error
	Close() error
}
type fileUploadSpoolFactory func(storage.Config) (FileUploadSpool, error)

func filesUploadID(r *http.Request) string {
	return strings.Split(strings.TrimPrefix(r.URL.Path, "/api/files/uploads/"), "/")[0]
}
func filesUploadStateError(state fileops.UploadState) error {
	switch state {
	case "aborted":
		return fileops.E("upload_aborted", nil)
	case "expired":
		return fileops.E("upload_expired", nil)
	case "failed":
		return fileops.E("upload_failed", nil)
	}
	return nil
}
func filesUploadResource(u fileops.Upload) map[string]any {
	var intent storage.WriteIntent
	_ = json.Unmarshal(u.Operation.Intent, &intent)
	var result any
	if len(u.Operation.Result) > 0 {
		result = json.RawMessage(u.Operation.Result)
	}
	return map[string]any{"id": u.ID, "path": "/" + string(intent.Target), "state": u.State, "size": intent.Size, "partSize": u.PartSize, "partCount": u.PartCount, "maxParallelism": u.MaxParallelism, "ordering": "any", "nextPart": nil, "receivedParts": u.ReceivedParts, "receivedBytes": u.ReceivedBytes, "createdAt": u.CreatedAt.UTC(), "expiresAt": u.ExpiresAt.UTC(), "partsUrl": "/api/files/uploads/" + u.ID + "/parts", "commitUrl": "/api/files/uploads/" + u.ID + "/commit", "operationId": u.Operation.ID, "result": result}
}
func filesUploadReceipt(p fileops.PartReceipt) map[string]any {
	return map[string]any{"number": p.Number, "size": p.Size, "sha256": p.SHA256}
}
func filesUploadMaxSize(native fileops.NativeActions) int64 {
	max := int64(filesUploadMaxParts) * filesUploadPartSize
	if limit := native.Describe().MaxFileBytes; limit > 0 && limit < max {
		max = limit
	}
	return max
}
func (s *Server) handleFilesCreateUpload(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	var req struct {
		Path             string     `json:"path"`
		Size             *int64     `json:"size"`
		MediaType        string     `json:"mediaType"`
		SourceModifiedAt *time.Time `json:"sourceModifiedAt"`
		Condition        struct {
			Mode            string `json:"mode"`
			ContentRevision string `json:"contentRevision"`
		} `json:"condition"`
	}
	if err := strictFilesJSON(r, &req); err != nil {
		return err
	}
	p, err := fc.ParsePath(req.Path)
	if err != nil {
		return err
	}
	if p == "/" {
		return fc.NewProblem(400, "invalid_path", "The root cannot contain file content.")
	}
	if err = f.RequireWrite(p); err != nil {
		return err
	}
	if req.Size == nil || *req.Size < 0 || *req.Size > filesMaxJSONBytes {
		return fc.NewProblem(400, "invalid_request", "Size must be a nonnegative safe integer.")
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 1024 || strings.ContainsAny(key, "\r\n\x00") {
		return fc.NewProblem(400, "invalid_request", "A valid Idempotency-Key is required.")
	}
	if req.MediaType == "" {
		req.MediaType = "application/octet-stream"
	}
	if _, _, err = mime.ParseMediaType(req.MediaType); err != nil || len(req.MediaType) > 1024 {
		return fc.NewProblem(400, "invalid_request", "Media type is invalid.")
	}
	condition := fc.PutCondition{Mode: fc.PutCreate}
	switch req.Condition.Mode {
	case "", "create":
		if req.Condition.ContentRevision != "" {
			return fc.NewProblem(400, "invalid_request", "Create does not accept a content revision.")
		}
	case "replace":
		if req.Condition.ContentRevision == "" {
			return fc.NewProblem(428, "precondition_required", "Replacement requires a content revision.")
		}
		h := make(http.Header)
		h.Set("If-Match", strconv.Quote(req.Condition.ContentRevision))
		condition, err = fc.ParsePutCondition(h)
		if err != nil {
			return err
		}
	default:
		return fc.NewProblem(400, "invalid_request", "The upload condition is invalid.")
	}
	native, root, actor, err := s.fileMutationContext(r.Context(), f)
	if err != nil {
		return err
	}
	if *req.Size > filesUploadMaxSize(native) {
		return fc.NewProblem(413, "file_too_large", "The file exceeds the admitted publication or part-count limit.")
	}
	if root.RetainsVersions && req.SourceModifiedAt != nil {
		return fc.NewProblem(422, "unsupported_option", "This object route cannot set native source modification time.")
	}
	if req.SourceModifiedAt != nil {
		v := req.SourceModifiedAt.UTC()
		req.SourceModifiedAt = &v
	}
	client := storage.WriteIntent{Target: storage.Path(p.Relative()), Size: *req.Size, MediaType: req.MediaType, SourceModifiedAt: req.SourceModifiedAt, ClientCondition: filesPutClientCondition(condition)}
	if old, found, e := s.filesJournal.ReplayUpload(r.Context(), root, actor, key, client); e != nil {
		return filesMutationProblem(e)
	} else if found {
		w.Header().Set("Location", "/api/files/uploads/"+old.ID)
		return writeFilesJSON(w, 200, filesUploadResource(old))
	}
	intent := client
	parentName := path.Dir(p.Relative())
	if parentName == "." {
		parentName = ""
	}
	intent.ParentPath = storage.Path(parentName)
	parent, err := native.Stat(r.Context(), intent.ParentPath)
	if storage.IsCode(err, "not_found") {
		return fc.NewProblem(409, "parent_missing", "The destination parent does not exist.")
	}
	if err != nil {
		return filesMutationProblem(err)
	}
	if parent.Kind != storage.Folder {
		return fc.NewProblem(409, "not_a_folder", "The destination parent is not a folder.")
	}
	intent.ParentVersion = parent.Version
	var oldBytes int64
	existing, err := native.Stat(r.Context(), intent.Target)
	if err != nil && !storage.IsCode(err, "not_found") {
		return filesMutationProblem(err)
	}
	if err == nil {
		if existing.Kind != storage.File {
			return fc.NewProblem(409, "name_conflict", "A folder occupies the destination.")
		}
		revision, e := s.filesObservedContentRevision(r.Context(), f, existing)
		if e != nil {
			return e
		}
		if e = fc.CheckPutCondition(condition, revision); e != nil {
			return e
		}
		intent.Conditions.ExpectedVersion = existing.Version
		oldBytes = existing.Size
	} else {
		if condition.Mode == fc.PutReplace {
			return fc.NewProblem(412, "target_changed", "The destination no longer exists.")
		}
		intent.Conditions.Absent = true
	}
	physical, err := fileops.PhysicalReservation(root, intent.Size)
	if err != nil {
		return filesMutationProblem(err)
	}
	// Chunks coexist with the assembled stage; bounded replay attempts need headroom.
	physical += intent.Size + filesUploadParallelism*min(intent.Size, filesUploadPartSize)
	reservation := fileops.ReservationRequest{NewBytes: intent.Size, OldBytes: oldBytes, PhysicalBytes: physical}
	if oldBytes > 0 {
		reservation.CreditPath = intent.Target
		reservation.CreditVersion = intent.Conditions.ExpectedVersion
	}
	encoded, _ := json.Marshal(intent)
	u, _, err := s.filesJournal.CreateUpload(r.Context(), fileops.BeginRequest{Root: root, Actor: actor, Kind: "upload_commit", IdempotencyKey: key, Intent: encoded, Guards: []fileops.Guard{{Path: intent.Target}}, Reservation: reservation}, fileops.UploadOptions{PartSize: filesUploadPartSize, MaxParallelism: filesUploadParallelism, ClientIntent: client})
	if err != nil {
		return filesMutationProblem(err)
	}
	w.Header().Set("Location", "/api/files/uploads/"+u.ID)
	return writeFilesJSON(w, 201, filesUploadResource(u))
}
func (s *Server) filesUploadContext(r *http.Request, f *fileRequest) (fileops.NativeActions, fileops.Root, fileops.Actor, fileops.Upload, error) {
	native, root, actor, err := s.fileMutationContext(r.Context(), f)
	if err != nil {
		return native, root, actor, fileops.Upload{}, err
	}
	u, err := s.filesJournal.GetUpload(r.Context(), root, actor, filesUploadID(r))
	if err != nil {
		return native, root, actor, u, filesMutationProblem(err)
	}
	return native, root, actor, u, nil
}
func (s *Server) handleFilesGetUpload(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	_, _, _, u, err := s.filesUploadContext(r, f)
	if err != nil {
		return err
	}
	return writeFilesJSON(w, 200, filesUploadResource(u))
}
func (s *Server) handleFilesListUploads(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	_, root, actor, err := s.fileMutationContext(r.Context(), f)
	if err != nil {
		return err
	}
	q, err := parseFilesReadQuery(r, f.Scope, false)
	if err != nil {
		return err
	}
	binding := s.filesCursorBinding(f, q, "uploads")
	key, err := s.filesDecodeCursor(q.Cursor, binding)
	if err != nil {
		return err
	}
	after := ""
	if key != nil {
		after = key.Path
	}
	items, err := s.filesJournal.ListUploads(r.Context(), root, actor, q.Limit+1, after)
	if err != nil {
		return filesMutationProblem(err)
	}
	var next any
	if len(items) > q.Limit {
		items = items[:q.Limit]
		next, err = s.filesEncodeCursor(filesSortKey{Path: items[len(items)-1].ID, Name: items[len(items)-1].ID}, binding)
		if err != nil {
			return err
		}
	}
	resources := make([]any, 0, len(items))
	for _, u := range items {
		resources = append(resources, filesUploadResource(u))
	}
	return writeFilesJSON(w, 200, map[string]any{"items": resources, "nextCursor": next})
}
func (s *Server) handleFilesUploadPart(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	native, root, actor, u, err := s.filesUploadContext(r, f)
	if err != nil {
		return err
	}
	if err = filesUploadStateError(u.State); err != nil {
		return filesMutationProblem(err)
	}
	if u.State != "open" {
		return fc.NewProblem(409, "commit_in_progress", "The part manifest is frozen.")
	}
	raw := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	number, err := strconv.Atoi(raw)
	if err != nil || number < 1 || number > u.PartCount || strconv.Itoa(number) != raw {
		return fc.NewProblem(400, "invalid_request", "The part number is outside the session geometry.")
	}
	expected := u.PartSize
	var intent storage.WriteIntent
	_ = json.Unmarshal(u.Operation.Intent, &intent)
	if number == u.PartCount {
		expected = intent.Size - int64(number-1)*u.PartSize
	}
	if r.ContentLength < 0 {
		return fc.NewProblem(411, "length_required", "Content-Length is required.")
	}
	if r.ContentLength != expected {
		return fc.NewProblem(400, "length_mismatch", "Content-Length must match the assigned part length.")
	}
	digest, err := filesDigest(r.Header.Get("Content-Digest"))
	if err != nil {
		return err
	}
	spool, err := s.uploadSpools(native.StorageBinding())
	if err != nil {
		return err
	}
	defer spool.Close()
	var receipt fileops.PartReceipt
	var replay bool
	_, err = spool.Receive(r.Context(), u.ID, number, expected, digest, r.Body, func(part uploadspool.Part) (uploadspool.Part, error) {
		var e error
		receipt, replay, e = s.filesJournal.AcceptUploadPart(r.Context(), root, actor, u.ID, fileops.PartReceipt{Number: part.Number, Size: part.Size, SHA256: part.SHA256, Handle: part.Handle})
		if e != nil {
			var typed *fileops.Error
			if errors.As(e, &typed) {
				return part, uploadspool.Reject(e)
			}
			return part, e
		}
		return uploadspool.Part{Number: receipt.Number, Size: receipt.Size, SHA256: receipt.SHA256, Handle: receipt.Handle}, nil
	})
	if err != nil {
		return filesUploadSpoolProblem(err)
	}
	status := 201
	if replay {
		status = 200
	}
	return writeFilesJSON(w, status, filesUploadReceipt(receipt))
}
func filesUploadSpoolProblem(err error) error {
	switch {
	case storage.IsCode(err, "digest_mismatch"):
		return fc.NewProblem(422, "checksum_mismatch", "The part digest did not match its bytes.")
	case storage.IsCode(err, "size_mismatch"):
		return fc.NewProblem(400, "length_mismatch", "The body must match the assigned part length.")
	case errors.Is(err, uploadspool.ErrBusy):
		return fc.NewProblem(429, "rate_limited", "The session has reached its concurrent byte-request limit.")
	case errors.Is(err, uploadspool.ErrDiscarded):
		return fc.NewProblem(410, "upload_aborted", "The upload no longer accepts bytes.")
	case errors.Is(err, uploadspool.ErrConflict):
		return fc.NewProblem(409, "part_conflict", "The part number is already bound to different bytes.")
	case errors.Is(err, uploadspool.ErrIntegrity):
		return fc.NewProblem(503, "staging_failure", "The durable chunk could not be verified.")
	}
	return filesMutationProblem(err)
}
func (s *Server) handleFilesUploadParts(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	_, root, actor, u, err := s.filesUploadContext(r, f)
	if err != nil {
		return err
	}
	q, err := parseFilesReadQuery(r, f.Scope, false)
	if err != nil {
		return err
	}
	binding := s.filesCursorBinding(f, q, "upload-parts:"+u.ID)
	key, err := s.filesDecodeCursor(q.Cursor, binding)
	if err != nil {
		return err
	}
	var after, watermark int64
	if key != nil {
		after, err = strconv.ParseInt(path.Dir(key.Path), 10, 64)
		if err != nil {
			return fc.NewProblem(400, "invalid_request", "Invalid receipt cursor.")
		}
		watermark, err = strconv.ParseInt(key.Name, 10, 64)
		if err != nil {
			return fc.NewProblem(400, "invalid_request", "Invalid receipt watermark.")
		}
	}
	parts, watermark, err := s.filesJournal.ListUploadParts(r.Context(), root, actor, u.ID, q.Limit+1, after, watermark)
	if err != nil {
		return filesMutationProblem(err)
	}
	var next any
	if len(parts) > q.Limit {
		parts = parts[:q.Limit]
		next, err = s.filesEncodeCursor(filesSortKey{Path: strconv.FormatInt(parts[len(parts)-1].Sequence, 10) + "/" + strconv.FormatInt(watermark, 10), Name: strconv.FormatInt(watermark, 10)}, binding)
		if err != nil {
			return err
		}
	}
	resources := make([]any, 0, len(parts))
	for _, p := range parts {
		resources = append(resources, filesUploadReceipt(p))
	}
	return writeFilesJSON(w, 200, map[string]any{"items": resources, "nextCursor": next, "watermark": watermark})
}
func (s *Server) handleFilesCommitUpload(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	_, root, actor, u, err := s.filesUploadContext(r, f)
	if err != nil {
		return err
	}
	u, err = s.filesJournal.FreezeUpload(r.Context(), root, actor, u.ID)
	if err != nil {
		return filesMutationProblem(err)
	}
	if u.Operation.State == fileops.Succeeded {
		return writeFilesJSON(w, 200, json.RawMessage(u.Operation.Result))
	}
	if u.Operation.Terminal() {
		return filesOperationFailure(u.Operation)
	}
	if err = s.queueFileOperation(u.Operation); err != nil && !errors.Is(err, errFilesWorkerBusy) {
		return err
	}
	return writeFilesOperation(w, u.Operation, 202)
}
func (s *Server) handleFilesAbortUpload(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	_, root, actor, u, err := s.filesUploadContext(r, f)
	if err != nil {
		return err
	}
	u, err = s.filesJournal.AbortUpload(r.Context(), root, actor, u.ID)
	if err != nil {
		return filesMutationProblem(err)
	}
	_ = s.queueFileOperation(u.Operation)
	w.WriteHeader(204)
	return nil
}

// Cleanup retains physical liability until chunks and the P3/P4 stage are gone.
type uploadChunkWriter struct {
	fileops.NativeActions
	spool FileUploadSpool
	id    string
}

func (w *uploadChunkWriter) Stage(ctx context.Context, intent storage.WriteIntent, body io.Reader, checkpoint storage.Checkpoint) (stage storage.Staged, err error) {
	// Native Stage may reject before reading EOF. Release this consumer before
	// Service invokes Discard; other late receivers remain independently fenced.
	if closer, ok := body.(io.ReadCloser); ok {
		defer func() { err = errors.Join(err, closer.Close()) }()
	}
	return w.NativeActions.Stage(ctx, intent, body, checkpoint)
}
func (w *uploadChunkWriter) Discard(ctx context.Context, intent storage.WriteIntent, checkpoint []byte) error {
	if err := w.spool.Discard(ctx, w.id); err != nil {
		return err
	}
	return w.NativeActions.Discard(ctx, intent, checkpoint)
}
func (s *Server) runUploadOperation(ctx context.Context, op fileops.Operation, actor fileops.Actor, native fileops.NativeActions, service *fileops.Service) (fileops.Operation, error) {
	u, err := s.filesJournal.GetUpload(ctx, op.Root, actor, op.ID)
	if err != nil {
		return op, err
	}
	if u.State == "open" {
		return op, fileops.E("commit_not_started", nil)
	}
	spool, err := s.uploadSpools(native.StorageBinding())
	if err != nil {
		return op, err
	}
	defer spool.Close()
	writer := &uploadChunkWriter{NativeActions: native, spool: spool, id: op.ID}
	if !op.Terminal() {
		if op.Phase == fileops.Admitted && !op.CancelRequested {
			receipts := make([]fileops.PartReceipt, 0, u.PartCount)
			var after, watermark int64
			for {
				page, mark, e := s.filesJournal.ListUploadParts(ctx, op.Root, actor, op.ID, 1000, after, watermark)
				if e != nil {
					return op, e
				}
				watermark = mark
				receipts = append(receipts, page...)
				if len(receipts) > filesUploadMaxParts {
					return op, fileops.E("invalid_intent", nil)
				}
				if len(page) < 1000 {
					break
				}
				after = page[len(page)-1].Sequence
			}
			if len(receipts) != u.PartCount {
				return op, fileops.E("upload_incomplete", nil)
			}
			sort.Slice(receipts, func(i, j int) bool { return receipts[i].Number < receipts[j].Number })
			parts := make([]uploadspool.Part, 0, len(receipts))
			for _, p := range receipts {
				parts = append(parts, uploadspool.Part{Number: p.Number, Size: p.Size, SHA256: p.SHA256, Handle: p.Handle})
			}
			body, e := spool.Open(ctx, op.ID, parts)
			if e != nil {
				if errors.Is(e, uploadspool.ErrDiscarded) {
					// Cleanup may have fenced chunks immediately before a crash.
					// Inspect the original native intent instead of reopening chunks.
					op, err = service.Recover(ctx, op.Root, actor, op.ID, writer)
				} else {
					return op, e
				}
			} else {
				op, err = service.Write(ctx, op.Root, actor, op.ID, writer, body)
				_ = body.Close()
			}
		} else if op.Phase == fileops.StagedReady && op.State != fileops.Unknown {
			op, err = service.Write(ctx, op.Root, actor, op.ID, writer, nil)
		} else {
			op, err = service.Recover(ctx, op.Root, actor, op.ID, writer)
		}
	}
	if fileops.IsCode(err, "forbidden") || fileops.IsCode(err, "target_changed") || fileops.IsCode(err, "precondition_failed") {
		latest, e := s.filesJournal.Get(ctx, op.Root, actor, op.ID)
		if e == nil && (latest.Phase == fileops.Admitted || latest.Phase == fileops.Receiving || latest.Phase == fileops.StagedReady) {
			if _, e = s.filesJournal.RequestCancel(ctx, op.Root, actor, op.ID); e == nil {
				op, err = service.Recover(ctx, op.Root, actor, op.ID, writer)
			}
		}
	}
	if err == nil && op.Terminal() {
		err = service.Cleanup(ctx, op.Root, actor, op.ID, writer)
	}
	return op, err
}
