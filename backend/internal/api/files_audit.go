package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/filechanges"
	fc "everythingabout.me/backend/internal/filecontract"
)

const filesAuditTimeout = 3 * time.Second

// filesAuditRecorder returns false when a fixture has no persistence backend.
// Production records commit before a protected read enters its handler.
type filesAuditRecorder func(context.Context, *filechanges.Audit) (bool, error)

func (s *Server) recordFilesAudit(ctx context.Context, audit *filechanges.Audit) (bool, error) {
	if s.db == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, filesAuditTimeout)
	defer cancel()
	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), filesAuditTimeout)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	if err = filechanges.RecordAudit(ctx, tx, audit); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func filesAuditActor(principal *auth.Principal) filechanges.Actor {
	actor := filechanges.Actor{Kind: "owner", PrincipalID: filePrincipalID(principal), Scope: "/", CanRead: true, CanWrite: true, CanChanges: true, CanSubscribe: true, CanEvents: true}
	if token := principal.Token; token != nil {
		actor.Kind = "app"
		if strings.HasPrefix(token.ID, auth.SaveLinkTokenPrefix) {
			actor.Kind = "save_link"
		}
		actor.GrantID, actor.Label = token.ID, token.Name
		actor.CanRead, actor.CanWrite = token.CanRead, token.CanWrite
		actor.CanChanges, actor.CanSubscribe, actor.CanEvents = token.CanChanges, token.CanSubscribe, token.CanEvents
		actor.PolicyGeneration = strconv.FormatInt(token.PolicyVersion, 10)
		actor.Scope = ""
		if scope, err := fc.FromRelative(token.Prefix); err == nil {
			actor.Scope = scope
		}
	}
	return actor
}

// Never copy raw URLs, search terms, upload signatures, headers, bodies or errors.
func filesAuditAction(r *http.Request) string {
	route := strings.TrimPrefix(r.URL.Path, fc.Base)
	switch route {
	case "":
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			return "metadata"
		case http.MethodPost:
			return "mkdir"
		case http.MethodDelete:
			return "delete"
		}
	case "/content":
		switch r.Method {
		case http.MethodGet:
			return "read"
		case http.MethodHead:
			return "head"
		case http.MethodPut:
			return "put"
		}
	case "/downloads":
		return "download_grant"
	case "/list", "/search", "/move", "/copy", "/thumbnail", "/me", "/capabilities", "/changes", "/subscriptions":
		return strings.TrimPrefix(route, "/")
	}
	switch {
	case route == "/uploads", strings.HasPrefix(route, "/uploads/"):
		switch {
		case strings.HasSuffix(route, "/parts/sign"):
			return "upload_sign"
		case strings.HasSuffix(route, "/commit"):
			return "upload_commit"
		case strings.Contains(route, "/parts/"):
			return "upload_part"
		case r.Method == http.MethodDelete:
			return "upload_abort"
		case r.Method == http.MethodGet:
			return "upload_metadata"
		default:
			return "upload_create"
		}
	case strings.HasPrefix(route, "/operations/"):
		if strings.HasSuffix(route, "/cancel") {
			return "operation_cancel"
		}
		return "operation_metadata"
	case route == "/change-snapshots", strings.HasPrefix(route, "/change-snapshots/"):
		return "change_snapshot"
	case strings.HasPrefix(route, "/subscriptions/"):
		return "subscriptions"
	}
	return "request"
}

func filesAuditAttempt(r *http.Request, principal *auth.Principal, requestID string) *filechanges.Audit {
	audit := &filechanges.Audit{OwnerID: principal.User.ID, Action: filesAuditAction(r), Outcome: "admitted", Actor: filesAuditActor(principal), RequestID: requestID, Origin: "http", ObservedAt: time.Now().UTC()}
	// ParseQuery decodes once; ParsePath preserves literal percent escapes.
	if query, err := url.ParseQuery(r.URL.RawQuery); err == nil {
		if values := query["path"]; len(values) == 1 {
			if path, err := fc.ParsePath(values[0]); err == nil {
				audit.Path = &path
			}
		}
		if values := query["id"]; len(values) == 1 && fc.ValidEntryID(values[0]) {
			id := values[0]
			audit.EntryID = &id
		}
	}
	audit.OperationID = filesAuditOperationID(r.URL.Path)
	return audit
}

func filesAuditOperationID(path string) string {
	if rest, ok := strings.CutPrefix(path, fc.Base+"/operations/"); ok {
		id, _, _ := strings.Cut(rest, "/")
		if fc.ValidEntryID(id) {
			return id
		}
	}
	return ""
}

func filesAuditIncomplete(audit *filechanges.Audit, phase string) {
	// Persistence errors can contain SQL parameters. Log correlation only.
	slog.Error("file audit incomplete", "request_id", audit.RequestID, "phase", phase)
}

