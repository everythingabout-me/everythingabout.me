package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"everythingabout.me/backend/internal/filechanges"
	fc "everythingabout.me/backend/internal/filecontract"
	"github.com/jackc/pgx/v5"
)

const filesChangesLifetime = 7 * 24 * time.Hour
const filesChangesStreamDuration = 25 * time.Second

// Filter is sealed into every continuation. ID filters follow identity; path
// filters continue observing the location when its occupant is replaced.
type filesChangeFilter struct {
	Target    fc.EntrySelector `json:"target"`
	Kind      string           `json:"kind"`
	Recursive bool             `json:"recursive"`
}

type filesChangeCursor struct {
	Version         int               `json:"v"`
	Epoch           string            `json:"epoch"`
	After           int64             `json:"after"`
	Through         *int64            `json:"through,omitempty"`
	Expires         int64             `json:"expires"`
	Filter          filesChangeFilter `json:"filter"`
	Anchor          fc.Path           `json:"anchor"`
	Terminal        string            `json:"terminal,omitempty"`
	PendingTerminal bool              `json:"pendingTerminal,omitempty"`
	TerminalEventID string            `json:"terminalEventId,omitempty"`
	Refresh         bool              `json:"refresh,omitempty"`
}

type filesChangeItem struct {
	filechanges.Projection
	Reason string `json:"reason,omitempty"`
}

type filesChangesPage struct {
	Items      []filesChangeItem `json:"items"`
	NextCursor string            `json:"nextCursor"`
	HasMore    bool              `json:"hasMore"`
}

type filesSubscription struct {
	ID             string            `json:"id"`
	Filter         filesChangeFilter `json:"filter"`
	State          string            `json:"state"`
	ExpiresAt      time.Time         `json:"expiresAt"`
	ChangesCursor  string            `json:"changesCursor"`
	EventsURL      string            `json:"eventsUrl"`
	epoch          string
	anchor         fc.Path
	start          int64
	signalPosition int64
	signalAnchor   fc.Path
	notificationID string
	endedReason    string
}

func filesChangeInvalidCursor() error {
	return fc.NewProblem(400, "invalid_cursor", "The change cursor is invalid for this credential and root.")
}
func filesChangeRelocate(anchor, before, after fc.Path, key fc.SegmentKey) (fc.Path, bool) {
	if fc.ValidatePath(string(before)) != nil || fc.ValidatePath(string(after)) != nil || !fc.WithinScope(before, anchor, key) {
		return "", false
	}
	parts := func(p fc.Path) []string {
		v := strings.Trim(string(p), "/")
		if v == "" {
			return nil
		}
		return strings.Split(v, "/")
	}
	source, target, selected := parts(before), parts(after), parts(anchor)
	if len(source) > len(selected) {
		return "", false
	}
	relocated, err := fc.ParsePath("/" + strings.Join(append(target, selected[len(source):]...), "/"))
	return relocated, err == nil
}

func filesWatchEnded(reason string) error {
	code := "watch_access_lost"
	detail := "The ID watch ended because access was lost."
	if reason == "deleted" {
		code = "watch_deleted"
		detail = "The ID watch ended because its target was deleted."
	}
	return fc.NewProblem(409, code, detail)
}
func (s *Server) filesWatchEndedItem(f *fileRequest, c filesChangeCursor) filesChangeItem {
	sum := sha256.Sum256([]byte("eam-watch-ended-v1:" + c.Epoch + ":" + c.TerminalEventID + ":" + c.Filter.Target.ID + ":" + c.Terminal))
	id := fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
	return filesChangeItem{Projection: filechanges.Projection{ID: id, Type: "watch_ended", Coverage: "entry", EntryID: &c.Filter.Target.ID}, Reason: c.Terminal}
}
func filesChangeExpired() error {
	return fc.NewProblem(410, "cursor_expired", "The change baseline has expired. Create a new baseline before resuming.")
}
func (f *fileRequest) requireChanges(subscribe bool) error {
	if err := f.RequireRead(f.Scope); err != nil {
		return err
	}
	if token := f.Principal.Token; token != nil {
		if !token.CanChanges || (subscribe && !token.CanSubscribe) {
			return fc.NewProblem(403, "forbidden", "This credential does not permit the requested file change access.")
		}
	}
	return nil
}

