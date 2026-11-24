package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"everythingabout.me/backend/internal/connections"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/snapshot"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/storageproduct"
	"github.com/jackc/pgx/v5"
)

// The storage settings surface. It is owner-only on purpose: a credential-bearing
// endpoint that an API token could reach would let a read-only token move a user's
// tree somewhere else. Every handler here takes the session cookie and asserts the
// same origin, exactly like the other account mutations.

// defaultBackupKeep is the number of dated snapshots a rotation keeps when no policy row
// has chosen one. It repeats sql/037's column default because a prune asked for before any
// retention was configured must still keep something: reading "no policy row yet" as "keep
// everything" would make the same command mean opposite things.
const defaultBackupKeep = 14

// backupRunHistoryLimit bounds the restore points one provider's page lists. A run is
// one row per date and the default retention is 14 dates, so this is years of daily
// snapshots — a page that needs more has a retention question, not a paging one.
const backupRunHistoryLimit = 120

type storageConnectionView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
	// Product is the store's name (aws-s3, cos, minio, rustfs, local). It is what the page
	// shows, because it is what decides the guarantees: "S3-compatible" was the label that
	// made a person ask questions the row could not answer.
	Product  string `json:"product"`
	Backend  string `json:"backend"`
	Location string `json:"location"`
	Enabled  bool   `json:"enabled"`
	Mine     bool   `json:"mine"`
	// BackupFolder is where dated snapshots are placed inside this store. It is the
	// backup feature's placement recorded against this row, not a property of the
	// provider: the folder is a backup setting, which is what lets a second feature
	// reuse the same provider with its own placement.
	BackupFolder string `json:"backupFolder"`
	// Runs counts the account's backup runs on this connection. A nonzero count is
	// why the folder can no longer change, so the page has to know it.
	Runs int `json:"runs"`
	// LastRun is the newest of those runs: what a provider's card can say about
	// history without carrying every date of every provider to the list page.
	LastRun *backupRunView `json:"lastRun,omitempty"`
}

func connectionView(c connections.Connection, runCount int, last *backupRunView) storageConnectionView {
	return storageConnectionView{
		ID: c.ID, Name: c.Name, Role: c.Role, Product: connectionProduct(c), Backend: c.Backend,
		Location: connectionLocation(c), Enabled: c.Enabled, Mine: c.OwnerID != "",
		BackupFolder: c.Config.BackupFolder, Runs: runCount, LastRun: last,
	}
}

// connectionProduct names the product a row uses. Rows written before the product split
// carry only a backend, and COS and Local have exactly one product each; an S3 row from
// that era is reported as the family it named rather than silently guessed as one product.
func connectionProduct(c connections.Connection) string {
	if c.Config.Product != "" {
		return c.Config.Product
	}
	if id, ok := storageproduct.ProductForBackend(c.Backend); ok {
		return id
	}
	return c.Backend
}

// backupTargetView names a provider rather than just identifying it: a provider's own
// page has to say which other provider choosing this one would turn off, and an id
// would leave the person to go and look it up.
type backupTargetView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type backupRunView struct {
	ConnectionID   string `json:"connectionId"`
	ConnectionName string `json:"connectionName"`
	Snapshot       string `json:"snapshot"`
	State          string `json:"state"`
	Files          int    `json:"files"`
	Folders        int    `json:"folders"`
	Bytes          int64  `json:"bytes"`
	Error          string `json:"error,omitempty"`
	StartedAt      string `json:"startedAt"`
	Finished       string `json:"finishedAt,omitempty"`
}

