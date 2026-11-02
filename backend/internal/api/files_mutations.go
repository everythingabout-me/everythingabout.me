package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storage"
)

func filesMutationProblem(err error) error {
	var p *fc.Problem
	if errors.As(err, &p) {
		return err
	}
	var e *fileops.Error
	if errors.As(err, &e) {
		status, code := 409, e.Code
		switch e.Code {
		case "forbidden":
			status = 403
		case "not_found":
			status = 404
		case "precondition_required":
			status = 428
		case "target_changed", "precondition_failed", "changed":
			status = 412
			code = "target_changed"
		case "invalid_request", "invalid_path", "invalid_name", "invalid_intent":
			status = 400
		case "checksum_mismatch":
			status = 422
		case "length_mismatch", "body_reception_failed":
			status = 400
		case "quota_exceeded", "staging_full":
			status = 507
		case "unsupported", "unbound_writer":
			status = 422
			code = "unsupported_option"
		case "baseline_required", "root_conflict", "lease_lost", "recovery_required", "outcome_unknown":
			status = 503
		case "not_empty":
			code = "directory_not_empty"
		}
		return fc.NewProblem(status, code, "The file operation could not complete: "+code+".")
	}
	var n *storage.Error
	if errors.As(err, &n) {
		switch n.Code {
		case "parent_missing":
			return fc.NewProblem(409, "parent_missing", "The destination parent does not exist.")
		case "not_empty":
			return fc.NewProblem(409, "directory_not_empty", "The directory is not empty.")
		case "changed":
			return fc.NewProblem(412, "target_changed", "The captured native path changed.")
		case "name_conflict":
			return fc.NewProblem(409, "name_conflict", "The destination already exists.")
		}
	}
	return err
}
func (s *Server) fileMutationContext(ctx context.Context, f *fileRequest) (fileops.NativeActions, fileops.Root, fileops.Actor, error) {
	if !s.cfg.Storage.CoordinatedWrites || s.filesJournal == nil {
		return nil, fileops.Root{}, fileops.Actor{}, fc.NewProblem(501, "not_implemented", "Native file writes are not enabled.")
	}
	native, ok := f.Driver.(fileops.NativeActions)
	if !ok {
		return nil, fileops.Root{}, fileops.Actor{}, fc.NewProblem(422, "unsupported_option", "This root does not support safe native file mutations.")
	}
	binding := native.StorageBinding()
	root := fileops.Root{OwnerID: binding.OwnerID, ConnectionID: binding.ConnectionID, Generation: binding.Generation, ContentRoot: binding.ContentRoot, SystemRoot: binding.SystemRoot, CaseSensitive: native.StorageNameRules().CaseSensitive}
	actor := fileops.Actor{ID: filePrincipalID(f.Principal), Scope: storage.Path(f.Scope.Relative()), CanRead: f.Principal.Token == nil || f.Principal.Token.CanRead, CanWrite: f.Principal.Token == nil || f.Principal.Token.CanWrite, ManageOwner: f.Principal.Token == nil}
	// This internal readiness query does not disclose owner-wide usage to a token.
	_, err := s.filesJournal.Usage(ctx, root, fileops.Actor{ID: "owner:" + root.OwnerID, CanRead: true, CanWrite: true, ManageOwner: true})
	if err != nil {
		return nil, root, actor, filesMutationProblem(err)
	}
	return native, root, actor, nil
}
func (s *Server) filesMutationAuthorize(r *http.Request, f *fileRequest, native fileops.NativeActions) fileops.Authorize {
	captured := r.Clone(context.WithoutCancel(r.Context()))
	captured.Header = r.Header.Clone()
	return func(ctx context.Context, op fileops.Operation) error {
		if s.auth != nil {
			if err := s.auth.AuthorizeFileOperation(ctx, op); err != nil {
				return err
			}
		}
		verifier := s.filesAuth
		if verifier == nil {
			verifier = s.auth
		}
		if verifier == nil {
			return fc.NewProblem(401, "unauthenticated", "A current credential is required.")
		}
		principal, source, err := verifier.AuthenticateFiles(ctx, captured.WithContext(ctx))
		if err != nil {
			return err
		}
		if principal == nil || principal.User == nil || principal.User.ID != op.Root.OwnerID || source != f.Source || filePrincipalID(principal) != filePrincipalID(f.Principal) {
			return fc.NewProblem(401, "unauthenticated", "The credential changed before the native effect.")
		}
		scope := fc.Path("/")
		if principal.Token != nil {
			scope, err = fc.FromRelative(principal.Token.Prefix)
			if err != nil {
				return err
			}
		}
		current := *f
		current.Principal = principal
		current.Scope = scope
		current.Driver = native
		for _, g := range op.Guards {
			p, e := fc.FromRelative(string(g.Path))
			if e != nil {
				return e
			}
			if e = current.RequireWrite(p); e != nil {
				return e
			}
		}
		if op.Kind == "upload_commit" && !op.Terminal() {
			var intent storage.WriteIntent
			if json.Unmarshal(op.Intent, &intent) != nil {
				return fileops.E("invalid_intent", nil)
			}
			parent, err := native.Stat(ctx, intent.ParentPath)
			if err != nil {
				return filesMutationProblem(err)
			}
			if parent.Kind != storage.Folder || !bytes.Equal(parent.Version, intent.ParentVersion) {
				return fc.NewProblem(412, "target_changed", "The destination parent changed.")
			}
		}
		return nil
	}
}
func strictFilesJSON(r *http.Request, out any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil {
		return fc.NewProblem(400, "invalid_request", "The JSON body could not be read.")
	}
	if len(data) > 1<<20 {
		return fc.NewProblem(413, "request_too_large", "The JSON request exceeds the supported size.")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return fc.NewProblem(400, "invalid_request", "The JSON request is invalid.")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fc.NewProblem(400, "invalid_request", "Exactly one JSON object is required.")
	}
	return nil
}
func filesDigest(value string) (*storage.Digest, error) {
	if value == "" {
		return nil, nil
	}
	if !strings.HasPrefix(value, "sha-256=:") || !strings.HasSuffix(value, ":") {
		return nil, fc.NewProblem(400, "invalid_request", "Content-Digest must contain one SHA-256 value.")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(value, "sha-256=:"), ":"))
	if err != nil || len(decoded) != sha256.Size {
		return nil, fc.NewProblem(400, "invalid_request", "Content-Digest is invalid.")
	}
	return &storage.Digest{Algorithm: "sha-256", Value: decoded}, nil
}
func filesPutClientCondition(c fc.PutCondition) string {
	if c.Mode == fc.PutReplace {
		return "replace:" + c.ContentRevision
	}
	if c.ExplicitCreate {
		return "create:explicit"
	}
	return "create:default"
}
func (s *Server) handleFilesPut(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if !s.cfg.Storage.CoordinatedWrites {
		return fc.NewProblem(501, "not_implemented", "Native file writes are not enabled.")
	}
	p, err := fc.ParseQueryPath(r.URL.RawQuery)
	if err != nil {
		return err
	}
	if p == "/" {
		return fc.NewProblem(400, "invalid_path", "The root cannot contain file content.")
	}
	if err = f.RequireWrite(p); err != nil {
		return err
	}
	if r.ContentLength < 0 {
		return fc.NewProblem(411, "length_required", "Content-Length is required.")
	}
	limit := s.cfg.Storage.MaxSinglePutBytes
	if limit == 0 {
		limit = 32 << 20
	}
	if r.ContentLength > limit || r.ContentLength > 9007199254740991 {
		return fc.NewProblem(413, "request_too_large", "Use a resumable upload for content above the single-PUT limit.")
	}
	condition, err := fc.ParsePutCondition(r.Header)
	if err != nil {
		return err
	}
	digest, err := filesDigest(r.Header.Get("Content-Digest"))
	if err != nil {
		return err
	}
	media := r.Header.Get("Content-Type")
	if media == "" {
		media = "application/octet-stream"
	}
	if _, _, err = mime.ParseMediaType(media); err != nil || len(media) > 1024 {
		return fc.NewProblem(400, "invalid_request", "Content-Type is invalid.")
	}
	var modified *time.Time
	if raw := r.Header.Get("X-Source-Modified-At"); raw != "" {
		v, e := time.Parse(time.RFC3339Nano, raw)
		if e != nil {
			return fc.NewProblem(400, "invalid_request", "X-Source-Modified-At must be RFC 3339.")
		}
		v = v.UTC()
		modified = &v
	}
	native, root, actor, err := s.fileMutationContext(r.Context(), f)
	if err != nil {
		return err
	}
	intent := storage.WriteIntent{Target: storage.Path(p.Relative()), Size: r.ContentLength, MediaType: media, SourceModifiedAt: modified, Digest: digest, ClientCondition: filesPutClientCondition(condition)}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 1024 || strings.ContainsAny(key, "\r\n\x00") {
		return fc.NewProblem(400, "invalid_request", "Idempotency-Key is invalid.")
	}
	service, err := s.filesPutRecovery(r, f, native)
	if err != nil {
		return err
	}
	replayWrite := func() (bool, error) {
		if key == "" {
			return false, nil
		}
		old, found, e := s.filesJournal.ReplayWrite(r.Context(), root, actor, key, intent)
		if e != nil {
			return true, filesMutationProblem(e)
		}
		if !found {
			return false, nil
		}
		if e = filesValidatePutReplay(r.Context(), r.Body, intent, old.Stage); e != nil {
			return true, filesMutationProblem(e)
		}
		saved, e := service.Write(r.Context(), root, actor, old.ID, native, nil)
		s.filesCleanupTerminal(r.Context(), service, native, root, actor, saved)
		return true, s.filesPutResponse(w, r, saved, e, condition.Mode == fc.PutReplace)
	}
	if found, e := replayWrite(); found {
		return e
	}
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
	intent.ParentVersion = bytes.Clone(parent.Version)
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
			if found, replayErr := replayWrite(); found {
				return replayErr
			}
			return e
		}
		intent.Conditions.ExpectedVersion = bytes.Clone(existing.Version)
		oldBytes = existing.Size
	} else {
		if condition.Mode == fc.PutReplace {
			return fc.NewProblem(412, "target_changed", "The destination no longer exists.")
		}
		intent.Conditions.Absent = true
	}
	encoded, _ := json.Marshal(intent)
	op, _, err := s.filesJournal.Begin(r.Context(), fileops.BeginRequest{Root: root, Actor: actor, Kind: "upload_commit", IdempotencyKey: key, Intent: encoded, Guards: []fileops.Guard{{Path: actor.Scope, Subtree: true}}, Reservation: fileops.ReservationRequest{NewBytes: intent.Size, OldBytes: oldBytes, PhysicalBytes: intent.Size, CreditPath: func() storage.Path {
		if oldBytes > 0 {
			return intent.Target
		}
		return ""
	}(), CreditVersion: func() []byte {
		if oldBytes > 0 {
			return intent.Conditions.ExpectedVersion
		}
		return nil
	}()}})
	if err != nil {
		if found, replayErr := replayWrite(); found {
			return replayErr
		}
		return filesMutationProblem(err)
	}
	saved, err := service.Write(r.Context(), root, actor, op.ID, native, r.Body)
	s.filesCleanupTerminal(r.Context(), service, native, root, actor, saved)
	return s.filesPutResponse(w, r, saved, err, condition.Mode == fc.PutReplace)
}
func (s *Server) filesCleanupTerminal(ctx context.Context, service *fileops.Service, native fileops.NativeActions, root fileops.Root, actor fileops.Actor, op fileops.Operation) {
	if !op.Terminal() || op.ID == "" {
		return
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := service.Cleanup(cleanup, root, actor, op.ID, native); err != nil {
		_ = s.queueFileOperation(op)
	}
}

func filesValidatePutReplay(ctx context.Context, body io.Reader, intent storage.WriteIntent, stage *storage.Staged) error {
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(body, intent.Size+1))
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if n != intent.Size {
		return fileops.E("length_mismatch", nil)
	}
	expected := intent.Digest
	if stage != nil && stage.Digest != nil {
		expected = stage.Digest
	}
	if expected == nil || expected.Algorithm != "sha-256" || !bytes.Equal(expected.Value, h.Sum(nil)) {
		return fileops.E("idempotency_conflict", nil)
	}
	return nil
}