func filesAuditCompletion(ctx context.Context, persist filesAuditRecorder, attempt *filechanges.Audit, response *filesResponseWriter, requestErr error) {
	if attempt == nil {
		return
	}
	completion := *attempt
	completion.ID, completion.EffectKey = "", ""
	completion.ObservedAt = time.Now().UTC()
	completion.Status = response.status
	if completion.Status == 0 {
		completion.Status = http.StatusOK
	}
	completion.Outcome = "completed"
	if operationID := filesAuditOperationID(response.Header().Get("Location")); operationID != "" {
		completion.OperationID = operationID
	}
	if requestErr != nil {
		problem := filesProblem(requestErr)
		if fc.ValidEntryID(problem.OperationID) {
			completion.OperationID = problem.OperationID
		}
		completion.ReasonCode = problem.Code
		if !response.started {
			completion.Status = problem.Status
		}
		completion.Outcome = "failed"
		if errors.Is(requestErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			completion.Outcome = "cancelled"
		}
		if response.started {
			completion.Outcome = "interrupted"
		}
	}
	if requestErr == nil && completion.Status >= 400 {
		completion.Outcome = "failed"
	}
	if completion.Status == 401 || completion.Status == 403 || completion.Status == 404 {
		completion.Outcome = "denied"
		allowed := false
		completion.Allowed = &allowed
	} else if completion.Status < 400 && (requestErr == nil || response.started) {
		allowed := true
		completion.Allowed = &allowed
	}
	written := response.bytesWritten
	completion.BytesWritten = &written
	// This is bytes accepted by the writer, never proof of client delivery.
	// Error completions precede Problem Details and count no future bytes.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), filesAuditTimeout)
	defer cancel()
	if _, err := persist(finishCtx, &completion); err != nil {
		filesAuditIncomplete(&completion, "completion")
	}
}

// ownerAuditHandler mounts directly at GET /api/audit and avoids legacy envelopes.
// An owner cookie session is required; a token for that owner grants no access.
func (s *Server) ownerAuditHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		if err := s.handleOwnerAudit(w, r); err != nil {
			writeFilesProblem(w, r, err)
		}
	}
}

func (s *Server) handleOwnerAudit(w http.ResponseWriter, r *http.Request) error {
	credential, err := fc.SelectCredential(r.Header)
	if err != nil {
		return err
	}
	if credential.Source != fc.CookieCredential {
		return fc.NewProblem(403, "forbidden", "An owner session is required to read audit records.")
	}
	verifier := s.filesAuth
	if verifier == nil {
		verifier = s.auth
	}
	if verifier == nil {
		return fc.NewProblem(503, "temporarily_unavailable", "Authentication is not configured.")
	}
	principal, source, err := verifier.AuthenticateFiles(r.Context(), r)
	if err != nil {
		return err
	}
	if principal == nil || principal.User == nil {
		return fc.NewProblem(401, "unauthenticated", "A valid owner session is required.")
	}
	if source != fc.CookieCredential || principal.Token != nil {
		return fc.NewProblem(403, "forbidden", "An owner session is required to read audit records.")
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return fc.NewProblem(400, "invalid_request", "The query encoding is invalid.")
	}
	limit := 50
	for key, values := range query {
		if len(values) != 1 || (key != "limit" && key != "cursor") {
			return fc.NewProblem(400, "invalid_request", "Only one limit and cursor are accepted.")
		}
	}
	if query.Has("limit") {
		limit, err = strconv.Atoi(query.Get("limit"))
		if err != nil || limit < 1 || limit > 100 {
			return fc.NewProblem(400, "invalid_request", "The audit page limit must be between 1 and 100.")
		}
	}
	var before filesAuditCursor
	if query.Has("cursor") {
		if len(query.Get("cursor")) > 512 {
			return fc.NewProblem(400, "invalid_cursor", "The audit cursor is invalid.")
		}
		data, decodeErr := base64.RawURLEncoding.DecodeString(query.Get("cursor"))
		if decodeErr != nil || len(data) > 384 || json.Unmarshal(data, &before) != nil || before.OwnerID != principal.User.ID || before.ObservedAt.IsZero() || !fc.ValidEntryID(before.ID) {
			return fc.NewProblem(400, "invalid_cursor", "The audit cursor is invalid.")
		}
	}
	if s.db == nil {
		return fc.NewProblem(503, "temporarily_unavailable", "Audit storage is unavailable.")
	}
	items, err := filechanges.ReadAuditPage(r.Context(), s.db.Pool(), principal.User.ID, limit+1, before.ObservedAt, before.ID)
	if err != nil {
		return err
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		encoded, _ := json.Marshal(filesAuditCursor{OwnerID: principal.User.ID, ObservedAt: last.ObservedAt, ID: last.ID})
		cursor := base64.RawURLEncoding.EncodeToString(encoded)
		next = &cursor
	}
	return writeFilesJSON(w, 200, fc.NewCollection(items, next))
}

type filesAuditCursor struct {
	OwnerID    string    `json:"owner"`
	ObservedAt time.Time `json:"at"`
	ID         string    `json:"id"`
}
