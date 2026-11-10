package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"everythingabout.me/backend/internal/filechanges"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/nodes"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const filesSnapshotLifetime = 24 * time.Hour
const filesSnapshotMaxEntries = 100000
const filesSnapshotMaxBytes = 64 << 20
const filesSnapshotPrincipalLimit = 10

type filesChangeSnapshot struct {
	ID            string            `json:"id"`
	State         string            `json:"state"`
	Filter        filesChangeFilter `json:"filter"`
	TotalEntries  int               `json:"totalEntries"`
	EntriesURL    string            `json:"entriesUrl"`
	ChangesCursor string            `json:"changesCursor"`
	CreatedAt     time.Time         `json:"createdAt"`
	ExpiresAt     time.Time         `json:"expiresAt"`
	epoch         string
	head          int64
	anchor        fc.Path
	targetID      string
}
type filesSnapshotPageCursor struct {
	Version    int    `json:"v"`
	SnapshotID string `json:"snapshot"`
	After      int    `json:"after"`
	Expires    int64  `json:"expires"`
}

func filesSnapshotNotFound() error {
	return fc.NewProblem(404, "not_found", "The change snapshot was not found.")
}
func filesSnapshotExpired() error {
	return fc.NewProblem(410, "snapshot_expired", "The snapshot baseline has expired. Create a new snapshot before resuming.")
}
func filesSnapshotError(err error) error {
	if errors.Is(err, filechanges.ErrRootChanged) {
		return fc.NewProblem(503, "change_root_reset_required", "The configured file root changed. Reset the change baseline explicitly before creating snapshots.")
	}
	var database *pgconn.PgError
	if errors.As(err, &database) && (database.Code == "40001" || database.Code == "57014" || database.Code == "55P03") || errors.Is(err, context.DeadlineExceeded) {
		return fc.NewProblem(503, "temporarily_unavailable", "A complete snapshot could not be captured within the supported request window.")
	}
	return err
}
func filesSnapshotPolicy(f *fileRequest) []byte {
	return append([]byte("eam-file-change-snapshot-page-v1:"), filesChangePolicy(f)...)
}
func (s *Server) filesEncodeSnapshotCursor(f *fileRequest, c filesSnapshotPageCursor) (string, error) {
	aead, _, err := s.filesCursorKeys()
	if err != nil {
		return "", err
	}
	c.Version = 1
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	// Conceal ordinal digit lengths as well as the private page position.
	if len(data) > 256 {
		return "", filesChangeInvalidCursor()
	}
	data = append(data, bytes.Repeat([]byte(" "), 256-len(data))...)
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, data, filesSnapshotPolicy(f))), nil
}
func (s *Server) filesDecodeSnapshotCursor(f *fileRequest, id, token string) (filesSnapshotPageCursor, error) {
	var c filesSnapshotPageCursor
	if len(token) > 2048 {
		return c, filesChangeInvalidCursor()
	}
	aead, _, err := s.filesCursorKeys()
	if err != nil {
		return c, err
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) != aead.NonceSize()+256+aead.Overhead() {
		return c, filesChangeInvalidCursor()
	}
	data, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], filesSnapshotPolicy(f))
	if err != nil || json.Unmarshal(data, &c) != nil || c.Version != 1 || c.SnapshotID != id || c.After < 0 || c.After >= filesSnapshotMaxEntries {
		return c, filesChangeInvalidCursor()
	}
	if c.Expires <= time.Now().Unix() {
		return c, filesSnapshotExpired()
	}
	return c, nil
}
func filesSnapshotQuery(r *http.Request) (string, int, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", 0, fc.NewProblem(400, "invalid_request", "The query encoding is invalid.")
	}
	for key, values := range q {
		if len(values) != 1 || (key != "cursor" && key != "limit") {
			return "", 0, fc.NewProblem(400, "invalid_request", "Only one cursor and limit may be supplied.")
		}
	}
	if q.Has("cursor") && q.Get("cursor") == "" {
		return "", 0, filesChangeInvalidCursor()
	}
	limit := filesDefaultLimit
	if q.Has("limit") {
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > filesMaxLimit {
			return "", 0, fc.NewProblem(400, "invalid_request", "Limit must be between 1 and 1000.")
		}
	}
	return q.Get("cursor"), limit, nil
}