func (s *Server) filesPutResponse(w http.ResponseWriter, r *http.Request, op fileops.Operation, err error, replaced bool) error {
	if op.ID != "" {
		w.Header().Set("Location", "/api/files/operations/"+op.ID)
	}
	if err != nil {
		p := filesProblem(filesMutationProblem(err))
		copy := *p
		if op.ID != "" {
			copy.OperationID = op.ID
			w.Header().Set("Retry-After", "2")
		}
		return &copy
	}
	if !op.Terminal() {
		p := fc.NewProblem(503, "outcome_unknown", "The native publication is still being resolved.")
		p.OperationID = op.ID
		w.Header().Set("Retry-After", "2")
		return p
	}
	if op.State != fileops.Succeeded {
		return filesOperationFailure(op)
	}
	if len(op.Result) == 0 {
		return fc.NewProblem(503, "temporarily_unavailable", "The native result has not been settled.")
	}
	status := 201
	if replaced {
		status = 200
	}
	return writeFilesJSON(w, status, json.RawMessage(op.Result))
}
func randomFilesKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "request-" + hex.EncodeToString(b[:])
}
func (s *Server) filesActions(r *http.Request, f *fileRequest, native fileops.NativeActions) (*fileops.Actions, error) {
	return fileops.NewActions(s.filesJournal, fileops.ActionsOptions{Authorize: s.filesMutationAuthorize(r, f, native), Commit: nodes.CommitFileAction, Validate: func(ctx context.Context, n fileops.NativeActions, req fileops.ActionRequest) error {
		if req.IfRevision == "" {
			return nil
		}
		entry, err := n.Stat(ctx, req.From)
		if err != nil {
			return err
		}
		current := *f
		current.Driver = n
		resource, err := s.filesMetadata(ctx, &current, entry, nil)
		if err != nil {
			return err
		}
		return fc.CheckRevision(req.IfRevision, resource.Revision)
	}})
}
func (s *Server) handleFilesMkdir(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if !s.cfg.Storage.CoordinatedWrites {
		return fc.NewProblem(501, "not_implemented", "Native file writes are not enabled.")
	}
	var request struct {
		Kind    string `json:"kind"`
		Path    string `json:"path"`
		Parents bool   `json:"parents"`
	}
	if err := strictFilesJSON(r, &request); err != nil {
		return err
	}
	if request.Kind != "folder" {
		return fc.NewProblem(400, "invalid_request", "Folder creation requires kind folder.")
	}
	p, err := fc.ParsePath(request.Path)
	if err != nil {
		return err
	}
	if err = f.RequireWrite(p); err != nil {
		return err
	}
	return s.filesAction(w, r, f, fileops.ActionRequest{Kind: "mkdir", To: storage.Path(p.Relative()), Parents: request.Parents})
}
func (s *Server) filesMoveCopy(w http.ResponseWriter, r *http.Request, f *fileRequest, kind string) error {
	if !s.cfg.Storage.CoordinatedWrites {
		return fc.NewProblem(501, "not_implemented", "Native file writes are not enabled.")
	}
	var request struct {
		From             string `json:"from"`
		To               string `json:"to"`
		IfRevision       string `json:"ifRevision"`
		IfSourceRevision string `json:"ifSourceRevision"`
	}
	if err := strictFilesJSON(r, &request); err != nil {
		return err
	}
	from, err := fc.ParsePath(request.From)
	if err != nil {
		return err
	}
	to, err := fc.ParsePath(request.To)
	if err != nil {
		return err
	}
	if err = f.RequireRead(from); err != nil {
		return err
	}
	if err = f.RequireWrite(from); err != nil && kind == "move" {
		return err
	}
	if err = f.RequireWrite(to); err != nil {
		return err
	}
	revision := request.IfRevision
	if kind == "copy" {
		revision = request.IfSourceRevision
	}
	if revision == "" {
		return fc.NewProblem(428, "precondition_required", "The source metadata revision is required.")
	}
	return s.filesAction(w, r, f, fileops.ActionRequest{Kind: kind, From: storage.Path(from.Relative()), To: storage.Path(to.Relative()), IfRevision: revision})
}
func (s *Server) handleFilesMove(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	return s.filesMoveCopy(w, r, f, "move")
}
func (s *Server) handleFilesCopy(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	return s.filesMoveCopy(w, r, f, "copy")
}
func (s *Server) handleFilesDelete(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if !s.cfg.Storage.CoordinatedWrites {
		return fc.NewProblem(501, "not_implemented", "Native file writes are not enabled.")
	}
	p, err := fc.ParseQueryPath(r.URL.RawQuery)
	if err != nil {
		return err
	}
	if err = f.RequireWrite(p); err != nil {
		return err
	}
	recursive := false
	if values, ok := r.URL.Query()["recursive"]; ok {
		if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
			return fc.NewProblem(400, "invalid_request", "Recursive must be true or false.")
		}
		recursive = values[0] == "true"
	}
	return s.filesAction(w, r, f, fileops.ActionRequest{Kind: "delete", From: storage.Path(p.Relative()), Recursive: recursive, IfRevision: r.Header.Get("If-Revision")})
}
func (s *Server) filesAction(w http.ResponseWriter, r *http.Request, f *fileRequest, request fileops.ActionRequest) error {
	native, root, actor, err := s.fileMutationContext(r.Context(), f)
	if err != nil {
		return err
	}
	request.Root = root
	request.Actor = actor
	request.IdempotencyKey = r.Header.Get("Idempotency-Key")
	if request.IdempotencyKey == "" {
		request.IdempotencyKey = randomFilesKey()
	}
	actions, err := s.filesActions(r, f, native)
	if err != nil {
		return err
	}
	op, replay, err := actions.Prepare(r.Context(), request, native)
	if err != nil {
		return filesMutationProblem(err)
	}
	if !op.Terminal() && strings.Contains(strings.ToLower(r.Header.Get("Prefer")), "respond-async") {
		if err = s.startFilesAction(r, f, op); err != nil && !errors.Is(err, errFilesWorkerBusy) {
			return filesMutationProblem(err)
		}
		return writeFilesOperation(w, op, 202)
	}
	if !op.Terminal() {
		op, err = actions.Run(r.Context(), root, actor, op.ID, native)
	}
	if err != nil {
		if op.ID != "" {
			w.Header().Set("Location", "/api/files/operations/"+op.ID)
		}
		return filesMutationProblem(err)
	}
	if !op.Terminal() {
		return writeFilesOperation(w, op, 202)
	}
	if op.State != fileops.Succeeded {
		return filesOperationFailure(op)
	}
	if request.Kind == "delete" {
		w.WriteHeader(204)
		return nil
	}
	status := 201
	if request.Kind == "move" {
		status = 200
	}
	if request.Kind == "mkdir" && len(op.Entries) == 0 {
		status = 200
	}
	_ = replay
	return writeFilesJSON(w, status, json.RawMessage(op.Result))
}
func (s *Server) filesPutRecovery(r *http.Request, f *fileRequest, native fileops.NativeActions) (*fileops.Service, error) {
	return fileops.NewService(s.filesJournal, fileops.ServiceOptions{Authorize: s.filesMutationAuthorize(r, f, native), Commit: nodes.CommitStagedFile, PhysicalDelta: func(op fileops.Operation, _ storage.MutationResult) (int64, error) {
		return op.Reservation.NewBytes - op.Reservation.OldBytes, nil
	}})
}
