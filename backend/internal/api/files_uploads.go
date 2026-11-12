package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/uploadspool"
)

const filesUploadPartSize int64 = 16 << 20
const filesUploadMaxParts = 10000

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
func filesUploadExpired(u fileops.Upload) error {
	if u.State == "open" && !u.ExpiresAt.IsZero() && u.ExpiresAt.Before(time.Now()) {
		return fileops.E("upload_expired", nil)
	}
	return nil
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

// filesUploadEnded answers a request against a session that can no longer accept bytes or
// commit, and says why when the journal recorded it. Without the reason a client whose
// session was replaced by a newer one for the same path, or failed because its destination
// changed, sees only "upload_aborted" for something it never asked to abort.
func filesUploadEnded(u fileops.Upload, err error) error {
	problem := filesMutationProblem(err)
	p := filesProblem(problem)
	if p == nil || p.Status != 410 {
		return problem
	}
	var saved struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(u.Operation.Problem, &saved) == nil && saved.Detail != "" {
		p.Detail = saved.Detail
	}
	return p
}
func (s *Server) filesUploadResource(u fileops.Upload, scopes ...*fileRequest) map[string]any {
	if len(scopes) > 0 {
		u.Operation.Result = filesScopedResult(u.Operation.Result, scopes[0])
	}
	var intent storage.WriteIntent
	_ = json.Unmarshal(u.Operation.Intent, &intent)
	var result any
	if len(u.Operation.Result) > 0 {
		result = json.RawMessage(u.Operation.Result)
	}
	resource := map[string]any{"class": s.filesTransferClassLabel(), "id": u.ID, "path": "/" + string(intent.Target), "state": u.State, "size": intent.Size, "partSize": u.PartSize, "partCount": u.PartCount, "ordering": "any", "nextPart": nil, "receivedParts": u.ReceivedParts, "receivedBytes": u.ReceivedBytes, "createdAt": u.CreatedAt.UTC(), "expiresAt": u.ExpiresAt.UTC(), "partsUrl": "/api/files/uploads/" + u.ID + "/parts", "commitUrl": "/api/files/uploads/" + u.ID + "/commit", "operationId": u.Operation.ID, "result": result}
	if s.filesTransferClass() == storage.TransferPresignedParts {
		resource["signUrl"] = "/api/files/uploads/" + u.ID + "/parts/sign"
	}
	return resource
}
func filesUploadReceipt(p fileops.PartReceipt) map[string]any {
	// Class C receipts carry the server-computed SHA-256. They are not a
	// provider digest and never a client assertion.
	return map[string]any{"number": p.Number, "size": p.Size, "sha256": p.SHA256}
}
func filesUploadMaxSize(native fileops.NativeActions) int64 {
	max := int64(filesUploadMaxParts) * filesUploadPartSize
	if numbered, ok := native.(storage.NumberedUploadWriter); ok {
		return min(max, numbered.UploadMaxFileBytes())
	}
	if limit := native.Describe().MaxFileBytes; limit > 0 && limit < max {
		max = limit
	}
	return max
}

type filesCreateUploadRequest struct {
	Destination *fc.Destination `json:"destination"`
	// IdempotencyKey is used by the batch form only; the single create reads the header.
	IdempotencyKey   string            `json:"idempotencyKey,omitempty"`
	Target           *fc.EntrySelector `json:"target"`
	Path             *string           `json:"path"`
	Size             *int64            `json:"size"`
	MediaType        string            `json:"mediaType"`
	SourceModifiedAt *time.Time        `json:"sourceModifiedAt"`
	Condition        struct {
		Mode            string `json:"mode"`
		ContentRevision string `json:"contentRevision"`
	} `json:"condition"`
	// Parents creates the missing ancestor folders of a full-path destination as
	// part of this request. The folder rows are the same mkdir a client could send;
	// this only saves a round trip per folder when a whole tree is imported.
	Parents bool `json:"parents"`
}

func filesUploadClientTarget(req filesCreateUploadRequest) (storage.WriteIntent, []filesReplaySelection, error) {
	switch req.Condition.Mode {
	case "", "create":
		if req.Target != nil {
			return storage.WriteIntent{}, nil, filesSelectorError()
		}
		destination, err := filesCreateDestination(req.Destination, req.Path)
		if err != nil {
			return storage.WriteIntent{}, nil, err
		}
		target, parentID := filesClientDestination(destination)
		selector := fc.EntrySelector{Path: destination.Path}
		if destination.Parent != nil {
			selector = *destination.Parent
		}
		return storage.WriteIntent{Target: target, ParentID: parentID, Selection: filesDestinationSelection(destination)}, []filesReplaySelection{{selector: selector, write: true}}, nil
	case "replace":
		if req.Destination != nil || (req.Target == nil) == (req.Path == nil) {
			return storage.WriteIntent{}, nil, filesSelectorError()
		}
		target := fc.EntrySelector{}
		if req.Target != nil {
			target = *req.Target
		} else {
			target.Path = fc.Path(*req.Path)
		}
		if err := target.Validate(); err != nil {
			return storage.WriteIntent{}, nil, err
		}
		return storage.WriteIntent{Target: storage.Path(target.Path.Relative()), EntryID: target.ID, Selection: filesSelectorSelection(target)}, []filesReplaySelection{{selector: target, write: true}}, nil
	default:
		return storage.WriteIntent{}, nil, fc.NewProblem(400, "invalid_request", "The upload condition is invalid.")
	}
}

func (s *Server) filesResolveUploadTarget(ctx context.Context, f *fileRequest, req filesCreateUploadRequest) (fc.Path, string, string, error) {
	switch req.Condition.Mode {
	case "", "create":
		if req.Target != nil {
			return "", "", "", filesSelectorError()
		}
		destination, err := filesCreateDestination(req.Destination, req.Path)
		if err != nil {
			return "", "", "", err
		}
		p, err := s.filesResolveDestination(ctx, f, destination)
		selectedID := ""
		if destination.Parent != nil {
			selectedID = destination.Parent.ID
		}
		return p, filesDestinationSelection(destination), selectedID, err
	case "replace":
		if req.Destination != nil || (req.Target == nil) == (req.Path == nil) {
			return "", "", "", filesSelectorError()
		}
		var target fc.EntrySelector
		if req.Target != nil {
			target = *req.Target
		} else {
			target.Path = fc.Path(*req.Path)
		}
		p, err := s.filesResolveSelector(ctx, f, target)
		return p, filesSelectorSelection(target), target.ID, err
	default:
		return "", "", "", fc.NewProblem(400, "invalid_request", "The upload condition is invalid.")
	}
}

// filesEnsureParents materializes the folders above an upload destination that asked
// for missing parents. It runs the same mkdir action a client would have sent, so
// identity, change facts and audit stay identical; only the round trip moves from the
// client into this request.
//
// The chain is known from the destination path, so nothing is guessed: the outermost
// missing level is created first, and a level a concurrent upload created first is
// adopted — mkdir is idempotent and the conflicting operation's guard is the wait.
func (s *Server) filesEnsureParents(ctx context.Context, r *http.Request, f *fileRequest, native fileops.NativeActions, root fileops.Root, actor fileops.Actor, target storage.Path) error {
	if target == "" {
		return nil
	}
	entry, err := native.Stat(ctx, target)
	if err == nil {
		if entry.Kind != storage.Folder {
			return fc.NewProblem(409, "not_a_folder", "A file occupies the destination parent.")
		}
		return nil
	}
	if !storage.IsCode(err, "not_found") {
		return filesMutationProblem(err)
	}
	parent := storage.Path("")
	if i := strings.LastIndexByte(string(target), '/'); i > 0 {
		parent = target[:i]
	}
	if parent != "" {
		if err = s.filesEnsureParents(ctx, r, f, native, root, actor, parent); err != nil {
			return err
		}
	}
	actions, err := s.filesActions(r, f, native)
	if err != nil {
		return err
	}
	request := fileops.ActionRequest{
		Root: root, Actor: actor, Kind: "mkdir", To: target,
		Selection: "path:path:" + string(target), IdempotencyKey: randomFilesKey(),
	}
	// A concurrent upload may be creating this same level. How long that takes is its
	// business, not a number picked here, so the wait is bounded by the request's own
	// context: the caller gives up when it gives up, and once the wait stops being
	// momentary the log names the operation holding the path.
	var waited time.Duration
	warned := false
	for {
		op, _, e := actions.Prepare(ctx, request, native)
		if e == nil && !op.Terminal() {
			op, e = actions.Run(ctx, root, actor, op.ID, native)
		}
		if e == nil {
			if op.State != fileops.Succeeded {
				return filesOperationFailure(op)
			}
			return nil
		}
		if fileops.IsCode(e, "name_conflict") {
			if current, statErr := native.Stat(ctx, target); statErr == nil && current.Kind == storage.Folder {
				return nil
			}
		}
		if fileops.IsCode(e, "operation_in_progress") {
			if current, statErr := native.Stat(ctx, target); statErr == nil && current.Kind == storage.Folder {
				return nil
			}
			var held *fileops.Error
			holder := ""
			if errors.As(e, &held) && held.OperationID != "" {
				holder = held.OperationKind + " " + held.OperationID
			}
			select {
			case <-ctx.Done():
				slog.Info("stopped waiting for the destination folder", "target", target, "heldBy", holder, "waited", waited.Round(time.Millisecond))
				return filesMutationProblem(ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
			waited += 100 * time.Millisecond
			if !warned && waited >= 5*time.Second {
				warned = true
				slog.Warn("waiting for another file operation to create the destination", "target", target, "heldBy", holder, "waited", waited.Round(time.Second))
			}
			request.IdempotencyKey = randomFilesKey()
			continue
		}
		return filesMutationProblem(e)
	}
}

func (s *Server) handleFilesCreateUpload(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	var req filesCreateUploadRequest
	if err := strictFilesJSON(r, &req); err != nil {
		return err
	}
	client, replaySelection, err := filesUploadClientTarget(req)
	if err != nil {
		return err
	}
	if client.Target == "" && client.Selection != "id" {
		return fc.NewProblem(400, "invalid_path", "The root cannot contain file content.")
	}
	if err = f.RequireWrite(f.Scope); err != nil {
		return err
	}
	if err = s.filesAuthorizeReplaySelection(r.Context(), f, replaySelection); err != nil {
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
	if s.filesTransferClass() == storage.TransferPresignedParts {
		if _, e := s.filesPlannedWriter(native); e != nil {
			return e
		}
	}
	if numbered, ok := native.(storage.NumberedUploadWriter); ok && numbered.UploadMaxFileBytes() == 0 {
		return fc.NewProblem(422, "unsupported_option", "Native multipart uploads require explicit provider admission evidence.")
	}
	if *req.Size > filesUploadMaxSize(native) {
		return fc.NewProblem(413, "file_too_large", "The file exceeds the admitted publication or part-count limit.")
	}
	if s.cfg.BackendName() != "local" && req.SourceModifiedAt != nil {
		return fc.NewProblem(422, "unsupported_option", "This object route cannot set native source modification time.")
	}
	if condition.Mode == fc.PutReplace && !native.Describe().AtomicReplace {
		return fc.NewProblem(422, "unsupported_option", "This storage connection supports new files only.")
	}
	if req.SourceModifiedAt != nil {
		v := req.SourceModifiedAt.UTC()
		req.SourceModifiedAt = &v
	}
	client.Size, client.MediaType, client.SourceModifiedAt, client.ClientCondition = *req.Size, req.MediaType, req.SourceModifiedAt, filesPutClientCondition(condition)
	if old, found, e := s.filesJournal.ReplayUpload(r.Context(), root, actor, key, client); e != nil {
		return filesMutationProblem(e)
	} else if found {
		if e = s.filesAuthorizeReplaySelection(r.Context(), f, replaySelection); e != nil {
			return e
		}
		if e = s.filesMutationAuthorize(r, f, native)(r.Context(), old.Operation); e != nil {
			return filesMutationProblem(e)
		}
		resource := s.filesUploadResource(old, f)
		if filesUploadExpired(old) != nil {
			resource["state"] = "expired"
		}
		if old.State == "open" && filesUploadExpired(old) == nil {
			// A replayed create answers with the transport that session actually uses. A
			// direct session publishes with one request at the destination, so handing it the
			// multipart transport would send the browser to parts that do not exist.
			var transport any
			var e error
			if old.Mode == fileops.UploadModeDirect {
				transport, e = s.filesDirectPutTransport(r.Context(), native, old)
			} else {
				transport, e = s.filesUploadTransport(r.Context(), native, old)
			}
			if e != nil {
				return e
			}
			resource["transport"] = transport
		}
		w.Header().Set("Location", "/api/files/uploads/"+old.ID)
		return writeFilesJSON(w, 200, resource)
	}
	p, selection, selectedID, err := s.filesResolveUploadTarget(r.Context(), f, req)
	if err != nil {
		return err
	}
	if p == "/" {
		return fc.NewProblem(400, "invalid_path", "The root cannot contain file content.")
	}
	if err = f.RequireWrite(p); err != nil {
		return err
	}
	parentName := path.Dir(p.Relative())
	if parentName == "." {
		parentName = ""
	}
	parentPath := storage.Path(parentName)
	parent, err := native.Stat(r.Context(), parentPath)
	// On a provider that makes a folder with its first child, parents:true only needs the
	// ancestor to be *acceptable* now; settlement materializes it when the file lands. Doing
	// it here was one provider-heavy mkdir per folder, paid again on every import, and it
	// bought nothing: the marker is the same object the first child's key already implies.
	// A filesystem cannot do this — it needs the directory before the file — so it keeps the
	// explicit mkdir, which is what ImplicitParents reports.
	implicitParents := req.Parents && condition.Mode == fc.PutCreate && native.Describe().ImplicitParents
	if storage.IsCode(err, "not_found") && req.Parents && condition.Mode == fc.PutCreate && !implicitParents {
		if err = s.filesEnsureParents(r.Context(), r, f, native, root, actor, parentPath); err != nil {
			return err
		}
		parent, err = native.Stat(r.Context(), parentPath)
	}
	if err != nil && !storage.IsCode(err, "not_found") {
		return filesMutationProblem(err)
	}
	if storage.IsCode(err, "not_found") {
		if !implicitParents {
			return fc.NewProblem(409, "parent_missing", "The destination parent does not exist.")
		}
	} else if parent.Kind != storage.Folder {
		return fc.NewProblem(409, "not_a_folder", "The destination parent is not a folder.")
	}
	entryID, parentID, err := s.filesBindMutationSelection(r.Context(), f, p, selection, selectedID)
	if err != nil {
		return err
	}
	if condition.Mode == fc.PutCreate {
		entryID = ""
	}
	client.Target, client.EntryID, client.ParentID = storage.Path(p.Relative()), entryID, parentID
	client.Parents = implicitParents
	intent := client
	intent.ParentPath = parentPath
	intent.ParentRevision = parent.Revision
	var oldBytes int64
	existing, err := native.Stat(r.Context(), intent.Target)
	if err != nil && !storage.IsCode(err, "not_found") {
		return filesMutationProblem(err)
	}
	// published says the destination already holds exactly the bytes this session would
	// upload: a direct publication whose commit failed after the client's PUT landed. The
	// retry then has nothing to transfer, and refusing it with name_conflict would leave a
	// file the tree has and the library does not.
	published := false
	if err == nil {
		if existing.Kind != storage.File {
			return fc.NewProblem(409, "name_conflict", "A folder occupies the destination.")
		}
		if condition.Mode == fc.PutCreate {
			// Adoption needs the bytes to be provably this owner's own failed publication
			// and a transfer class that can publish without the client uploading again.
			if writer, ok := native.(storage.DirectPutWriter); ok && writer.SupportsDirectPut() && s.filesTransferClass() == storage.TransferPresignedParts {
				published = s.filesReclaimsPublication(r.Context(), root.OwnerID, string(intent.Target), existing, *req.Size)
			}
			if !published {
				return fc.NewProblem(409, "name_conflict", "The destination already exists.")
			}
			intent.Conditions.Absent = true
		} else {
			revision, e := s.filesObservedContentRevision(r.Context(), f, existing)
			if e != nil {
				return e
			}
			if e = fc.CheckPutCondition(condition, revision); e != nil {
				return e
			}
			intent.Conditions.ExpectedRevision = existing.Revision
			oldBytes = existing.Size
		}
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
	// Unpublished parts coexist with the final publication, and Local chunks coexist
	// with their assembled stage, so reserve one file's worth of headroom. This is
	// deliberately independent of how many part bodies a client keeps in flight: the
	// receiver admits those with one process-wide pool, not a per-session number.
	physical += intent.Size
	reservation := fileops.ReservationRequest{NewBytes: intent.Size, OldBytes: oldBytes, PhysicalBytes: physical}
	if oldBytes > 0 {
		reservation.CreditPath = intent.Target
		reservation.CreditVersion = intent.Conditions.ExpectedRevision
	}
	encoded, _ := json.Marshal(intent)
	// One conditional PUT publishes a file that fits in a single part: the client writes the
	// destination itself, so nothing is staged and the commit only verifies the object. A
	// driver that cannot sign that condition keeps the multipart path.
	mode := fileops.UploadModeMultipart
	if published {
		// Nothing is staged: the writer observes the object the client already wrote.
		mode = fileops.UploadModeDirect
	} else if *req.Size > 0 && *req.Size <= filesUploadPartSize && s.filesTransferClass() == storage.TransferPresignedParts {
		if writer, ok := native.(storage.DirectPutWriter); ok && writer.SupportsDirectPut() {
			mode = fileops.UploadModeDirect
		}
	}
	u, err := s.filesCreateUpload(r.Context(), fileops.BeginRequest{Root: root, Actor: actor, Kind: "upload_commit", IdempotencyKey: key, Intent: encoded, Guards: []fileops.Guard{{Path: intent.Target}}, Reservation: reservation}, fileops.UploadOptions{PartSize: filesUploadPartSize, ClientIntent: client, Mode: mode})
	if err != nil {
		return filesMutationProblem(err)
	}
	resource := s.filesUploadResource(u, f)
	if published {
		// The client must not PUT these bytes again: the object is already the publication,
		// and a create condition would refuse the overwrite. It commits, and the commit
		// verifies the same revision before settling it into the index.
		resource["bytesPresent"] = true
	} else {
		var transport any
		if u.Mode == fileops.UploadModeDirect {
			transport, err = s.filesDirectPutTransport(r.Context(), native, u)
		} else {
			transport, err = s.filesUploadTransport(r.Context(), native, u)
		}
		if err != nil {
			// A create that cannot hand the client its transport has admitted a session
			// nobody can finish: it holds a guard on the path and a quota reservation
			// until the idle window, and every retry of the same row opens another. The
			// refusal is the answer, so the session goes with it.
			s.discardUnusableUpload(r.Context(), root, actor, u)
			return err
		}
		resource["transport"] = transport
	}
	w.Header().Set("Location", "/api/files/uploads/"+u.ID)
	return writeFilesJSON(w, 201, resource)
}

// discardUnusableUpload aborts a session the create admitted but cannot give the client a
// transport for. Aborting is not a publication: it releases the path's guard and the
// reservation the same way a client's own abort does, and queues the cleanup that retires
// the operation. The request context is detached because the usual reason to be here is a
// signing failure on a request the client may already have abandoned.
func (s *Server) discardUnusableUpload(ctx context.Context, root fileops.Root, actor fileops.Actor, u fileops.Upload) {
	aborted, err := s.filesJournal.AbortUpload(context.WithoutCancel(ctx), root, actor, u.ID)
	if err != nil {
		slog.Warn("could not abort an upload session that has no usable transport", "upload", u.ID, "cause", err)
		return
	}
	_ = s.queueFileOperation(aborted.Operation)
}

// filesCreateUpload admits a session, waiting out a folder creation that is still settling a
// folder above the destination.
//
// By the time the request reaches this call its own Stat of the parent has succeeded, so the
// folder's marker object exists; what still holds the guard is the index settlement that
// follows that object. A sibling upload of the same import therefore refuses this one for
// milliseconds — surfacing that as 409 cost the browser a round trip, a status probe and up to
// a poll interval to learn what this loop already knows: the folder is about to exist.
//
// Only a folder creation is waited on. An upload, delete, move or copy holder is a decision
// the client or the person has to make, and keeps its own 409 and Operation.
func (s *Server) filesCreateUpload(ctx context.Context, req fileops.BeginRequest, opts fileops.UploadOptions) (fileops.Upload, error) {
	var waited time.Duration
	for {
		u, _, err := s.filesJournal.CreateUpload(ctx, req, opts)
		if err == nil {
			return u, nil
		}
		var held *fileops.Error
		if !errors.As(err, &held) || held.Code != "operation_in_progress" || held.OperationKind != "mkdir" {
			return fileops.Upload{}, err
		}
		select {
		case <-ctx.Done():
			return fileops.Upload{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		waited += 100 * time.Millisecond
		if waited == 5*time.Second {
			// The wait is the client's business, so it is bounded by the request context
			// only. A folder creation that outlives a person's patience is worth naming,
			// though: it means a sibling is stuck, not merely slow.
			target := ""
			if len(req.Guards) > 0 {
				target = string(req.Guards[0].Path)
			}
			slog.Warn("waiting for a folder creation to settle before admitting an upload", "target", target, "heldBy", held.OperationID, "waited", waited)
		}
	}
}

// filesDirectPutTransport signs the one request a direct session uploads. It is shaped like
// a one-part presigned upload so the browser needs no second transport.
//
// A create is signed as a form upload when the connection can, because the browser sends a
// form upload as a CORS-simple request: no preflight, one round trip per small file instead
// of two. A replacement keeps the conditional PUT, which carries the revision a form upload
// cannot express. The edge ingress rewrites provider URLs to a same-origin path that serves
// PUT only, so a form upload is not offered there.
func (s *Server) filesDirectPutTransport(ctx context.Context, native fileops.NativeActions, u fileops.Upload) (any, error) {
	intent, err := filesUploadIntent(u)
	if err != nil {
		return nil, filesMutationProblem(err)
	}
	formPost := intent.Conditions.Absent && (s.cfg == nil || !s.cfg.EdgeOneUploadIngress())
	if post, ok := native.(storage.DirectPostWriter); ok && formPost && post.SupportsDirectPost() {
		grant, err := post.BeginDirectPost(ctx, intent, nil, s.filesPartGrantExpiry(u))
		if err != nil {
			return nil, filesMutationProblem(err)
		}
		payload, err := s.filesPartGrantPayload(grant)
		if err != nil {
			return nil, err
		}
		return map[string]any{"kind": storage.TransferPresignedParts, "parts": []any{payload}, "expiresAt": grant.ExpiresAt.UTC()}, nil
	}
	writer, ok := native.(storage.DirectPutWriter)
	if !ok {
		return nil, fc.NewProblem(503, "temporarily_unavailable", "This connection cannot sign a conditional destination PUT.")
	}
	grant, err := writer.BeginDirectPut(ctx, intent, nil, s.filesPartGrantExpiry(u))
	if err != nil {
		return nil, filesMutationProblem(err)
	}
	payload, err := s.filesPartGrantPayload(grant)
	if err != nil {
		return nil, err
	}
	return map[string]any{"kind": storage.TransferPresignedParts, "parts": []any{payload}, "expiresAt": grant.ExpiresAt.UTC()}, nil
}

// runDirectUploadOperation settles a session whose bytes the client already PUT at the
// destination. The write pipeline is the ordinary one; only its native effect is missing,
// because the client's conditional PUT was the publication.
func (s *Server) runDirectUploadOperation(ctx context.Context, op fileops.Operation, actor fileops.Actor, native fileops.NativeActions, service *fileops.Service) (fileops.Operation, error) {
	writer := fileops.NewDirectUploadWriter(native)
	var err error
	if !op.Terminal() {
		switch {
		case op.Phase == fileops.Admitted && !op.CancelRequested:
			// The writer never reads the body; the object is already at the destination.
			op, err = service.Write(ctx, op.Root, actor, op.ID, writer, bytes.NewReader(nil))
		case op.Phase == fileops.StagedReady && op.State != fileops.Unknown:
			op, err = service.Write(ctx, op.Root, actor, op.ID, writer, nil)
		default:
			op, err = service.Recover(ctx, op.Root, actor, op.ID, writer)
		}
	}
	if err == nil && op.Terminal() {
		err = service.Cleanup(ctx, op.Root, actor, op.ID, writer)
	}
	return op, err
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
	native, _, _, u, err := s.filesUploadContext(r, f)
	if err != nil {
		return err
	}
	resource := s.filesUploadResource(u, f)
	if filesUploadExpired(u) != nil {
		resource["state"] = "expired"
	}
	if u.State == "open" && filesUploadExpired(u) == nil {
		if s.filesTransferClass() == storage.TransferPresignedParts {
			parts, e := s.filesPlannedParts(r.Context(), native, u)
			if e != nil {
				return e
			}
			var bytes int64
			for _, part := range parts {
				bytes += part.Size
			}
			resource["receivedParts"], resource["receivedBytes"] = len(parts), bytes
		}
		transport, e := s.filesUploadTransport(r.Context(), native, u)
		if e != nil {
			return e
		}
		resource["transport"] = transport
	}
	return writeFilesJSON(w, 200, resource)
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
		resources = append(resources, s.filesUploadResource(u, f))
	}
	return writeFilesJSON(w, 200, map[string]any{"items": resources, "nextCursor": next})
}
func (s *Server) handleFilesUploadPart(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if s.filesTransferClass() != storage.TransferRelay {
		return fc.NewProblem(422, "unsupported_option", "This connection uses class A presigned part URLs, not application-relayed parts.")
	}
	native, root, actor, u, err := s.filesUploadContext(r, f)
	if err != nil {
		return err
	}
	if err = filesUploadStateError(u.State); err != nil {
		return filesUploadEnded(u, err)
	}
	if err = filesUploadExpired(u); err != nil {
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
	if numbered, ok := native.(storage.NumberedUploadWriter); ok {
		intent.ID = u.ID
		return s.receiveNativeUploadPart(w, r, numbered, root, actor, u.ID, intent, number, expected, digest)
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
	case storage.IsCode(err, "upload_aborted"):
		return fc.NewProblem(410, "upload_aborted", "The upload no longer accepts bytes.")
	case storage.IsCode(err, "part_conflict"):
		return fc.NewProblem(409, "part_conflict", "The part is bound to different bytes.")
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
	native, root, actor, u, err := s.filesUploadContext(r, f)
	if err != nil {
		return err
	}
	if s.filesTransferClass() == storage.TransferPresignedParts {
		return s.handleFilesPlannedUploadParts(w, r, f, native, u)
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
func (s *Server) handleFilesPlannedUploadParts(w http.ResponseWriter, r *http.Request, f *fileRequest, native fileops.NativeActions, u fileops.Upload) error {
	q, err := parseFilesReadQuery(r, f.Scope, false)
	if err != nil {
		return err
	}
	binding := s.filesCursorBinding(f, q, "upload-parts:"+u.ID)
	key, err := s.filesDecodeCursor(q.Cursor, binding)
	if err != nil {
		return err
	}
	after := 0
	if key != nil {
		after, err = strconv.Atoi(key.Path)
		if err != nil || after < 0 {
			return fc.NewProblem(400, "invalid_cursor", "Invalid provider part cursor.")
		}
	}
	planned, err := s.filesPlannedWriter(native)
	if err != nil {
		return err
	}
	intent, err := filesUploadIntent(u)
	if err != nil {
		return err
	}
	parts, more, err := planned.ListUploadParts(r.Context(), intent, after, q.Limit, s.filesJournal)
	if err != nil {
		if storage.IsCode(err, "not_found") {
			parts, more = nil, false
		} else {
			return filesMutationProblem(err)
		}
	}
	var next any
	if more && len(parts) > 0 {
		next, err = s.filesEncodeCursor(filesSortKey{Path: strconv.Itoa(parts[len(parts)-1].Number)}, binding)
		if err != nil {
			return err
		}
	}
	items := make([]any, 0, len(parts))
	for _, part := range parts {
		items = append(items, map[string]any{"number": part.Number, "size": part.Size, "etag": part.ETag})
	}
	return writeFilesJSON(w, 200, map[string]any{"items": items, "nextCursor": next})
}

func (s *Server) handleFilesCommitUpload(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	native, root, actor, u, err := s.filesUploadContext(r, f)
	if err != nil {
		return err
	}
	if err = filesUploadExpired(u); err != nil {
		return filesMutationProblem(err)
	}
	direct := u.Mode == fileops.UploadModeDirect
	if !direct && s.filesTransferClass() == storage.TransferPresignedParts && u.State == "open" {
		parts, listErr := s.filesPlannedParts(r.Context(), native, u)
		if listErr != nil {
			return listErr
		}
		intent, intentErr := filesUploadIntent(u)
		if intentErr != nil {
			return intentErr
		}
		if coverageErr := filesPlannedCoverage(intent, parts); coverageErr != nil {
			return filesMutationProblem(coverageErr)
		}
	}
	// The freeze is where the session takes its guard and proves its destination is still
	// the one it was opened against. A destination that changed ends the session there, and
	// the terminal operation it returns carries the reason (rendered below).
	opened := u
	if direct {
		// The client's conditional PUT already published the object; there is no part
		// coverage to prove and the commit's own observation of the destination is the
		// evidence. The freeze still takes the guard and proves the destination binding.
		u, err = s.filesJournal.FreezeDirect(r.Context(), root, actor, u.ID, nodes.CheckUploadBinding)
	} else if s.filesTransferClass() == storage.TransferPresignedParts {
		// Class A parts never pass through this application, so coverage is the
		// provider listing verified above rather than application receipts.
		u, err = s.filesJournal.FreezeProviderListed(r.Context(), root, actor, u.ID, nodes.CheckUploadBinding)
	} else {
		u, err = s.filesJournal.FreezeUpload(r.Context(), root, actor, u.ID, nodes.CheckUploadBinding)
	}
	if err != nil {
		return filesUploadEnded(opened, err)
	}
	if u.Operation.State == fileops.Succeeded {
		return writeFilesJSON(w, 200, filesScopedResult(u.Operation.Result, f))
	}
	if u.Operation.Terminal() {
		if opened.State == "open" {
			// This request ended the session at the freeze. Its staged parts are of no use
			// to anyone now, so discard them without waiting for the next sweep.
			_ = s.queueFileOperation(u.Operation)
		}
		return filesOperationFailure(u.Operation)
	}
	// Settle in this request, the way every other mutation does. A small file is done in well
	// under a second, and a 202 made the client spend a round trip and up to a poll interval
	// to read a result the server already had. The settlement still runs under the server's
	// context, so a client that gives up does not abort an accepted publication — it only
	// stops waiting for the answer, and the Operation remains durable for the next read.
	//
	// A caller that asked for async work, or whose operation is already being settled
	// elsewhere (this scanner, another tab, a request that lost its response), gets the
	// Operation and polls it, exactly as before.
	if !strings.Contains(strings.ToLower(r.Header.Get("Prefer")), "respond-async") {
		started, done, e := s.startFileOperation(u.Operation)
		if e != nil && !errors.Is(e, errFilesWorkerBusy) {
			return e
		}
		if !started {
			return writeFilesOperation(w, u.Operation, 202, f)
		}
		select {
		case <-done:
		case <-r.Context().Done():
			return nil
		}
		settled, e := s.filesJournal.Get(r.Context(), root, actor, u.Operation.ID)
		if e != nil {
			return filesMutationProblem(e)
		}
		if !settled.Terminal() {
			return writeFilesOperation(w, settled, 202, f)
		}
		if settled.State == fileops.Succeeded {
			return writeFilesJSON(w, 200, filesScopedResult(settled.Result, f))
		}
		return filesOperationFailure(settled)
	}
	if err = s.queueFileOperation(u.Operation); err != nil && !errors.Is(err, errFilesWorkerBusy) {
		return err
	}
	return writeFilesOperation(w, u.Operation, 202, f)
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
	if u.Mode == fileops.UploadModeDirect {
		return s.runDirectUploadOperation(ctx, op, actor, native, service)
	}
	if s.filesTransferClass() == storage.TransferPresignedParts {
		planned, ok := native.(storage.PlannedUploadWriter)
		if !ok {
			return op, fileops.E("unsupported", nil)
		}
		return s.runPlannedUploadOperation(ctx, op, actor, native, planned, u, service)
	}
	if numbered, ok := native.(storage.NumberedUploadWriter); ok {
		return s.runNativeUploadOperation(ctx, op, actor, native, numbered, u, service)
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