// handleStorageGet lists the providers this account may use: the row, how many runs the
// backup feature has recorded against it, and its newest one. The dates themselves are
// not here — they belong to the provider that holds them, which is what that provider's
// own page answers (handleStorageConnectionGet).
func (s *Server) handleStorageGet(w http.ResponseWriter, r *http.Request) error {
	if s.db == nil || s.conns == nil {
		return httpx.ServiceUnavailable("Storage settings are unavailable on this instance.")
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	rows, err := s.conns.Visible(ctx, user.ID)
	if err != nil {
		return err
	}
	runCounts, err := s.backupRunCounts(ctx, user.ID)
	if err != nil {
		return err
	}
	lastRuns, err := s.lastBackupRuns(ctx, user.ID)
	if err != nil {
		return err
	}
	views := make([]storageConnectionView, 0, len(rows))
	for _, c := range rows {
		views = append(views, connectionView(c, runCounts[c.ID], lastRuns[c.ID]))
	}
	target, err := s.backupTarget(ctx, user.ID)
	if err != nil {
		return err
	}
	return s.ok(w, map[string]any{"connections": views, "backupTarget": target})
}

// backupRunColumns is backupRunView's select list, shared by the readers below so the
// scan order cannot drift away from a query.
const backupRunColumns = `r.connection_id::text, c.name, to_char(r.snapshot,'YYYY-MM-DD'), r.state, r.files, r.folders, r.bytes, coalesce(r.error,''),
	       to_char(r.started_at,'YYYY-MM-DD"T"HH24:MI:SSOF'), coalesce(to_char(r.finished_at,'YYYY-MM-DD"T"HH24:MI:SSOF'),'')`

type rowScanner interface{ Scan(dest ...any) error }

func scanBackupRun(row rowScanner) (backupRunView, error) {
	var run backupRunView
	err := row.Scan(&run.ConnectionID, &run.ConnectionName, &run.Snapshot, &run.State,
		&run.Files, &run.Folders, &run.Bytes, &run.Error, &run.StartedAt, &run.Finished)
	return run, err
}

// backupRuns reads this account's runs, newest first. An empty connectionID means every
// provider. The filter travels as a query parameter rather than as a second statement so
// both readers keep one column list and one ordering.
func (s *Server) backupRuns(ctx context.Context, ownerID, connectionID string, limit int) ([]backupRunView, error) {
	var filter *string
	if connectionID != "" {
		filter = &connectionID
	}
	rows, err := s.db.Pool().Query(ctx, `
		select `+backupRunColumns+`
		from backup_runs r join storage_connections c on c.id = r.connection_id
		where r.owner_id=$1::uuid and ($2::uuid is null or r.connection_id=$2::uuid)
		order by r.started_at desc limit $3`, ownerID, filter, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := make([]backupRunView, 0, 8)
	for rows.Next() {
		run, err := scanBackupRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// lastBackupRuns is the newest run per connection. A provider's card needs one date;
// reading a bounded tail of everyone's history and keeping the first per provider would
// hide a provider whose last backup is older than the newest runs of another.
func (s *Server) lastBackupRuns(ctx context.Context, ownerID string) (map[string]*backupRunView, error) {
	rows, err := s.db.Pool().Query(ctx, `
		select distinct on (r.connection_id) `+backupRunColumns+`
		from backup_runs r join storage_connections c on c.id = r.connection_id
		where r.owner_id=$1::uuid
		order by r.connection_id, r.started_at desc`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	last := make(map[string]*backupRunView)
	for rows.Next() {
		run, err := scanBackupRun(rows)
		if err != nil {
			return nil, err
		}
		last[run.ConnectionID] = &run
	}
	return last, rows.Err()
}

// backupRunCounts is how many runs each connection holds. The count, not the history, is
// what locks a destination's backup folder: a restore point names a date and a folder,
// so moving the folder makes every listed date unreachable.
func (s *Server) backupRunCounts(ctx context.Context, ownerID string) (map[string]int, error) {
	rows, err := s.db.Pool().Query(ctx, `select connection_id::text, count(*) from backup_runs where owner_id=$1::uuid group by connection_id`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		counts[id] = count
	}
	return counts, rows.Err()
}

// handleStorageConnectionGet is one provider's page: the row, the backup feature's
// placement on it, and the restore points that live in it.
//
// It is not folded into GET /api/storage because a provider page owns its own history.
// One shared response would either ship every date of every provider to a page that
// shows one, or cap one provider's history at the newest runs of the others.
func (s *Server) handleStorageConnectionGet(w http.ResponseWriter, r *http.Request) error {
	if s.db == nil || s.conns == nil {
		return httpx.ServiceUnavailable("Storage settings are unavailable on this instance.")
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	target, ok, err := s.visibleConnection(r, user.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	if !ok {
		return httpx.NotFound("No such storage connection on this account.")
	}
	counts, err := s.backupRunCounts(ctx, user.ID)
	if err != nil {
		return err
	}
	runs, err := s.backupRuns(ctx, user.ID, target.ID, backupRunHistoryLimit)
	if err != nil {
		return err
	}
	var last *backupRunView
	if len(runs) > 0 {
		last = &runs[0]
	}
	chosen, err := s.backupTargetRef(ctx, user.ID)
	if err != nil {
		return err
	}
	view := connectionView(target, counts[target.ID], last)
	return s.ok(w, map[string]any{
		"connection":   view,
		"backupTarget": chosen,
		"runs":         runs,
	})
}

// backupTargetRef is the provider the backup feature writes to, or nil when this account
// has not configured it. It carries the name because a provider's own page has to say
// which provider choosing this one would turn off: with one page per provider, "at most
// one destination" is otherwise invisible from where the choice is made.
func (s *Server) backupTargetRef(ctx context.Context, ownerID string) (*backupTargetView, error) {
	var ref backupTargetView
	err := s.db.Pool().QueryRow(ctx, `
		select c.id::text, c.name from backup_policies p
		join storage_connections c on c.id = p.connection_id
		where p.owner_id=$1::uuid and p.enabled and c.role='backup'
		order by p.updated_at desc limit 1`, ownerID).Scan(&ref.ID, &ref.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

// backupTarget is the provider the backup feature writes to, or "" when this
// account has not configured it. There is deliberately no fallback to "the only
// attached provider": adding a provider is setup, and a feature that switched
// itself on because it happened to be the only candidate would make the second
// step invisible — the same mistake as silently choosing the first row.
func (s *Server) backupTarget(ctx context.Context, ownerID string) (string, error) {
	ref, err := s.backupTargetRef(ctx, ownerID)
	if err != nil || ref == nil {
		return "", err
	}
	return ref.ID, nil
}

type connectionInput struct {
	Name string `json:"name"`
	// Product names the store (aws-s3, cos, minio, rustfs). It replaces the old
	// "backend" of cos|s3 because a protocol family does not answer the questions a
	// connection has: how bytes travel, and whether the store replaces an object it was
	// told not to touch.
	Product  string `json:"product"`
	Bucket   string `json:"bucket"`
	Region   string `json:"region"`
	Endpoint string `json:"endpoint"`
	// PathStyle is nil when the account left the product's default alone.
	PathStyle *bool `json:"pathStyle"`
	// Prefix is the tree's prefix inside an S3 bucket: nil keeps the deployment default,
	// an explicit empty string means the bucket itself.
	Prefix    *string `json:"prefix"`
	SecretID  string  `json:"secretId"`
	SecretKey string  `json:"secretKey"`
}

// handleStorageConnectionAdd lets an account attach its own drive as a backup
// destination, naming the store's product: a COS bucket or an endpoint running AWS S3,
// MinIO or RustFS. A local root stays operator-only — it is a host path, not a drive an
// account can name.
func (s *Server) handleStorageConnectionAdd(w http.ResponseWriter, r *http.Request) error {
	if s.db == nil || s.conns == nil {
		return httpx.ServiceUnavailable("Storage settings are unavailable on this instance.")
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	var in connectionInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		return httpx.BadRequest("The request body is not valid JSON.")
	}
	// A destination is only useful if this process can publish into it, and publication
	// needs the coordinated object protocol — which a local-root deployment does not run.
	if !s.cfg.Storage.CoordinatedWrites {
		return httpx.BadRequest("This deployment runs on a local root, so there is no object store for a backup destination to publish into.")
	}
	product, err := storageproduct.Lookup(strings.ToLower(strings.TrimSpace(in.Product)))
	if err != nil {
		return httpx.BadRequest(err.Error())
	}
	if product.Backend == "local" {
		return httpx.BadRequest("A local root is a host path, not a drive an account can attach; only an operator can configure it.")
	}
	// The row id is generated before the INSERT because this connection's spool is keyed
	// by it; the name is what the account reads, not what separates two accounts'
	// directories.
	id, err := connections.NewID()
	if err != nil {
		return err
	}
	c := connections.Connection{
		ID: id, OwnerID: user.ID, Name: in.Name, Backend: product.Backend, Role: connections.RoleBackup,
		Generation: 1, Enabled: true,
		Config: connections.Spec{Product: product.ID},
	}
	switch product.Backend {
	case "cos":
		c.Config.COS = connections.COSSpec{Bucket: in.Bucket, Region: in.Region}
		c.Secret = connections.Secret{COS: connections.COSCredential{SecretID: in.SecretID, SecretKey: in.SecretKey}}
	case "s3":
		prefix, rootAtBucket, err := connections.S3ContentPrefix(in.Prefix)
		if err != nil {
			return httpx.BadRequest(err.Error())
		}
		pathStyle := product.PathStyle
		if in.PathStyle != nil {
			pathStyle = *in.PathStyle
		}
		c.Config.S3 = connections.S3Spec{Endpoint: in.Endpoint, Region: in.Region, Bucket: in.Bucket,
			PathStyle: pathStyle, ContentPrefix: prefix, SystemPrefix: "__eam_system__/"}
		c.Config.RootAtBucket = rootAtBucket
		c.Secret = connections.Secret{S3: connections.S3Credential{AccessKeyID: in.SecretID, SecretAccessKey: in.SecretKey}}
	}
	if err := connections.ValidateName(c.Name); err != nil {
		return httpx.BadRequest(err.Error())
	}
	// Each connection owns its spool: inheriting the deployment's would let one target's
	// temporary bytes be published to another.
	spoolRoot, err := connections.ObjectPaths(s.cfg.Storage, c, "")
	if err != nil {
		return httpx.BadRequest(err.Error())
	}
	c.Config.ObjectSpoolRoot = spoolRoot
	if err := os.MkdirAll(spoolRoot, 0o700); err != nil {
		return fmt.Errorf("create spool root for storage connection %q: %w", c.Name, err)
	}
	identity, err := c.ComputeIdentity()
	if err != nil {
		return err
	}
	c.Identity = identity
	if err := c.ValidateAccountAdded(); err != nil {
		return httpx.BadRequest(err.Error())
	}
	if err := s.conns.Insert(r.Context(), c); err != nil {
		if errors.Is(err, connections.ErrLocationTaken) {
			return httpx.BadRequest(err.Error())
		}
		return err
	}
	return s.ok(w, map[string]any{"name": c.Name})
}

// visibleConnection resolves one row the account may act on, or reports that it is
// not on this page. Every settings handler starts here so a row that belongs to
// another account is indistinguishable from a row that does not exist.
func (s *Server) visibleConnection(r *http.Request, userID, id string) (connections.Connection, bool, error) {
	rows, err := s.conns.Visible(r.Context(), userID)
	if err != nil {
		return connections.Connection{}, false, err
	}
	for _, c := range rows {
		if c.ID == id {
			return c, true, nil
		}
	}
	return connections.Connection{}, false, nil
}

// connectionSettingsInput carries the backup settings an account may change on a
// destination it added. Folder is a relative path inside that store, not a new
// location: changing it does not move the connection, only where dates are placed.
type connectionSettingsInput struct {
	BackupFolder *string `json:"backupFolder"`
}

// handleStorageConnectionSettings changes where a destination keeps its snapshots.
// The deployment's own drive is refused: its location is an operator decision, and
// an account editing it would be editing the tree its files live on.
func (s *Server) handleStorageConnectionSettings(w http.ResponseWriter, r *http.Request) error {
	if s.db == nil || s.conns == nil {
		return httpx.ServiceUnavailable("Storage settings are unavailable on this instance.")
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	var in connectionSettingsInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		return httpx.BadRequest("The request body is not valid JSON.")
	}
	if in.BackupFolder == nil {
		return httpx.BadRequest(`The request must carry "backupFolder" (send "" for the store's root).`)
	}
	target, ok, err := s.visibleConnection(r, user.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	if !ok {
		return httpx.NotFound("No such storage connection on this account.")
	}
	if target.OwnerID != user.ID {
		return httpx.Forbidden("The default drive is managed by the deployment; only a destination this account added can be configured.")
	}
	if target.Role != connections.RoleBackup {
		return httpx.Forbidden("Only a backup destination can be configured here.")
	}
	folder, err := connections.NormalizeBackupFolder(*in.BackupFolder)
	if err != nil {
		return httpx.BadRequest(err.Error())
	}
	if folder == target.Config.BackupFolder {
		return s.ok(w, map[string]any{"backupFolder": folder})
	}
	if err := s.conns.SetBackupFolder(r.Context(), target.ID, user.ID, folder); err != nil {
		switch {
		case errors.Is(err, connections.ErrBackupFolderLocked):
			return httpx.Conflict("This destination already holds backups, so its folder cannot change: those restore points live under the old folder. Remove them first, or keep the folder.")
		case errors.Is(err, connections.ErrConnectionNotFound):
			return httpx.NotFound("No such storage connection on this account.")
		default:
			return err
		}
	}
	return s.ok(w, map[string]any{"backupFolder": folder})
}

// handleStorageBackupTarget chooses the destination a backup uses. The policy row
// is the account's decision and survives a page reload, which the previous "first
// backup connection wins" rule did not: a second destination was silently ignored.
func (s *Server) handleStorageBackupTarget(w http.ResponseWriter, r *http.Request) error {
	if s.db == nil || s.conns == nil {
		return httpx.ServiceUnavailable("Storage settings are unavailable on this instance.")
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	var in struct {
		ConnectionID string `json:"connectionId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		return httpx.BadRequest("The request body is not valid JSON.")
	}
	ctx := r.Context()
	if in.ConnectionID == "" {
		// The provider rows stay: "not configured" is a feature setting, not a reason
		// to forget the provider. The policy row also carries retention later, so it
		// is disabled rather than deleted.
		if _, err := s.db.Pool().Exec(ctx, `update backup_policies set enabled=false, updated_at=now() where owner_id=$1::uuid`, user.ID); err != nil {
			return err
		}
		return s.ok(w, map[string]any{"backupTarget": ""})
	}
	target, ok, err := s.visibleConnection(r, user.ID, in.ConnectionID)
	if err != nil {
		return err
	}
	if !ok {
		return httpx.NotFound("No such storage connection on this account.")
	}
	if target.Role != connections.RoleBackup {
		return httpx.BadRequest("Only a backup destination can be chosen as the target; the drive your files live on is not one.")
	}
	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `update backup_policies set enabled=false, updated_at=now() where owner_id=$1::uuid and connection_id<>$2::uuid`, user.ID, target.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		insert into backup_policies(owner_id, connection_id, enabled) values($1::uuid,$2::uuid,true)
		on conflict (owner_id, connection_id) do update set enabled=true, updated_at=now()`, user.ID, target.ID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return s.ok(w, map[string]any{"backupTarget": target.ID})
}

func (s *Server) handleStorageConnectionRemove(w http.ResponseWriter, r *http.Request) error {
	if s.db == nil || s.conns == nil {
		return httpx.ServiceUnavailable("Storage settings are unavailable on this instance.")
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	id := r.PathValue("id")
	res, err := s.db.Pool().Exec(r.Context(), `delete from storage_connections where id=$1::uuid and owner_id=$2::uuid`, id, user.ID)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return httpx.NotFound("No such connection on this account.")
	}
	return s.ok(w, map[string]any{"removed": id})
}

type backupRequest struct {
	ConnectionID string `json:"connectionId"`
	Action       string `json:"action"`
	Date         string `json:"date"`
	Keep         int    `json:"keep"`
}

// handleBackupStart queues one backup action. The work runs off the request: a
// snapshot is bounded by the tree it copies, not by a request timeout, and the page
// polls backup_runs for progress.
func (s *Server) handleBackupStart(w http.ResponseWriter, r *http.Request) error {
	if s.db == nil || s.conns == nil {
		return httpx.ServiceUnavailable("Storage settings are unavailable on this instance.")
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	var in backupRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		return httpx.BadRequest("The request body is not valid JSON.")
	}
	rows, err := s.conns.Visible(r.Context(), user.ID)
	if err != nil {
		return err
	}
	var target *connections.Connection
	for i := range rows {
		if rows[i].ID == in.ConnectionID {
			target = &rows[i]
		}
	}
	if target == nil {
		return httpx.NotFound("No such storage connection on this account.")
	}
	// The authority connection is where the owner's files live; writing a dated copy
	// into it would put a second tree inside the one being copied, and a restore
	// would write back into the live tree's parent. Only a destination exists to
	// receive snapshots.
	if target.Role != connections.RoleBackup {
		return httpx.BadRequest("Only a backup destination can run a snapshot, restore or prune; the connection your files live on cannot.")
	}
	switch in.Action {
	case "snapshot", "prune", "restore":
	default:
		return httpx.BadRequest("action must be snapshot, prune or restore.")
	}
	date := in.Date
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return httpx.BadRequest("date must be YYYY-MM-DD.")
	}
	key := user.ID + ":" + target.ID
	if s.backupBusy(key) {
		return httpx.Conflict("A backup is already running for this destination.")
	}
	// Record the attempt before answering 202. The copy itself runs off the request,
	// so a shutdown or crash would otherwise destroy the only evidence that a backup
	// was ever asked for. The completion upsert updates this same row through the
	// (owner, connection, snapshot) unique index, and the settings page already knows
	// how to render 'running'.
	folder, err := s.recordBackupStart(r.Context(), user.ID, target.ID, date)
	if err != nil {
		s.releaseBackup(key)
		return err
	}
	// The row was read before the run row existed. Locking it while recording the
	// attempt re-reads the folder under the same lock a folder change takes, so the
	// copy writes where this run's history row says it did.
	target.Config.BackupFolder = folder
	go s.runBackupJob(key, user.ID, *target, in.Action, date, in.Keep)
	httpx.WriteOKStatus(w, http.StatusAccepted, map[string]any{"started": true})
	return nil
}

// recordBackupStart writes the row a run owns before any bytes move. Without it a
// run interrupted by a deploy left no trace at all, and the history could not tell
// "never started" from "started and died"; a re-run resets the row because the
// unique index keys a day's attempt, not an append-only log.
func (s *Server) recordBackupStart(ctx context.Context, ownerID, connectionID, date string) (string, error) {
	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var folder *string
	if err := tx.QueryRow(ctx, `select config->>'backupFolder' from storage_connections where id=$1::uuid for update`, connectionID).Scan(&folder); err != nil {
		return "", fmt.Errorf("read the backup folder of connection %s before starting a run: %w", connectionID, err)
	}
	if _, err := tx.Exec(ctx, `
		insert into backup_runs(owner_id, connection_id, snapshot, state)
		values($1::uuid,$2::uuid,$3::date,'running')
		on conflict (owner_id, connection_id, snapshot) do update
		set state='running', error=null, files=0, folders=0, bytes=0, started_at=now(), finished_at=null`,
		ownerID, connectionID, date); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	if folder == nil {
		return "", nil
	}
	return *folder, nil
}

// MarkInterruptedBackups records that runs still at 'running' did not finish.
// A backup started from the web runs on its own context and is deliberately not
// waited for — copying a whole tree can legitimately outlive any drain budget — so
// the row written at start would otherwise claim forever that a backup is in
// progress. Call it while the pool is still open, after the HTTP drain.
//
// It is an UPDATE, not a second drain, and it is safe against a run finishing
// concurrently: that run's completion upsert either lands first (the row is then
// complete and this statement skips it) or last (the real result replaces the
// failure). A deployment runs one instance, so a 'running' row at this point
// belongs to this process.
func (s *Server) MarkInterruptedBackups(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	tag, err := s.db.Pool().Exec(ctx, `update backup_runs set state='failed', error=$1, finished_at=now() where state='running'`, "the server stopped before this backup finished")
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		slog.Warn("marked interrupted backups as failed", "runs", tag.RowsAffected())
	}
	return nil
}

func (s *Server) releaseBackup(key string) {
	s.backupMu.Lock()
	delete(s.backupJobs, key)
	s.backupMu.Unlock()
}

func (s *Server) backupBusy(key string) bool {
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	if s.backupJobs == nil {
		s.backupJobs = map[string]bool{}
	}
	if s.backupJobs[key] {
		return true
	}
	s.backupJobs[key] = true
	return false
}

func (s *Server) runBackupJob(key, ownerID string, target connections.Connection, action, date string, keep int) {
	defer s.releaseBackup(key)
	ctx := context.Background()
	fail := func(err error) {
		s.recordBackupRun(ctx, ownerID, target.ID, date, snapshot.Result{}, err)
	}
	if s.trees == nil {
		fail(fmt.Errorf("file storage is not configured"))
		return
	}
	named, ok := s.trees.(interface {
		OpenConnection(context.Context, string, string) (storage.Driver, error)
	})
	if !ok {
		fail(fmt.Errorf("this deployment cannot open a named storage connection"))
		return
	}
	dst, err := named.OpenConnection(ctx, ownerID, target.Name)
	if err != nil {
		fail(err)
		return
	}
	defer dst.Close()
	switch action {
	case "snapshot":
		src, err := s.trees.Open(ctx, ownerID)
		if err != nil {
			fail(err)
			return
		}
		defer src.Close()
		writer, ok := dst.(snapshot.Target)
		if !ok {
			fail(fmt.Errorf("storage connection %q cannot accept a snapshot", target.Name))
			return
		}
		result, err := snapshot.Copy(ctx, src, "", writer, target.BackupRoot(date))
		s.recordBackupRun(ctx, ownerID, target.ID, date, result, err)
	case "prune":
		remover, ok := dst.(storage.Remover)
		if !ok {
			fail(fmt.Errorf("storage connection %q cannot prune", target.Name))
			return
		}
		if keep <= 0 {
			keep = defaultBackupKeep
		}
		if _, err := snapshot.Prune(ctx, dst, remover, target.BackupFolderPath(), keep); err != nil {
			fail(err)
			return
		}
		s.recordBackupRun(ctx, ownerID, target.ID, date, snapshot.Result{}, nil)
	case "restore":
		src, err := named.OpenConnection(ctx, ownerID, target.Name)
		if err != nil {
			fail(err)
			return
		}
		defer src.Close()
		live, err := s.trees.Open(ctx, ownerID)
		if err != nil {
			fail(err)
			return
		}
		defer live.Close()
		writer, ok := live.(snapshot.Target)
		if !ok {
			fail(fmt.Errorf("the owner's tree cannot be restored into"))
			return
		}
		result, err := snapshot.Copy(ctx, src, target.BackupRoot(date), writer, storage.Path("restore-"+date))
		s.recordBackupRun(ctx, ownerID, target.ID, date, result, err)
	}
}

func (s *Server) recordBackupRun(ctx context.Context, ownerID, connectionID, date string, result snapshot.Result, runErr error) {
	state, detail := "complete", any(nil)
	if runErr != nil {
		state, detail = "failed", runErr.Error()
	}
	_, err := s.db.Pool().Exec(ctx, `
		insert into backup_runs(owner_id, connection_id, snapshot, state, files, folders, bytes, error, finished_at)
		values($1::uuid,$2::uuid,$3::date,$4,$5,$6,$7,$8,now())
		on conflict (owner_id, connection_id, snapshot) do update
		set state=$4, files=$5, folders=$6, bytes=$7, error=$8, finished_at=now()`,
		ownerID, connectionID, date, state, result.Files, result.Folders, result.Bytes, detail)
	if err != nil {
		slog.Error("record backup run", "owner", ownerID, "connection", connectionID, "error", err)
	}
}

// connectionLocation is what the page may say about where a store is. A shared row's
// store belongs to the deployment, not to the account: its bucket name carries the
// operator's provider account id and its region names infrastructure the account can
// neither choose nor check, so it answers "". The account acts on that row all the same
// (it holds the files), and the role says what it is. Redacted here rather than in the
// page because a value that reaches the browser is a value the user can read; operator
// tooling still prints it (`eam storage-connection list`).
func connectionLocation(c connections.Connection) string {
	if c.OwnerID == "" {
		return ""
	}
	switch c.Backend {
	case "cos":
		return c.Config.COS.Bucket + " · " + c.Config.COS.Region
	case "s3":
		// The region of an S3-compatible server is usually a formality; the host is what
		// a person recognises.
		if host, err := connections.S3EndpointHost(c.Config.S3.Endpoint); err == nil {
			return host + " · " + c.Config.S3.Bucket
		}
		return c.Config.S3.Endpoint + " · " + c.Config.S3.Bucket
	case "local":
		return c.Config.Local.ContentRoot
	}
	return c.Backend
}