// All mutable policy dimensions participate, including the monotonically
// increasing version so removing and restoring a grant cannot revive a cursor.
func filesChangePolicy(f *fileRequest) []byte {
	hidden := append([]string(nil), filesHiddenRoots(f)...)
	sort.Strings(hidden)
	var rights any = "owner"
	if t := f.Principal.Token; t != nil {
		rights = struct {
			Read, Write, Events, Changes, Subscribe bool
			Version                                 int64
		}{t.CanRead, t.CanWrite, t.CanEvents, t.CanChanges, t.CanSubscribe, t.PolicyVersion}
	}
	data, _ := json.Marshal(struct {
		Domain, Owner, Principal, Root, Scope string
		CaseSensitive                         bool
		Hidden                                []string
		Rights                                any
	}{
		"eam-filechanges-v1", f.Principal.User.ID, filePrincipalID(f.Principal), f.ContentNamespace, string(f.Scope), f.Driver.Describe().Names.CaseSensitive, hidden, rights,
	})
	hash := sha256.Sum256(data)
	return hash[:]
}
func (s *Server) filesEncodeChangeCursor(f *fileRequest, c filesChangeCursor) (string, error) {
	aead, _, err := s.filesCursorKeys()
	if err != nil {
		return "", err
	}
	c.Version = 1
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	// Fixed-size plaintext also conceals position digit lengths and page state.
	const paddedBytes = 16384
	if len(payload) > paddedBytes {
		return "", fc.NewProblem(400, "invalid_request", "The subscription selector exceeds the supported cursor size.")
	}
	for len(payload) < paddedBytes {
		payload = append(payload, ' ')
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, payload, filesChangePolicy(f))), nil
}
func (s *Server) filesChangeCheckpoint(f *fileRequest, filter filesChangeFilter, anchor fc.Path, head filechanges.Head, expiresAt time.Time) (string, error) {
	return s.filesEncodeChangeCursor(f, filesChangeCursor{Epoch: head.RootEpoch, After: head.Position, Expires: expiresAt.Unix(), Filter: filter, Anchor: anchor})
}

func (s *Server) filesDecodeChangeCursor(f *fileRequest, token string) (filesChangeCursor, error) {
	var c filesChangeCursor
	if token == "" || len(token) > 32768 {
		return c, filesChangeInvalidCursor()
	}
	aead, _, err := s.filesCursorKeys()
	if err != nil {
		return c, err
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) < aead.NonceSize()+aead.Overhead() {
		return c, filesChangeInvalidCursor()
	}
	data, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], filesChangePolicy(f))
	if err != nil || json.Unmarshal(data, &c) != nil || c.Version != 1 || c.After < 0 || c.Epoch == "" || (c.Through != nil && *c.Through < c.After) {
		return c, filesChangeInvalidCursor()
	}
	if c.Filter.Target.Validate() != nil || (c.Filter.Kind != "file" && c.Filter.Kind != "folder") || (c.Filter.Kind == "file" && c.Filter.Recursive) {
		return c, filesChangeInvalidCursor()
	}
	if c.Expires <= time.Now().Unix() {
		return c, filesChangeExpired()
	}
	return c, nil
}
func filesChangesQuery(r *http.Request) (string, int, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", 0, fc.NewProblem(400, "invalid_request", "The query encoding is invalid.")
	}
	for key, v := range q {
		if len(v) != 1 || (key != "cursor" && key != "limit") {
			return "", 0, fc.NewProblem(400, "invalid_request", "Only one cursor and limit may be supplied.")
		}
	}
	limit := filesDefaultLimit
	if q.Has("limit") {
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > filesMaxLimit {
			return "", 0, fc.NewProblem(400, "invalid_request", "Limit must be between 1 and 1000.")
		}
	}
	if q.Get("cursor") == "" {
		return "", 0, filesChangeInvalidCursor()
	}
	return q.Get("cursor"), limit, nil
}
func (s *Server) handleFilesChanges(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if err := f.requireChanges(false); err != nil {
		return err
	}
	token, limit, err := filesChangesQuery(r)
	if err != nil {
		return err
	}
	c, err := s.filesDecodeChangeCursor(f, token)
	if err != nil {
		return err
	}
	page, err := s.filesReadChanges(r.Context(), f, c, limit)
	if err != nil {
		return err
	}
	return writeFilesJSON(w, http.StatusOK, page)
}

func filesSubscriptionMode(filter filesChangeFilter) string {
	if filter.Target.IsID() {
		return "id"
	}
	return "path"
}
func (s *Server) filesSubscriptionResource(f *fileRequest, sub filesSubscription) (filesSubscription, error) {
	cursor, err := s.filesEncodeChangeCursor(f, filesChangeCursor{Epoch: sub.epoch, After: sub.start, Expires: sub.ExpiresAt.Unix(), Filter: sub.Filter, Anchor: sub.anchor})
	sub.State = "active"
	if sub.endedReason != "" {
		sub.State = "ended"
	}
	sub.ChangesCursor = cursor
	sub.EventsURL = fc.Base + "/subscriptions/" + sub.ID + "/events"
	return sub, err
}
func (s *Server) filesChangesDB() error {
	if s.db == nil {
		return fc.NewProblem(503, "temporarily_unavailable", "The durable change store is unavailable.")
	}
	return nil
}
func filesChangeRootError(err error) error {
	if errors.Is(err, filechanges.ErrRootChanged) {
		return fc.NewProblem(410, "reset_required", "The file root changed. An operator must reset the change stream before resuming.")
	}
	return err
}

