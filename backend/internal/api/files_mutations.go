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
	"net/url"
	"path"
	"strings"
	"time"

	"everythingabout.me/backend/internal/filechanges"
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
		case "upload_expired", "upload_aborted", "upload_gone", "upload_failed":
			status = 410
		case "invalid_part":
			status = 400
		case "intent_conflict":
			code = "idempotency_conflict"
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
		problem := fc.NewProblem(status, code, "The file operation could not complete: "+code+".")
		if code == "operation_in_progress" {
			problem.OperationID = e.OperationID
		}
		return problem
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
	root := fileops.Root{OwnerID: binding.OwnerID, ConnectionID: binding.ConnectionID, Generation: binding.Generation, ContentRoot: binding.ContentRoot, SystemRoot: binding.SystemRoot, CaseSensitive: native.StorageNameRules().CaseSensitive, RetainsVersions: native.Describe().RetainsVersions}
	actor := fileops.Actor{ID: filePrincipalID(f.Principal), Scope: storage.Path(f.Scope.Relative()), CanRead: f.Principal.Token == nil || f.Principal.Token.CanRead, CanWrite: f.Principal.Token == nil || f.Principal.Token.CanWrite, ManageOwner: f.Principal.Token == nil}
	if attribution, requestID, ok := filechanges.ActorFromContext(ctx); ok {
		actor.Label = attribution.Label
		actor.Kind = attribution.Kind
		actor.RequestID = requestID
		actor.CanChanges = attribution.CanChanges
		actor.CanSubscribe = attribution.CanSubscribe
		actor.CanEvents = attribution.CanEvents
		if f.Principal.Token != nil {
			actor.PolicyVersion = f.Principal.Token.PolicyVersion
		}
	}
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
		if err = s.filesCheckIdentityBindings(ctx, op); err != nil {
			return err
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
	if err := filesUniqueJSONValue(json.NewDecoder(bytes.NewReader(data))); err != nil {
		var problem *fc.Problem
		if errors.As(err, &problem) {
			return err
		}
		return fc.NewProblem(400, "invalid_request", "JSON object fields must occur once.")
	}
	// A null alias must not disguise a mixed destination/target/path request.
	switch out.(type) {
	case *filesMkdirRequest, *filesCreateUploadRequest:
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) == nil {
			for _, key := range []string{"path", "destination", "target"} {
				if value, present := fields[key]; present && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
					return filesSelectorError()
				}
			}
		}
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		var problem *fc.Problem
		if errors.As(err, &problem) {
			return err
		}
		return fc.NewProblem(400, "invalid_request", "The JSON request is invalid.")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fc.NewProblem(400, "invalid_request", "Exactly one JSON object is required.")
	}
	return nil
}