// Request admission authenticates every page; this last check also prevents a
// long materialization from returning authority after a concurrent token revoke.
func (s *Server) filesRecheckSnapshotCredential(ctx context.Context, f *fileRequest) error {
	if err := f.requireChanges(false); err != nil {
		return err
	}
	if f.Principal.Token == nil {
		return nil
	}
	token := f.Principal.Token
	var allowed bool
	err := s.db.Pool().QueryRow(ctx, `select exists(select 1 from api_tokens where token_id=$1 and user_id=$2::uuid
 and revoked_at is null and (expires_at is null or expires_at>clock_timestamp())
 and can_read and can_changes and policy_version=$3 and prefix=$4
 and can_write=$5 and can_events=$6 and can_subscribe=$7)`, token.ID, f.Principal.User.ID, token.PolicyVersion, token.Prefix, token.CanWrite, token.CanEvents, token.CanSubscribe).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return fc.NewProblem(403, "forbidden", "The file change grant is no longer active.")
	}
	return nil
}
func filesSnapshotScope(f *fileRequest, p fc.Path) error {
	if err := f.RequireRead(p); err != nil {
		return err
	}
	for _, hidden := range filesHiddenRoots(f) {
		hp, err := fc.FromRelative(hidden)
		if err != nil {
			return err
		}
		if fc.WithinScope(hp, p, filesPathKey(f)) {
			return fc.NewProblem(404, "not_found", "The snapshot target was not found.")
		}
	}
	return nil
}
func filesSnapshotResolveTarget(ctx context.Context, tx pgx.Tx, f *fileRequest, selector fc.EntrySelector) (fc.Path, error) {
	if err := selector.Validate(); err != nil {
		return "", err
	}
	if !selector.IsID() {
		return selector.Path, filesSnapshotScope(f, selector.Path)
	}
	var relative string
	err := tx.QueryRow(ctx, `select path from file_entries where owner_id=$1::uuid and id=$2::uuid
 union all select '' from file_identity_roots where owner_id=$1::uuid and id=$2::uuid limit 1`, f.Principal.User.ID, selector.ID).Scan(&relative)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fc.NewProblem(404, "not_found", "The snapshot target was not found.")
	}
	if err != nil {
		return "", err
	}
	p, err := fc.FromRelative(relative)
	if err != nil {
		return "", err
	}
	return p, filesSnapshotScope(f, p)
}

// Native effects that may already have changed a tree cannot be represented as
// a complete indexed baseline. Queued work and unpublished uploads are safe:
// their eventual settlement and change facts occur strictly after this head.
func filesSnapshotCheckGuards(ctx context.Context, tx pgx.Tx, f *fileRequest, p fc.Path, folder bool) error {
	version := "nfc-sensitive-v1"
	if !f.Driver.Describe().Names.CaseSensitive {
		version = "nfc-fold-v1"
	}
	var id string
	err := tx.QueryRow(ctx, `select guard.operation_id::text from file_operation_guards guard
 join file_operations operation on operation.id=guard.operation_id and operation.owner_id=guard.owner_id
 where guard.owner_id=$1::uuid
 and (operation.snapshot->>'State'='outcome_unknown' or operation.snapshot->>'Phase' in ('effect_started','native_applied'))
 and (file_identity_comparison(guard.native_path,$4)=file_identity_comparison($2,$4)
 or (guard.subtree and (guard.native_path='' or file_changes_within('/'||guard.native_path,'/'||$2,$4)))
 or ($3::boolean and file_changes_within('/'||$2,'/'||guard.native_path,$4)))
 order by guard.operation_id,guard.ordinal limit 1`, f.Principal.User.ID, p.Relative(), folder, version).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	problem := fc.NewProblem(409, "operation_in_progress", "A native operation must settle before this snapshot can be captured.")
	// Only disclose an operation whose creator is this credential or the owner.
	// The caller can retry the baseline without learning another token's work ID.
	if f.Principal.Token == nil {
		problem.OperationID = id
	}
	return problem
}