// Bind legacy unbound streams in a short write transaction before opening the
// immutable read window. A configured root change never silently reuses history.
func (s *Server) filesBindChangeRoot(ctx context.Context, f *fileRequest) error {
	if err := s.filesChangesDB(); err != nil {
		return err
	}
	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = filechanges.BindRoot(ctx, tx, f.Principal.User.ID, f.ContentNamespace); err != nil {
		return filesChangeRootError(err)
	}
	return tx.Commit(ctx)
}

func (s *Server) handleFilesCreateSubscription(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if err := f.requireChanges(true); err != nil {
		return err
	}
	if err := s.filesChangesDB(); err != nil {
		return err
	}
	var body struct {
		Target    fc.EntrySelector `json:"target"`
		Kind      string           `json:"kind"`
		Recursive bool             `json:"recursive"`
		Delivery  struct {
			Type string `json:"type"`
		} `json:"delivery"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return fc.NewProblem(400, "invalid_request", "A valid subscription resource is required.")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return fc.NewProblem(400, "invalid_request", "Exactly one subscription resource is required.")
	}
	filter := filesChangeFilter{Target: body.Target, Kind: body.Kind, Recursive: body.Recursive}
	if err := filter.Target.Validate(); err != nil {
		return err
	}
	if (filter.Kind != "file" && filter.Kind != "folder") || (filter.Kind == "file" && filter.Recursive) || body.Delivery.Type != "sse" {
		return fc.NewProblem(400, "invalid_request", "Specify a file or folder filter and SSE delivery.")
	}
	if err := s.filesBindChangeRoot(r.Context(), f); err != nil {
		return err
	}
	p, err := s.filesResolveSelector(r.Context(), f, filter.Target)
	if err != nil {
		return err
	}
	metadata, err := s.filesIndexedMetadata(r.Context(), f, p)
	missingPath := false
	if err != nil {
		var problem *fc.Problem
		if filter.Target.IsID() || p == "/" || !errors.As(err, &problem) || problem.Status != http.StatusNotFound {
			return err
		}
		parent, resolveErr := s.filesResolveSelector(r.Context(), f, fc.EntrySelector{Path: fc.Path(path.Dir(string(p)))})
		if resolveErr != nil {
			return resolveErr
		}
		parentMetadata, parentErr := s.filesIndexedMetadata(r.Context(), f, parent)
		if parentErr != nil {
			return parentErr
		}
		if parentMetadata.Kind != "folder" {
			return fc.NewProblem(404, "not_found", "The indexed subscription parent was not found.")
		}
		missingPath = true
	}
	if !missingPath && (metadata.Kind != filter.Kind || (filter.Target.IsID() && metadata.ID != filter.Target.ID)) {
		return fc.NewProblem(409, "target_changed", "The subscription target does not match its declared kind.")
	}
	tx, err := s.db.Pool().BeginTx(r.Context(), pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(r.Context())
	head, err := filechanges.BindRoot(r.Context(), tx, f.Principal.User.ID, f.ContentNamespace)
	if err != nil {
		return filesChangeRootError(err)
	}
	if err = tx.QueryRow(r.Context(), `select head_position from file_change_heads where owner_id=$1::uuid for update`, f.Principal.User.ID).Scan(&head.Position); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRow(r.Context(), `select count(*) from file_subscriptions where owner_id=$1::uuid and principal_id=$2 and expires_at>now()`, f.Principal.User.ID, filePrincipalID(f.Principal)).Scan(&count); err != nil {
		return err
	}
	if count >= 100 {
		return fc.NewProblem(409, "subscription_limit", "At most 100 active subscriptions are supported per credential.")
	}
	sub := filesSubscription{Filter: filter, anchor: p}
	var targetID, targetPath any
	if filter.Target.IsID() {
		targetID = filter.Target.ID
	} else {
		targetPath = string(filter.Target.Path)
	}
	err = tx.QueryRow(r.Context(), `insert into file_subscriptions(owner_id,principal_id,root_namespace,epoch,policy_fingerprint,mode,selector_id,selector_path,kind,recursive,start_position,anchor_path,signal_position,signal_anchor_path)
 values($1::uuid,$2,$3,$4::uuid,$5,$6,$7::uuid,$8,$9,$10,$11,$12,$11,$12)
 returning id::text,epoch::text,start_position,expires_at`, f.Principal.User.ID, filePrincipalID(f.Principal), f.ContentNamespace, head.RootEpoch, hex.EncodeToString(filesChangePolicy(f)), filesSubscriptionMode(filter), targetID, targetPath, filter.Kind, filter.Recursive, head.Position, string(p)).Scan(&sub.ID, &sub.epoch, &sub.start, &sub.ExpiresAt)
	if err != nil {
		return err
	}
	resource, err := s.filesSubscriptionResource(f, sub)
	if err != nil {
		return err
	}
	if err = tx.Commit(r.Context()); err != nil {
		return err
	}
	w.Header().Set("Location", fc.Base+"/subscriptions/"+sub.ID)
	return writeFilesJSON(w, http.StatusCreated, resource)
}

const filesSubscriptionColumns = `id::text,epoch::text,start_position,mode,coalesce(selector_id::text,''),coalesce(selector_path,''),kind,recursive,expires_at,anchor_path,signal_position,coalesce(last_notification_id::text,''),ended_reason,coalesce(signal_anchor_path,anchor_path)`

func filesScanSubscription(row pgx.Row) (filesSubscription, error) {
	var sub filesSubscription
	var mode string
	var p string
	err := row.Scan(&sub.ID, &sub.epoch, &sub.start, &mode, &sub.Filter.Target.ID, &p, &sub.Filter.Kind, &sub.Filter.Recursive, &sub.ExpiresAt, &sub.anchor, &sub.signalPosition, &sub.notificationID, &sub.endedReason, &sub.signalAnchor)
	sub.Filter.Target.Path = fc.Path(p)
	return sub, err
}
func (s *Server) filesLoadSubscription(ctx context.Context, f *fileRequest, id string) (filesSubscription, error) {
	if err := s.filesChangesDB(); err != nil {
		return filesSubscription{}, err
	}
	if (fc.EntrySelector{ID: id}).Validate() != nil {
		return filesSubscription{}, fc.NewProblem(404, "not_found", "The subscription was not found.")
	}
	sub, err := filesScanSubscription(s.db.Pool().QueryRow(ctx, `select `+filesSubscriptionColumns+` from file_subscriptions where id=$1::uuid and owner_id=$2::uuid and principal_id=$3 and root_namespace=$4 and policy_fingerprint=$5`, id, f.Principal.User.ID, filePrincipalID(f.Principal), f.ContentNamespace, hex.EncodeToString(filesChangePolicy(f))))
	if errors.Is(err, pgx.ErrNoRows) {
		return sub, fc.NewProblem(404, "not_found", "The subscription was not found.")
	}
	if err != nil {
		return sub, err
	}
	if !sub.ExpiresAt.After(time.Now()) {
		return sub, filesChangeExpired()
	}
	return sub, nil
}
func (s *Server) handleFilesGetSubscription(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if err := f.requireChanges(true); err != nil {
		return err
	}
	sub, err := s.filesLoadSubscription(r.Context(), f, r.PathValue("id"))
	if err != nil {
		return err
	}
	resource, err := s.filesSubscriptionResource(f, sub)
	if err != nil {
		return err
	}
	return writeFilesJSON(w, http.StatusOK, resource)
}
func (s *Server) handleFilesListSubscriptions(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if err := f.requireChanges(true); err != nil {
		return err
	}
	if err := s.filesChangesDB(); err != nil {
		return err
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q) != 0 {
		return fc.NewProblem(400, "invalid_request", "Subscription listing does not accept filters.")
	}
	rows, err := s.db.Pool().Query(r.Context(), `select `+filesSubscriptionColumns+` from file_subscriptions where owner_id=$1::uuid and principal_id=$2 and root_namespace=$3 and policy_fingerprint=$4 and expires_at>now() order by id limit 101`, f.Principal.User.ID, filePrincipalID(f.Principal), f.ContentNamespace, hex.EncodeToString(filesChangePolicy(f)))
	if err != nil {
		return err
	}
	defer rows.Close()
	items := []filesSubscription{}
	for rows.Next() {
		sub, err := filesScanSubscription(rows)
		if err != nil {
			return err
		}
		sub, err = s.filesSubscriptionResource(f, sub)
		if err != nil {
			return err
		}
		items = append(items, sub)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	// The creation limit keeps the resource collection bounded and complete.
	if len(items) > 100 {
		return fc.NewProblem(503, "temporarily_unavailable", "The subscription collection exceeds the supported limit.")
	}
	return writeFilesJSON(w, http.StatusOK, filesSubscriptionList{Items: items})
}
func (s *Server) handleFilesDeleteSubscription(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if err := f.requireChanges(true); err != nil {
		return err
	}
	sub, err := s.filesLoadSubscription(r.Context(), f, r.PathValue("id"))
	if err != nil {
		return err
	}
	_, err = s.db.Pool().Exec(r.Context(), `delete from file_subscriptions where owner_id=$1::uuid and principal_id=$2 and id=$3::uuid`, f.Principal.User.ID, filePrincipalID(f.Principal), sub.ID)
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// SSE conveys availability only. Every reconnect checks durable facts from the
// subscription's starting position; a wakeup never advances consumer progress.
func (s *Server) handleFilesSubscriptionEvents(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if err := f.requireChanges(true); err != nil {
		return err
	}
	sub, err := s.filesLoadSubscription(r.Context(), f, r.PathValue("id"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), filesChangesStreamDuration)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	started := false
	for {
		if err = s.filesRecheckChangeCredential(ctx, f); err != nil {
			return err
		}
		sub, err = s.filesLoadSubscription(ctx, f, sub.ID)
		if err != nil {
			return err
		}
		sub, readErr := s.filesPollSubscription(ctx, f, sub)
		if readErr != nil {
			if errors.Is(readErr, context.DeadlineExceeded) && started {
				return nil
			}
			return readErr
		}
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Accel-Buffering", "no")
			if _, err = fmt.Fprint(w, ": connected\n\n"); err != nil {
				return err
			}
			if err = http.NewResponseController(w).Flush(); err != nil {
				return err
			}
			started = true
		}
		if sub.notificationID != "" && sub.notificationID != r.Header.Get("Last-Event-ID") {
			if err = s.filesRecheckChangeCredential(ctx, f); err != nil {
				return err
			}
			if _, err = s.filesLoadSubscription(ctx, f, sub.ID); err != nil {
				return err
			}
			data, _ := json.Marshal(filesChangeNotification{SubscriptionID: sub.ID, Type: "changes_available", NotificationID: sub.notificationID})
			if _, err = fmt.Fprintf(w, "id: %s\nevent: changes\ndata: %s\n\n", sub.notificationID, data); err != nil {
				return err
			}
			return http.NewResponseController(w).Flush()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// filesPollSubscription advances only a notification matcher. Consumer cursors
// remain independent. Concurrent pollers coalesce at the same committed head and
// replay the same persisted notification after disconnect or process restart.
func (s *Server) filesPollSubscription(ctx context.Context, f *fileRequest, sub filesSubscription) (filesSubscription, error) {
	if err := s.filesBindChangeRoot(ctx, f); err != nil {
		return sub, err
	}
	if sub.endedReason != "" {
		return sub, nil
	}
	c := filesChangeCursor{Epoch: sub.epoch, After: sub.signalPosition, Expires: sub.ExpiresAt.Unix(), Filter: sub.Filter, Anchor: sub.signalAnchor}
	page, err := s.filesReadChanges(ctx, f, c, 1)
	if err != nil {
		return sub, err
	}
	next, err := s.filesDecodeChangeCursor(f, page.NextCursor)
	if err != nil {
		return sub, err
	}
	position := next.After
	err = s.db.Pool().QueryRow(ctx, `update file_subscriptions
 set signal_position=greatest(signal_position,$6),
 signal_anchor_path=case when signal_position<=$6 then $9 else signal_anchor_path end,
 last_notification_id=case when $7::boolean and signal_position<$6 then gen_random_uuid() else last_notification_id end,
 ended_reason=case when ended_reason<>'' then ended_reason else $8 end
 where id=$1::uuid and owner_id=$2::uuid and principal_id=$3 and root_namespace=$4 and policy_fingerprint=$5 and expires_at>now()
 returning signal_position,coalesce(last_notification_id::text,''),ended_reason,coalesce(signal_anchor_path,anchor_path)`, sub.ID, f.Principal.User.ID, filePrincipalID(f.Principal), f.ContentNamespace, hex.EncodeToString(filesChangePolicy(f)), position, len(page.Items) > 0, next.Terminal, string(next.Anchor)).Scan(&sub.signalPosition, &sub.notificationID, &sub.endedReason, &sub.signalAnchor)
	if errors.Is(err, pgx.ErrNoRows) {
		return sub, fc.NewProblem(404, "not_found", "The subscription was not found.")
	}
	return sub, err
}

// Authentication at request admission is insufficient for a bounded stream:
// compare the durable token policy on every poll and stop on revoke or expiry.
func (s *Server) filesRecheckChangeCredential(ctx context.Context, f *fileRequest) error {
	if f.Principal.Token == nil {
		return nil
	}
	if err := s.filesChangesDB(); err != nil {
		return err
	}
	token := f.Principal.Token
	var allowed bool
	err := s.db.Pool().QueryRow(ctx, `select exists(select 1 from api_tokens where token_id=$1 and user_id=$2::uuid and revoked_at is null and (expires_at is null or expires_at>now()) and can_read and can_changes and can_subscribe and policy_version=$3)`, token.ID, f.Principal.User.ID, token.PolicyVersion).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return fc.NewProblem(403, "forbidden", "The file change grant is no longer active.")
	}
	return nil
}

// A repeatable-read transaction captures the retained floor, committed head and
// matching facts together. Through stays sealed across pages; commits arriving
// after the first page belong to the next window.
func (s *Server) filesReadChanges(ctx context.Context, f *fileRequest, c filesChangeCursor, limit int) (filesChangesPage, error) {
	page := filesChangesPage{Items: []filesChangeItem{}}
	if err := s.filesChangesDB(); err != nil {
		return page, err
	}
	if err := s.filesBindChangeRoot(ctx, f); err != nil {
		return page, err
	}
	if c.Expires <= time.Now().Unix() {
		return page, filesChangeExpired()
	}
	if c.Refresh {
		return page, fc.NewProblem(409, "baseline_required", "The selected view requires a fresh change snapshot.")
	}
	if c.Terminal != "" && !c.PendingTerminal {
		return page, filesWatchEnded(c.Terminal)
	}
	tx, release, err := filechanges.BeginRootRead(ctx, s.db.Pool(), f.Principal.User.ID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return page, err
	}
	defer release()
	defer tx.Rollback(ctx)
	if err = filechanges.RequireRoot(ctx, tx, f.Principal.User.ID, f.ContentNamespace); err != nil {
		return page, filesChangeRootError(err)
	}
	head, err := filechanges.ReadHead(ctx, tx, f.Principal.User.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return page, filesChangeExpired()
	}
	if err != nil {
		return page, err
	}
	if c.Epoch != head.RootEpoch || c.After < head.RetainedAfter || c.After > head.Position {
		return page, filesChangeExpired()
	}
	through := head.Position
	if c.Through != nil {
		through = *c.Through
		if through > head.Position {
			return page, filesChangeExpired()
		}
	}
	watchPath := c.Filter.Target.Path
	if c.PendingTerminal {
		page.Items = append(page.Items, s.filesWatchEndedItem(f, c))
		c.PendingTerminal = false
		page.NextCursor, err = s.filesEncodeChangeCursor(f, c)
		if err != nil {
			return page, err
		}
		if err = tx.Commit(ctx); err != nil {
			return page, err
		}
		return page, nil
	}
	if c.Filter.Target.IsID() {
		watchPath = c.Anchor
		var relative string
		err = tx.QueryRow(ctx, `select path from file_entries where owner_id=$1::uuid and id=$2::uuid union all select '' from file_identity_roots where owner_id=$1::uuid and id=$2::uuid limit 1`, f.Principal.User.ID, c.Filter.Target.ID).Scan(&relative)
		if err == nil {
			watchPath, err = fc.FromRelative(relative)
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return page, err
		}
		if err == nil && f.RequireRead(watchPath) != nil {
			watchPath = c.Anchor
		}
	}
	if fc.ValidatePath(string(watchPath)) != nil {
		return page, filesChangeInvalidCursor()
	}
	version := "nfc-sensitive-v1"
	if !f.Driver.Describe().Names.CaseSensitive {
		version = "nfc-fold-v1"
	}
	hidden := make([]string, 0, len(filesHiddenRoots(f)))
	for _, p := range filesHiddenRoots(f) {
		absolute, e := fc.FromRelative(p)
		if e != nil {
			return page, e
		}
		hidden = append(hidden, string(absolute))
	}
	queryAfter := c.After
	var positions []int64
	for {
		candidateLimit := limit + 1
		if c.Filter.Target.IsID() {
			candidateLimit = 1
			watchPath = c.Anchor
		}
		rows, err := tx.Query(ctx, filesChangesWindowSQL, f.Principal.User.ID, queryAfter, through, string(f.Scope), version, hidden, filesSubscriptionMode(c.Filter), c.Filter.Target.ID, string(watchPath), c.Filter.Kind == "folder", c.Filter.Recursive, candidateLimit, string(c.Anchor))
		if err != nil {
			return page, err
		}
		found := false
		for rows.Next() {
			found = true
			var change filechanges.Change
			var before, after, folders []byte
			var refresh bool
			var rawBefore, rawAfter fc.Path
			if err = rows.Scan(&change.ID, &change.Position, &change.Kind, &change.Coverage, &before, &after, &refresh, &change.Origin, &change.ObservedAt, &rawBefore, &rawAfter, &folders); err != nil {
				rows.Close()
				return page, err
			}
			if len(before) > 0 {
				if err = json.Unmarshal(before, &change.Before); err != nil {
					rows.Close()
					return page, err
				}
			}
			if len(after) > 0 {
				if err = json.Unmarshal(after, &change.After); err != nil {
					rows.Close()
					return page, err
				}
			}
			if len(folders) > 0 {
				if err = json.Unmarshal(folders, &change.AffectedFolders); err != nil {
					rows.Close()
					return page, err
				}
			}
			settledKind := change.Kind
			if refresh && change.Before == nil && change.After == nil {
				change.Kind = "invalidated"
				change.ImpactPaths = []fc.Path{watchPath}
			}
			if change.Kind == "invalidated" {
				change.ImpactPaths = []fc.Path{watchPath}
			}
			item, visible := filechanges.Project(change, f.Scope, filesPathKey(f))
			if !visible {
				rows.Close()
				return page, fc.NewProblem(503, "temporarily_unavailable", "A change projection could not be constructed.")
			}
			baselineArrival := settledKind == "moved" && item.Coverage == "subtree" && item.Before == nil && item.After != nil
			if baselineArrival {
				arrivalPath := item.After.Path
				item.Type = "invalidated"
				item.Path = arrivalPath
				item.ImpactPaths = []fc.Path{arrivalPath}
				item.After = nil
				item.Before = nil
				item.EntryID = nil
			}
			terminalReason := ""
			ancestorReason := ""
			if c.Filter.Target.IsID() && item.Type == "invalidated" && rawBefore != "" && fc.WithinScope(rawBefore, c.Anchor, filesPathKey(f)) {
				if settledKind == "deleted" {
					ancestorReason = "deleted"
				}
				if settledKind == "moved" {
					if relocated, ok := filesChangeRelocate(c.Anchor, rawBefore, rawAfter, filesPathKey(f)); ok && f.RequireRead(relocated) != nil {
						ancestorReason = "access_lost"
					}
				}
			}
			if c.Filter.Target.IsID() {
				if item.Type == "remove" && item.EntryID != nil && *item.EntryID == c.Filter.Target.ID {
					terminalReason = "access_lost"
					if change.Kind == "deleted" {
						terminalReason = "deleted"
					}
				} else if ancestorReason != "" && item.Type == "invalidated" && (settledKind == "moved" || settledKind == "deleted") {
					terminalReason = "access_lost"
					if settledKind == "deleted" {
						terminalReason = "deleted"
					}
					item.Type = "remove"
					item.EntryID = &c.Filter.Target.ID
					item.Path = c.Anchor
					item.Coverage = "entry"
					item.ImpactPaths = nil
				}
			}
			filesFilterChangeFolders(f, c, watchPath, &item)
			page.Items = append(page.Items, filesChangeItem{Projection: item})
			positions = append(positions, change.Position)
			if len(page.Items) <= limit && item.Type == "invalidated" && (c.Filter.Target.IsID() || baselineArrival) {
				c.Refresh = true
				break
			}
			if c.Filter.Target.IsID() && len(page.Items) <= limit && item.After != nil && item.After.ID == c.Filter.Target.ID {
				c.Anchor = item.After.Path
			}
			if terminalReason != "" && len(page.Items) <= limit {
				c.Terminal = terminalReason
				c.TerminalEventID = change.ID
				c.PendingTerminal = len(page.Items) == limit
				if !c.PendingTerminal {
					page.Items = append(page.Items, s.filesWatchEndedItem(f, c))
					positions = append(positions, change.Position)
				}
				break
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return page, err
		}
		if !c.Filter.Target.IsID() || !found || len(page.Items) > limit || c.Terminal != "" || c.Refresh {
			break
		}
		queryAfter = positions[len(positions)-1]
	}
	page.HasMore = len(page.Items) > limit || c.PendingTerminal
	if page.HasMore {
		page.Items = page.Items[:limit]
		c.After = positions[limit-1]
		c.Through = &through
	} else {
		c.After = through
		c.Through = nil
	}
	page.NextCursor, err = s.filesEncodeChangeCursor(f, c)
	if err != nil {
		return page, err
	}
	if err = tx.Commit(ctx); err != nil {
		return page, err
	}
	return page, nil
}

// Aggregate patches use the watch's view as well as the credential's scope.
// They never make an otherwise excluded event eligible for a feed page.
func filesFilterChangeFolders(f *fileRequest, c filesChangeCursor, current fc.Path, item *filechanges.Projection) {
	if item.Before != nil {
		redacted := filesRedactParent(f, *item.Before)
		item.Before = &redacted
	}
	if item.After != nil {
		redacted := filesRedactParent(f, *item.After)
		item.After = &redacted
	}
	folders := item.AffectedFolders
	item.AffectedFolders = nil
	if c.Filter.Kind != "folder" {
		return
	}
	roots := []fc.Path{c.Filter.Target.Path}
	if c.Filter.Target.IsID() {
		roots = []fc.Path{current}
		if item.Before != nil && item.Before.ID == c.Filter.Target.ID {
			roots = append(roots, item.Before.Path)
		}
		if item.After != nil && item.After.ID == c.Filter.Target.ID {
			roots = append(roots, item.After.Path)
		}
		if item.Type == "remove" {
			roots = append(roots, c.Anchor)
		}
	}
	key := filesPathKey(f)
	for _, folder := range folders {
		if folder.Kind != "folder" || f.RequireRead(folder.Path) != nil {
			continue
		}
		hidden := false
		for _, relative := range filesHiddenRoots(f) {
			p, err := fc.FromRelative(relative)
			if err == nil && fc.WithinScope(p, folder.Path, key) {
				hidden = true
				break
			}
		}
		if hidden {
			continue
		}
		matches := false
		for _, root := range roots {
			if fc.ValidatePath(string(root)) != nil || f.RequireRead(root) != nil {
				continue
			}
			if filesEqualPath(f, root, folder.Path) || (c.Filter.Recursive && fc.WithinScope(root, folder.Path, key)) || (!c.Filter.Recursive && filesEqualPath(f, root, folder.ParentPath)) {
				matches = true
				break
			}
		}
		if matches {
			item.AffectedFolders = append(item.AffectedFolders, filesRedactParent(f, folder))
		}
	}
}

// Scope and target match on the SAME endpoint. A move from an authorized
// unrelated endpoint into a hidden watched endpoint cannot disclose either side.
// Ancestor subtree effects require a baseline refresh rather than descendant
// expansion; refresh contains no concealed ancestor ID or location.
const filesChangesWindowSQL = `with policy as (
 select $1::uuid owner_id,$2::bigint after_position,$3::bigint through_position,$4::text scope_path,
 $5::text comparison_version,$6::text[] hidden_paths,$7::text selector_mode,$8::text selected_id,
 $9::text selected_path,$10::boolean folder,$11::boolean recursive,$13::text selected_anchor
), projected as (
 select changes.id,changes.position,changes.kind,changes.coverage,changes.origin,changes.observed_at,changes.affected_folders,
 changes.before_snapshot->>'path' raw_before,changes.after_snapshot->>'path' raw_after,
 case when file_subscription_visible(p.scope_path,changes.before_snapshot->>'path',p.hidden_paths,p.comparison_version)
   and file_subscription_matches(changes.before_snapshot,changes.before_ancestors,p.selector_mode,p.selected_id,p.selected_path,p.folder,p.recursive,p.comparison_version)
   then changes.before_snapshot end before_snapshot,
 case when file_subscription_visible(p.scope_path,changes.after_snapshot->>'path',p.hidden_paths,p.comparison_version)
   and file_subscription_matches(changes.after_snapshot,changes.after_ancestors,p.selector_mode,p.selected_id,p.selected_path,p.folder,p.recursive,p.comparison_version)
   then changes.after_snapshot end after_snapshot,
 coalesce((file_subscription_visible(p.scope_path,p.selected_path,p.hidden_paths,p.comparison_version) and (
   (changes.coverage='subtree' and (
     file_changes_within(changes.before_snapshot->>'path',p.selected_path,p.comparison_version)
     or file_changes_within(changes.after_snapshot->>'path',p.selected_path,p.comparison_version)
      or file_changes_within(changes.before_snapshot->>'path',p.selected_anchor,p.comparison_version)
      or file_changes_within(changes.after_snapshot->>'path',p.selected_anchor,p.comparison_version)))
   or (changes.kind='invalidated' and exists(select 1 from jsonb_array_elements_text(changes.impact_paths) impact(path)
     where file_subscription_visible(p.scope_path,impact.path,p.hidden_paths,p.comparison_version)
     and (file_changes_within(impact.path,p.selected_path,p.comparison_version)
       or (p.folder and p.recursive and file_changes_within(p.selected_path,impact.path,p.comparison_version))
       or (p.folder and not p.recursive and file_identity_comparison(regexp_replace(impact.path,'/[^/]+$',''),p.comparison_version)=file_identity_comparison(p.selected_path,p.comparison_version))))
 ))),false) as needs_refresh
 from file_changes changes cross join policy p
 where changes.owner_id=p.owner_id and changes.position>p.after_position and changes.position<=p.through_position
)
select id::text,position,kind,coverage,before_snapshot,after_snapshot,needs_refresh,origin,observed_at,coalesce(raw_before,''),coalesce(raw_after,''),affected_folders from projected
where before_snapshot is not null or after_snapshot is not null or needs_refresh
order by position limit $12`

func (s *Server) handleFilesRenewSubscription(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if err := f.requireChanges(true); err != nil {
		return err
	}
	sub, err := s.filesLoadSubscription(r.Context(), f, r.PathValue("id"))
	if err != nil {
		return err
	}
	head, err := filechanges.ReadHead(r.Context(), s.db.Pool(), f.Principal.User.ID)
	if err != nil {
		return err
	}
	if head.RootEpoch != sub.epoch || sub.start < head.RetainedAfter {
		return filesChangeExpired()
	}
	err = s.db.Pool().QueryRow(r.Context(), `update file_subscriptions set expires_at=now()+interval '7 days' where owner_id=$1::uuid and principal_id=$2 and id=$3::uuid and policy_fingerprint=$4 and expires_at>now() returning expires_at`, f.Principal.User.ID, filePrincipalID(f.Principal), sub.ID, hex.EncodeToString(filesChangePolicy(f))).Scan(&sub.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return filesChangeExpired()
	}
	if err != nil {
		return err
	}
	resource, err := s.filesSubscriptionResource(f, sub)
	if err != nil {
		return err
	}
	return writeFilesJSON(w, http.StatusOK, resource)
}