// Duplicate keys can otherwise replace a selected identity or alias silently.
func filesUniqueJSONValue(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for d.More() {
			token, err = d.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok || seen[key] {
				return errors.New("duplicate JSON field")
			}
			seen[key] = true
			if err = filesUniqueJSONValue(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err = filesUniqueJSONValue(d); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	_, err = d.Token()
	return err
}

func filesSelectorSelection(selector fc.EntrySelector) string {
	if selector.IsID() {
		return "id"
	}
	return "path"
}
func filesDestinationSelection(destination fc.Destination) string {
	if destination.Parent == nil {
		return "path"
	}
	if destination.Parent.IsID() {
		return "parentId"
	}
	return "parentPath"
}

// Bind the indexed identity after resolution and reject a concurrent retarget
// between those observations. The saved binding is rechecked before native I/O.
func (s *Server) filesBindMutationSelection(ctx context.Context, f *fileRequest, p fc.Path, mode, selectedID string) (string, string, error) {
	entryID, parentID, err := s.filesSelectionBinding(ctx, f, p, mode)
	if err != nil {
		return "", "", err
	}
	if selectedID != "" && (mode == "id" && entryID != selectedID || mode == "parentId" && parentID != selectedID) {
		return "", "", fc.NewProblem(412, "target_changed", "The selected identity changed before admission.")
	}
	return entryID, parentID, nil
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
func filesPutClientTarget(r *http.Request) (storage.WriteIntent, []filesReplaySelection, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return storage.WriteIntent{}, nil, fc.NewProblem(400, "invalid_request", "The query encoding is invalid.")
	}
	for _, values := range q {
		if len(values) != 1 {
			return storage.WriteIntent{}, nil, filesSelectorError()
		}
	}
	if q.Has("parentId") || q.Has("parentPath") || q.Has("name") {
		if q.Has("parentId") == q.Has("parentPath") || !q.Has("name") || q.Has("id") || q.Has("path") || r.Header.Get("If-Match") != "" {
			return storage.WriteIntent{}, nil, filesSelectorError()
		}
		parent := fc.EntrySelector{ID: q.Get("parentId"), Path: fc.Path(q.Get("parentPath"))}
		destination := fc.Destination{Parent: &parent, Name: q.Get("name")}
		if err = destination.Validate(); err != nil {
			return storage.WriteIntent{}, nil, err
		}
		target, parentID := filesClientDestination(destination)
		return storage.WriteIntent{Target: target, ParentID: parentID, Selection: filesDestinationSelection(destination)}, []filesReplaySelection{{selector: parent, write: true}}, nil
	}
	selector, err := fc.ParseSelectorQuery(r.URL.RawQuery, false)
	if err != nil {
		return storage.WriteIntent{}, nil, err
	}
	if selector.IsID() && r.Header.Get("If-Match") == "" {
		return storage.WriteIntent{}, nil, fc.NewProblem(428, "precondition_required", "Replacing a file by ID requires If-Match.")
	}
	if selector.Path == "/" {
		return storage.WriteIntent{}, nil, fc.NewProblem(400, "invalid_path", "The root cannot contain file content.")
	}
	return storage.WriteIntent{Target: storage.Path(selector.Path.Relative()), EntryID: selector.ID, Selection: filesSelectorSelection(selector)}, []filesReplaySelection{{selector: selector, write: true}}, nil
}

func (s *Server) handleFilesPut(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if !s.cfg.Storage.CoordinatedWrites {
		return fc.NewProblem(501, "not_implemented", "Native file writes are not enabled.")
	}
	intent, replaySelection, err := filesPutClientTarget(r)
	if err != nil {
		return err
	}
	if err = f.RequireWrite(f.Scope); err != nil {
		return err
	}
	if err = s.filesAuthorizeReplaySelection(r.Context(), f, replaySelection); err != nil {
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
	if s.cfg.BackendName() != "local" && modified != nil {
		return fc.NewProblem(422, "unsupported_option", "This object route cannot set native source modification time.")
	}
	if condition.Mode == fc.PutReplace && !native.Describe().AtomicReplace {
		return fc.NewProblem(422, "unsupported_option", "This storage connection supports new files only.")
	}
	if max := native.Describe().MaxFileBytes; max > 0 && r.ContentLength > max {
		return fc.NewProblem(413, "request_too_large", "The content exceeds the admitted publication limit.")
	}
	intent.Size, intent.MediaType, intent.SourceModifiedAt, intent.Digest, intent.ClientCondition = r.ContentLength, media, modified, digest, filesPutClientCondition(condition)
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
		if e = s.filesAuthorizeReplaySelection(r.Context(), f, replaySelection); e != nil {
			return true, e
		}
		if e = filesValidatePutReplay(r.Context(), r.Body, intent, old.Stage); e != nil {
			return true, filesMutationProblem(e)
		}
		saved, e := service.Write(r.Context(), root, actor, old.ID, native, nil)
		s.filesCleanupTerminal(r.Context(), service, native, root, actor, saved)
		return true, s.filesPutResponse(w, r, saved, e, condition.Mode == fc.PutReplace, f)
	}
	if found, e := replayWrite(); found {
		return e
	}
	p, err := s.filesResolvePutQuery(r.Context(), f, r)
	if err != nil {
		return err
	}
	if err = f.RequireWrite(p); err != nil {
		return err
	}
	selectedID := intent.EntryID
	if intent.Selection == "parentId" {
		selectedID = intent.ParentID
	}
	entryID, parentID, err := s.filesBindMutationSelection(r.Context(), f, p, intent.Selection, selectedID)
	if err != nil {
		return err
	}
	if condition.Mode == fc.PutCreate {
		entryID = ""
	}
	intent.Target, intent.EntryID, intent.ParentID = storage.Path(p.Relative()), entryID, parentID
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
		if condition.Mode == fc.PutCreate {
			if condition.ExplicitCreate {
				return fc.NewProblem(412, "precondition_failed", "The destination already exists.")
			}
			return fc.NewProblem(409, "name_conflict", "The destination already exists.")
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
	physicalReserved, err := fileops.PhysicalReservation(root, intent.Size)
	if !root.RetainsVersions && s.cfg.BackendName() != "local" {
		physicalReserved += intent.Size
	}
	if err != nil {
		return filesMutationProblem(err)
	}
	encoded, _ := json.Marshal(intent)
	op, _, err := s.filesJournal.Begin(r.Context(), fileops.BeginRequest{Root: root, Actor: actor, Kind: "upload_commit", IdempotencyKey: key, Intent: encoded, Guards: []fileops.Guard{{Path: actor.Scope, Subtree: true}}, Reservation: fileops.ReservationRequest{NewBytes: intent.Size, OldBytes: oldBytes, PhysicalBytes: physicalReserved, CreditPath: func() storage.Path {
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
	return s.filesPutResponse(w, r, saved, err, condition.Mode == fc.PutReplace, f)
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

func (s *Server) filesPutResponse(w http.ResponseWriter, r *http.Request, op fileops.Operation, err error, replaced bool, scopes ...*fileRequest) error {
	if len(scopes) > 0 {
		op.Result = filesScopedResult(op.Result, scopes[0])
	}
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
		encoded, err := json.Marshal(req)
		if err != nil {
			return err
		}
		if err = s.filesCheckIdentityBindings(ctx, fileops.Operation{Root: req.Root, Kind: req.Kind, Intent: encoded}); err != nil {
			return err
		}
		if req.IfRevision == "" {
			return nil
		}
		// Client metadata revisions describe the indexed view. Native source and
		// destination versions are captured and fenced separately by Actions.
		p, err := fc.FromRelative(string(req.From))
		if err != nil {
			return err
		}
		resource, err := s.filesIndexedMetadata(ctx, f, p)
		if err != nil {
			return err
		}
		return fc.CheckRevision(req.IfRevision, resource.Revision)
	}})
}

type filesMkdirRequest struct {
	Kind        string          `json:"kind"`
	Destination *fc.Destination `json:"destination"`
	Path        *string         `json:"path"`
	Parents     bool            `json:"parents"`
}

func filesCreateDestination(destination *fc.Destination, legacyPath *string) (fc.Destination, error) {
	if (destination == nil) == (legacyPath == nil) {
		return fc.Destination{}, filesSelectorError()
	}
	if destination != nil {
		return *destination, destination.Validate()
	}
	p, err := fc.ParsePath(*legacyPath)
	if err != nil {
		return fc.Destination{}, err
	}
	return fc.Destination{Path: p}, nil
}

// These paths describe client selectors only. An ID-selected location is resolved
// after replay, so a lost response can still be recovered after a move or delete.
func filesClientDestination(d fc.Destination) (storage.Path, string) {
	if d.Parent == nil {
		return storage.Path(d.Path.Relative()), ""
	}
	if d.Parent.IsID() {
		return storage.Path(d.Name), d.Parent.ID
	}
	return storage.Path(strings.TrimPrefix(path.Join(string(d.Parent.Path), d.Name), "/")), ""
}

type filesReplaySelection struct {
	selector    fc.EntrySelector
	read, write bool
}

func (s *Server) filesAuthorizeReplaySelection(ctx context.Context, f *fileRequest, selected []filesReplaySelection) error {
	for _, selection := range selected {
		if selection.selector.IsID() {
			index, ok := s.filesIndex.(FileIdentityIndex)
			if !ok {
				return fc.NewProblem(503, "temporarily_unavailable", "File identity lookup is unavailable.")
			}
			_, found, err := index.FileResolveID(ctx, f.Principal.User.ID, selection.selector.ID)
			if err != nil {
				return err
			}
			// A retired ID has no current entry to disclose. Only a matched, scoped
			// journal record can reach here; pending operations still validate bindings.
			if !found {
				continue
			}
		}
		p, err := s.filesResolveSelector(ctx, f, selection.selector)
		if err != nil {
			return err
		}
		if selection.read {
			if err = f.RequireRead(p); err != nil {
				return err
			}
		}
		if selection.write {
			if err = f.RequireWrite(p); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) filesReplayActionRequest(w http.ResponseWriter, r *http.Request, f *fileRequest, request fileops.ActionRequest, selected ...filesReplaySelection) (bool, error) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return false, nil
	}
	if len(key) > 1024 || strings.ContainsAny(key, "\r\n\x00") {
		return true, fc.NewProblem(400, "invalid_request", "Idempotency-Key is invalid.")
	}
	if err := f.RequireWrite(f.Scope); err != nil {
		return true, err
	}
	if request.Kind == "move" || request.Kind == "copy" {
		if err := f.RequireRead(f.Scope); err != nil {
			return true, err
		}
	}
	if err := s.filesAuthorizeReplaySelection(r.Context(), f, selected); err != nil {
		return true, err
	}
	_, root, actor, err := s.fileMutationContext(r.Context(), f)
	if err != nil {
		return true, err
	}
	request.Root, request.Actor, request.IdempotencyKey = root, actor, key
	op, found, err := s.filesJournal.ReplayAction(r.Context(), request)
	if err != nil {
		return true, filesMutationProblem(err)
	}
	if !found {
		return false, nil
	}
	if err = s.filesAuthorizeReplaySelection(r.Context(), f, selected); err != nil {
		return true, err
	}
	var original fileops.ActionRequest
	if json.Unmarshal(op.Intent, &original) != nil {
		return true, fileops.E("invalid_intent", nil)
	}
	// Reuse the saved admission paths. Prepare authorizes the original guards and
	// persistent identities before running any pending native effect.
	return true, s.filesAction(w, r, f, original)
}

func (s *Server) handleFilesMkdir(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if !s.cfg.Storage.CoordinatedWrites {
		return fc.NewProblem(501, "not_implemented", "Native file writes are not enabled.")
	}
	var request filesMkdirRequest
	if err := strictFilesJSON(r, &request); err != nil {
		return err
	}
	if request.Kind != "folder" {
		return fc.NewProblem(400, "invalid_request", "Folder creation requires kind folder.")
	}
	destination, err := filesCreateDestination(request.Destination, request.Path)
	if err != nil {
		return err
	}
	if request.Parents && destination.Parent != nil {
		return fc.NewProblem(400, "invalid_request", "Creating parents requires a full-path destination.")
	}
	clientTo, clientParent := filesClientDestination(destination)
	parentSelector := fc.EntrySelector{Path: destination.Path}
	if destination.Parent != nil {
		parentSelector = *destination.Parent
	}
	if replayed, e := s.filesReplayActionRequest(w, r, f, fileops.ActionRequest{Kind: "mkdir", To: clientTo, ParentID: clientParent, Parents: request.Parents, Selection: "path:" + filesDestinationSelection(destination)}, filesReplaySelection{selector: parentSelector, write: true}); replayed {
		return e
	}
	p, err := s.filesResolveDestination(r.Context(), f, destination)
	if err != nil {
		return err
	}
	if err = f.RequireWrite(p); err != nil {
		return err
	}
	mode := filesDestinationSelection(destination)
	selectedID := ""
	if destination.Parent != nil {
		selectedID = destination.Parent.ID
	}
	_, parentID, err := s.filesBindMutationSelection(r.Context(), f, p, mode, selectedID)
	if err != nil {
		return err
	}
	return s.filesAction(w, r, f, fileops.ActionRequest{Kind: "mkdir", To: storage.Path(p.Relative()), Parents: request.Parents, ParentID: parentID, Selection: "path:" + mode})
}

type filesMoveCopyRequest struct {
	From             fc.EntrySelector `json:"from"`
	To               fc.Destination   `json:"to"`
	IfRevision       string           `json:"ifRevision"`
	IfSourceRevision string           `json:"ifSourceRevision"`
}

func (s *Server) filesMoveCopy(w http.ResponseWriter, r *http.Request, f *fileRequest, kind string) error {
	if !s.cfg.Storage.CoordinatedWrites {
		return fc.NewProblem(501, "not_implemented", "Native file writes are not enabled.")
	}
	var request filesMoveCopyRequest
	if err := strictFilesJSON(r, &request); err != nil {
		return err
	}
	if err := request.From.Validate(); err != nil {
		return err
	}
	if err := request.To.Validate(); err != nil {
		return err
	}
	revision := request.IfRevision
	if kind == "copy" {
		revision = request.IfSourceRevision
	}
	if revision == "" {
		return fc.NewProblem(428, "precondition_required", "The source metadata revision is required.")
	}
	clientTo, clientParent := filesClientDestination(request.To)
	destinationSelector := fc.EntrySelector{Path: request.To.Path}
	if request.To.Parent != nil {
		destinationSelector = *request.To.Parent
	}
	if replayed, e := s.filesReplayActionRequest(w, r, f, fileops.ActionRequest{Kind: kind, From: storage.Path(request.From.Path.Relative()), To: clientTo, EntryID: request.From.ID, ParentID: clientParent, IfRevision: revision, Selection: filesSelectorSelection(request.From) + ":" + filesDestinationSelection(request.To)}, filesReplaySelection{selector: request.From, read: true, write: kind == "move"}, filesReplaySelection{selector: destinationSelector, write: true}); replayed {
		return e
	}
	from, err := s.filesResolveSelector(r.Context(), f, request.From)
	if err != nil {
		return err
	}
	to, err := s.filesResolveDestination(r.Context(), f, request.To)
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

	sourceMode, destinationMode := filesSelectorSelection(request.From), filesDestinationSelection(request.To)
	entryID, _, err := s.filesBindMutationSelection(r.Context(), f, from, sourceMode, request.From.ID)
	if err != nil {
		return err
	}
	selectedParentID := ""
	if request.To.Parent != nil {
		selectedParentID = request.To.Parent.ID
	}
	_, parentID, err := s.filesBindMutationSelection(r.Context(), f, to, destinationMode, selectedParentID)
	if err != nil {
		return err
	}
	return s.filesAction(w, r, f, fileops.ActionRequest{Kind: kind, From: storage.Path(from.Relative()), To: storage.Path(to.Relative()), IfRevision: revision, EntryID: entryID, ParentID: parentID, Selection: sourceMode + ":" + destinationMode})
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
	selector, err := fc.ParseSelectorQuery(r.URL.RawQuery, false)
	if err != nil {
		return err
	}
	recursive := false
	if values, ok := r.URL.Query()["recursive"]; ok {
		if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
			return fc.NewProblem(400, "invalid_request", "Recursive must be true or false.")
		}
		recursive = values[0] == "true"
	}
	mode := filesSelectorSelection(selector)
	if replayed, e := s.filesReplayActionRequest(w, r, f, fileops.ActionRequest{Kind: "delete", From: storage.Path(selector.Path.Relative()), EntryID: selector.ID, Selection: mode, Recursive: recursive, IfRevision: r.Header.Get("If-Revision")}, filesReplaySelection{selector: selector, write: true}); replayed {
		return e
	}
	p, err := s.filesResolveQuery(r.Context(), f, r.URL.RawQuery, false)
	if err != nil {
		return err
	}
	if err = f.RequireWrite(p); err != nil {
		return err
	}
	entryID, parentID, err := s.filesBindMutationSelection(r.Context(), f, p, mode, r.URL.Query().Get("id"))
	if err != nil {
		return err
	}
	return s.filesAction(w, r, f, fileops.ActionRequest{Kind: "delete", From: storage.Path(p.Relative()), Recursive: recursive, IfRevision: r.Header.Get("If-Revision"), EntryID: entryID, ParentID: parentID, Selection: mode})
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
		return writeFilesOperation(w, op, 202, f)
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
		return writeFilesOperation(w, op, 202, f)
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
	return writeFilesJSON(w, status, filesScopedResult(op.Result, f))
}
func (s *Server) filesPutRecovery(r *http.Request, f *fileRequest, native fileops.NativeActions) (*fileops.Service, error) {
	return fileops.NewService(s.filesJournal, fileops.ServiceOptions{Authorize: s.filesMutationAuthorize(r, f, native), Commit: nodes.CommitStagedFile, PhysicalDelta: func(op fileops.Operation, _ storage.MutationResult) (int64, error) {
		return fileops.PhysicalDelta(op.Root, op.Reservation.NewBytes, op.Reservation.OldBytes), nil
	}})
}