// The capture window is deliberate and documented (25 s here, 20 s of SQL statement
// timeout, both in docs/api/api.md): the snapshot is synchronous and bounded instead of
// becoming an Operation. A capture that does fail must still be traceable from the log,
// which is why every path inside reports its code and cause chain — the client only ever
// sees a generic temporarily_unavailable.
func (s *Server) handleFilesCreateChangeSnapshot(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	err := s.captureChangeSnapshot(w, r, f)
	if err != nil {
		code := "error"
		var problem *fc.Problem
		if errors.As(err, &problem) {
			code = problem.Code
		}
		slog.Warn("change snapshot not created", "owner", f.Principal.User.ID, "code", code, "cause", errorCauseChain(err))
	}
	return err
}

func (s *Server) captureChangeSnapshot(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if err := f.requireChanges(false); err != nil {
		return err
	}
	if err := s.filesChangesDB(); err != nil {
		return err
	}
	if r.URL.RawQuery != "" {
		return fc.NewProblem(400, "invalid_request", "Snapshot creation accepts its selector in the JSON body only.")
	}
	var filter filesChangeFilter
	if err := strictFilesJSON(r, &filter); err != nil {
		return err
	}
	if err := filter.Target.Validate(); err != nil {
		return err
	}
	if filter.Kind != "file" && filter.Kind != "folder" || filter.Kind == "file" && filter.Recursive {
		return fc.NewProblem(400, "invalid_request", "Specify a file or folder snapshot filter.")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	// First use may establish a previously unbound owner, but an existing
	// namespace never follows config changes without an explicit operator reset.
	binding, err := s.db.Pool().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return filesSnapshotError(err)
	}
	if _, err = filechanges.BindRoot(ctx, binding, f.Principal.User.ID, f.ContentNamespace); err != nil {
		_ = binding.Rollback(context.WithoutCancel(ctx))
		return filesSnapshotError(err)
	}
	if err = binding.Commit(ctx); err != nil {
		return filesSnapshotError(err)
	}
	// Acquire the root reader lock before RepeatableRead starts. Waiting for an
	// operator reset must never leave us reading its pre-reset MVCC snapshot.
	tx, release, err := filechanges.BeginRootRead(ctx, s.db.Pool(), f.Principal.User.ID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return filesSnapshotError(err)
	}
	defer release()
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err = tx.Exec(ctx, `set local statement_timeout='20s'`); err != nil {
		return filesSnapshotError(err)
	}
	if err = filechanges.RequireRoot(ctx, tx, f.Principal.User.ID, f.ContentNamespace); err != nil {
		return filesSnapshotError(err)
	}
	head, err := filechanges.ReadHead(ctx, tx, f.Principal.User.ID)
	if err != nil {
		return filesSnapshotError(err)
	}
	// A changed concurrent admission row causes repeatable-read serialization
	// failure rather than letting two requests count the same old collection.
	if _, err = tx.Exec(ctx, `insert into file_change_snapshot_admissions(owner_id,principal_id) values($1::uuid,$2)
 on conflict(owner_id,principal_id) do update set generation=file_change_snapshot_admissions.generation+1`, f.Principal.User.ID, filePrincipalID(f.Principal)); err != nil {
		return filesSnapshotError(err)
	}
	// Expiry also releases materialized metadata when the credential next creates
	// a baseline. The ten-resource admission bound keeps this cleanup bounded.
	if _, err = tx.Exec(ctx, `delete from file_change_snapshots where owner_id=$1::uuid and principal_id=$2 and id in
 (select id from file_change_snapshots where owner_id=$1::uuid and principal_id=$2 and expires_at<=clock_timestamp() order by expires_at,id limit 10)`, f.Principal.User.ID, filePrincipalID(f.Principal)); err != nil {
		return filesSnapshotError(err)
	}
	var count int
	if err = tx.QueryRow(ctx, `select count(*) from file_change_snapshots where owner_id=$1::uuid and principal_id=$2 and expires_at>clock_timestamp()`, f.Principal.User.ID, filePrincipalID(f.Principal)).Scan(&count); err != nil {
		return filesSnapshotError(err)
	}
	if count >= filesSnapshotPrincipalLimit {
		return fc.NewProblem(409, "snapshot_limit", "At most ten active change snapshots are supported per credential.")
	}
	anchor, err := filesSnapshotResolveTarget(ctx, tx, f, filter.Target)
	if err != nil {
		return filesSnapshotError(err)
	}
	if err = filesSnapshotCheckGuards(ctx, tx, f, anchor, filter.Kind == "folder"); err != nil {
		return filesSnapshotError(err)
	}
	// This root head and the index reads use the same MVCC snapshot. Producers
	// commit both their canonical index transition and Append in one transaction.
	items, err := nodes.CaptureFileSnapshot(ctx, tx, f.Principal.User.ID, f.ContentNamespace, anchor.Relative(), filter.Recursive, filesSnapshotMaxEntries, filesHiddenRoots(f)...)
	if err != nil {
		return filesSnapshotError(err)
	}
	if items[0].Kind != filter.Kind || filter.Target.IsID() && items[0].ID != filter.Target.ID {
		return fc.NewProblem(409, "target_changed", "The snapshot target does not match its declared kind.")
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `select clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	snapshot := filesChangeSnapshot{State: "ready", Filter: filter, TotalEntries: len(items), CreatedAt: now.UTC(), ExpiresAt: now.Add(filesSnapshotLifetime).UTC(), epoch: head.RootEpoch, head: head.Position, anchor: anchor, targetID: items[0].ID}
	snapshot.ChangesCursor, err = s.filesChangeCheckpoint(f, filter, anchor, head, snapshot.ExpiresAt)
	if err != nil {
		return err
	}
	var selectorID, selectorPath any
	if filter.Target.IsID() {
		selectorID = filter.Target.ID
	} else {
		selectorPath = string(filter.Target.Path)
	}
	err = tx.QueryRow(ctx, `insert into file_change_snapshots(owner_id,principal_id,root_namespace,policy_fingerprint,epoch,head_position,
 selector_mode,selector_id,selector_path,target_id,anchor_path,kind,recursive,entry_count,changes_cursor,created_at,expires_at)
 values($1::uuid,$2,$3,$4,$5::uuid,$6,$7,$8::uuid,$9,$10::uuid,$11,$12,$13,$14,$15,$16,$17) returning id::text`,
		f.Principal.User.ID, filePrincipalID(f.Principal), f.ContentNamespace, hex.EncodeToString(filesChangePolicy(f)), head.RootEpoch, head.Position,
		filesSubscriptionMode(filter), selectorID, selectorPath, items[0].ID, string(anchor), filter.Kind, filter.Recursive, len(items), snapshot.ChangesCursor, snapshot.CreatedAt, snapshot.ExpiresAt).Scan(&snapshot.ID)
	if err != nil {
		return filesSnapshotError(err)
	}
	rows := make([][]any, 0, len(items))
	totalBytes := 0
	for ordinal, item := range items {
		encoded, err := json.Marshal(item)
		if err != nil {
			return err
		}
		totalBytes += len(encoded)
		if totalBytes > filesSnapshotMaxBytes {
			return fc.NewProblem(503, "snapshot_too_large", "The complete snapshot exceeds the supported metadata size.")
		}
		rows = append(rows, []any{f.Principal.User.ID, snapshot.ID, ordinal, json.RawMessage(encoded)})
	}
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{"file_change_snapshot_entries"}, []string{"owner_id", "snapshot_id", "ordinal", "metadata"}, pgx.CopyFromRows(rows)); err != nil {
		return filesSnapshotError(err)
	}
	if err = s.filesRecheckSnapshotCredential(ctx, f); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return filesSnapshotError(err)
	}
	snapshot.EntriesURL = fc.Base + "/change-snapshots/" + snapshot.ID
	w.Header().Set("Location", snapshot.EntriesURL)
	return writeFilesJSON(w, http.StatusCreated, snapshot)
}

const filesSnapshotColumns = `id::text,epoch::text,head_position,coalesce(selector_id::text,''),coalesce(selector_path,''),target_id::text,anchor_path,kind,recursive,entry_count,changes_cursor,created_at,expires_at`

func filesScanSnapshot(row pgx.Row) (filesChangeSnapshot, error) {
	var snapshot filesChangeSnapshot
	var selectedPath string
	err := row.Scan(&snapshot.ID, &snapshot.epoch, &snapshot.head, &snapshot.Filter.Target.ID, &selectedPath, &snapshot.targetID, &snapshot.anchor, &snapshot.Filter.Kind, &snapshot.Filter.Recursive, &snapshot.TotalEntries, &snapshot.ChangesCursor, &snapshot.CreatedAt, &snapshot.ExpiresAt)
	snapshot.Filter.Target.Path = fc.Path(selectedPath)
	snapshot.State = "ready"
	snapshot.EntriesURL = fc.Base + "/change-snapshots/" + snapshot.ID
	return snapshot, err
}
func (s *Server) filesLoadChangeSnapshot(ctx context.Context, tx pgx.Tx, f *fileRequest, id string) (filesChangeSnapshot, error) {
	if !fc.ValidEntryID(id) {
		return filesChangeSnapshot{}, filesSnapshotNotFound()
	}
	snapshot, err := filesScanSnapshot(tx.QueryRow(ctx, `select `+filesSnapshotColumns+` from file_change_snapshots
 where id=$1::uuid and owner_id=$2::uuid and principal_id=$3 and root_namespace=$4 and policy_fingerprint=$5`, id, f.Principal.User.ID, filePrincipalID(f.Principal), f.ContentNamespace, hex.EncodeToString(filesChangePolicy(f))))
	if errors.Is(err, pgx.ErrNoRows) {
		return snapshot, filesSnapshotNotFound()
	}
	if err != nil {
		return snapshot, err
	}
	if !snapshot.ExpiresAt.After(time.Now()) {
		return snapshot, filesSnapshotExpired()
	}
	if err = filechanges.RequireRoot(ctx, tx, f.Principal.User.ID, f.ContentNamespace); err != nil {
		if errors.Is(err, filechanges.ErrRootChanged) {
			return snapshot, filesSnapshotExpired()
		}
		return snapshot, err
	}
	head, err := filechanges.ReadHead(ctx, tx, f.Principal.User.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return snapshot, filesSnapshotExpired()
	}
	if err != nil {
		return snapshot, err
	}
	if head.RootEpoch != snapshot.epoch || head.RetainedAfter > snapshot.head {
		return snapshot, filesSnapshotExpired()
	}
	if err = filesSnapshotScope(f, snapshot.anchor); err != nil {
		return snapshot, err
	}
	var relative string
	err = tx.QueryRow(ctx, `select path from file_entries where owner_id=$1::uuid and id=$2::uuid
 union all select '' from file_identity_roots where owner_id=$1::uuid and id=$2::uuid limit 1`, f.Principal.User.ID, snapshot.targetID).Scan(&relative)
	if err == nil {
		current, e := fc.FromRelative(relative)
		if e != nil {
			return snapshot, e
		}
		if e = filesSnapshotScope(f, current); e != nil {
			return snapshot, e
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return snapshot, err
	}
	return snapshot, nil
}
func (s *Server) handleFilesGetChangeSnapshot(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if err := f.requireChanges(false); err != nil {
		return err
	}
	if err := s.filesChangesDB(); err != nil {
		return err
	}
	token, limit, err := filesSnapshotQuery(r)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	tx, release, err := filechanges.BeginRootRead(r.Context(), s.db.Pool(), f.Principal.User.ID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return filesSnapshotError(err)
	}
	defer release()
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	snapshot, err := s.filesLoadChangeSnapshot(r.Context(), tx, f, id)
	if err != nil {
		return filesSnapshotError(err)
	}
	after := -1
	if token != "" {
		cursor, e := s.filesDecodeSnapshotCursor(f, id, token)
		if e != nil {
			return e
		}
		if cursor.Expires != snapshot.ExpiresAt.Unix() || cursor.After >= snapshot.TotalEntries {
			return filesChangeInvalidCursor()
		}
		after = cursor.After
	}
	rows, err := tx.Query(r.Context(), `select ordinal,metadata from file_change_snapshot_entries where owner_id=$1::uuid and snapshot_id=$2::uuid and ordinal>$3 order by ordinal limit $4`, f.Principal.User.ID, id, after, limit+1)
	if err != nil {
		return err
	}
	items := []fc.Metadata{}
	ordinals := []int{}
	for rows.Next() {
		var ordinal int
		var encoded []byte
		if err = rows.Scan(&ordinal, &encoded); err != nil {
			rows.Close()
			return err
		}
		if ordinal != after+len(items)+1 {
			rows.Close()
			return fc.NewProblem(503, "snapshot_incomplete", "The saved snapshot is incomplete. Create a new baseline.")
		}
		var item fc.Metadata
		if err = json.Unmarshal(encoded, &item); err != nil {
			rows.Close()
			return err
		}
		if err = filesSnapshotScope(f, item.Path); err != nil {
			rows.Close()
			return err
		}
		items = append(items, filesRedactParent(f, item))
		ordinals = append(ordinals, ordinal)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(items) == 0 || (len(items) <= limit && ordinals[len(ordinals)-1] != snapshot.TotalEntries-1) {
		return fc.NewProblem(503, "snapshot_incomplete", "The saved snapshot is incomplete. Create a new baseline.")
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		encoded, e := s.filesEncodeSnapshotCursor(f, filesSnapshotPageCursor{SnapshotID: id, After: ordinals[limit-1], Expires: snapshot.ExpiresAt.Unix()})
		if e != nil {
			return e
		}
		next = &encoded
	}
	if err = s.filesRecheckSnapshotCredential(r.Context(), f); err != nil {
		return err
	}
	if err = tx.Commit(r.Context()); err != nil {
		return err
	}
	return writeFilesJSON(w, http.StatusOK, struct {
		filesChangeSnapshot
		Items      []fc.Metadata `json:"items"`
		NextCursor *string       `json:"nextCursor"`
	}{snapshot, items, next})
}
func (s *Server) handleFilesDeleteChangeSnapshot(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if err := f.requireChanges(false); err != nil {
		return err
	}
	if err := s.filesChangesDB(); err != nil {
		return err
	}
	id := r.PathValue("id")
	if !fc.ValidEntryID(id) {
		return filesSnapshotNotFound()
	}
	if r.URL.RawQuery != "" {
		return fc.NewProblem(400, "invalid_request", "Snapshot deletion does not accept query parameters.")
	}
	if err := s.filesRecheckSnapshotCredential(r.Context(), f); err != nil {
		return err
	}
	// Deletion also works after expiry so clients can explicitly release storage.
	result, err := s.db.Pool().Exec(r.Context(), `delete from file_change_snapshots where id=$1::uuid and owner_id=$2::uuid and principal_id=$3 and root_namespace=$4 and policy_fingerprint=$5`, id, f.Principal.User.ID, filePrincipalID(f.Principal), f.ContentNamespace, hex.EncodeToString(filesChangePolicy(f)))
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return filesSnapshotNotFound()
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
