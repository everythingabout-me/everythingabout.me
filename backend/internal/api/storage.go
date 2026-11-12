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
	"everythingabout.me/backend/internal/storagebackend"
)

// The storage settings surface. It is owner-only on purpose: a credential-bearing
// endpoint that an API token could reach would let a read-only token move a user's
// tree somewhere else. Every handler here takes the session cookie and asserts the
// same origin, exactly like the other account mutations.

type storageConnectionView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Backend  string `json:"backend"`
	Location string `json:"location"`
	Enabled  bool   `json:"enabled"`
	Mine     bool   `json:"mine"`
}

type backupRunView struct {
	Snapshot  string `json:"snapshot"`
	State     string `json:"state"`
	Files     int    `json:"files"`
	Folders   int    `json:"folders"`
	Bytes     int64  `json:"bytes"`
	Error     string `json:"error,omitempty"`
	StartedAt string `json:"startedAt"`
	Finished  string `json:"finishedAt,omitempty"`
}

// handleStorageGet is the whole settings page in one response: which connections
// this account may use, what the backup policy is, and what the last runs did.
func (s *Server) handleStorageGet(w http.ResponseWriter, r *http.Request) error {
	if s.db == nil || s.conns == nil {
		return httpx.ServiceUnavailable("Storage settings are unavailable on this instance.")
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	rows, err := s.conns.Visible(r.Context(), user.ID)
	if err != nil {
		return err
	}
	views := make([]storageConnectionView, 0, len(rows))
	for _, c := range rows {
		views = append(views, storageConnectionView{
			ID: c.ID, Name: c.Name, Role: c.Role, Backend: c.Backend,
			Location: connectionLocation(c), Enabled: c.Enabled, Mine: c.OwnerID != "",
		})
	}
	keep := 14
	_ = s.db.Pool().QueryRow(r.Context(), `select keep from backup_policies where owner_id=$1::uuid limit 1`, user.ID).Scan(&keep)

	runs := make([]backupRunView, 0, 20)
	rows2, err := s.db.Pool().Query(r.Context(), `
		select to_char(snapshot,'YYYY-MM-DD'), state, files, folders, bytes, coalesce(error,''),
		       to_char(started_at,'YYYY-MM-DD"T"HH24:MI:SSOF'), coalesce(to_char(finished_at,'YYYY-MM-DD"T"HH24:MI:SSOF'),'')
		from backup_runs where owner_id=$1::uuid order by started_at desc limit 20`, user.ID)
	if err != nil {
		return err
	}
	defer rows2.Close()
	for rows2.Next() {
		var run backupRunView
		if err := rows2.Scan(&run.Snapshot, &run.State, &run.Files, &run.Folders, &run.Bytes, &run.Error, &run.StartedAt, &run.Finished); err != nil {
			return err
		}
		runs = append(runs, run)
	}
	if err := rows2.Err(); err != nil {
		return err
	}
	return s.ok(w, map[string]any{"connections": views, "keep": keep, "runs": runs})
}

type connectionInput struct {
	Name      string `json:"name"`
	Backend   string `json:"backend"`
	Bucket    string `json:"bucket"`
	Region    string `json:"region"`
	Endpoint  string `json:"endpoint"`
	PathStyle bool   `json:"pathStyle"`
	// Prefix is the tree's prefix inside an S3 bucket: nil keeps the deployment default,
	// an explicit empty string means the bucket itself.
	Prefix    *string `json:"prefix"`
	SecretID  string  `json:"secretId"`
	SecretKey string  `json:"secretKey"`
}

// handleStorageConnectionAdd lets an account attach its own drive as a backup
// destination: a COS bucket, or an S3-compatible endpoint whose host the operator has
// listed. A local root stays operator-only — it is a host path, not a drive an account
// can name.
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
	// needs the coordinated object protocol.
	if !s.cfg.Storage.CoordinatedWrites {
		return httpx.BadRequest("Object writes are disabled on this deployment, so a backup destination could not publish into it.")
	}
	backend := strings.ToLower(strings.TrimSpace(in.Backend))
	if backend == "" {
		backend = "cos"
	}
	c := connections.Connection{
		OwnerID: user.ID, Name: in.Name, Backend: backend, Role: connections.RoleBackup,
		Generation: 1, Enabled: true,
		Config: connections.Spec{CoordinatedWrites: true},
	}
	switch backend {
	case "cos":
		c.Config.COS = connections.COSSpec{Bucket: in.Bucket, Region: in.Region}
		c.Config.TransferClass = s.cfg.Storage.TransferClass
		c.Secret = connections.Secret{COS: connections.COSCredential{SecretID: in.SecretID, SecretKey: in.SecretKey}}
	case "s3":
		prefix, rootAtBucket, err := connections.S3ContentPrefix(in.Prefix)
		if err != nil {
			return httpx.BadRequest(err.Error())
		}
		c.Config.S3 = connections.S3Spec{Endpoint: in.Endpoint, Region: in.Region, Bucket: in.Bucket,
			PathStyle: in.PathStyle, ContentPrefix: prefix, SystemPrefix: "__eam_system__/"}
		c.Config.RootAtBucket = rootAtBucket
		// An S3-compatible server speaks the relay class only; a presigned part URL would
		// have to be signed for its host, which this connection's credentials may not be
		// allowed to do.
		c.Config.TransferClass = storage.TransferRelay
		c.Secret = connections.Secret{S3: connections.S3Credential{AccessKeyID: in.SecretID, SecretAccessKey: in.SecretKey}}
	default:
		return httpx.BadRequest("A drive is either a COS bucket or an S3-compatible endpoint.")
	}
	if err := connections.ValidateName(c.Name); err != nil {
		return httpx.BadRequest(err.Error())
	}
	// Each connection owns its evidence and its spool. Inheriting the deployment's would
	// make this bucket refuse evidence recorded for another one, and measuring this bucket
	// would overwrite the authority's proof at that path.
	admissionFile, spoolRoot, err := connections.ObjectPaths(s.cfg.Storage, c.Name, c.Backend, "", "")
	if err != nil {
		return httpx.BadRequest(err.Error())
	}
	c.Config.ObjectAdmissionFile, c.Config.ObjectSpoolRoot = admissionFile, spoolRoot
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
	if err := s.recordBackupStart(r.Context(), user.ID, target.ID, date); err != nil {
		s.releaseBackup(key)
		return err
	}
	go s.runBackupJob(key, user.ID, *target, in.Action, date, in.Keep)
	httpx.WriteOKStatus(w, http.StatusAccepted, map[string]any{"started": true})
	return nil
}

// recordBackupStart writes the row a run owns before any bytes move. Without it a
// run interrupted by a deploy left no trace at all, and the history could not tell
// "never started" from "started and died"; a re-run resets the row because the
// unique index keys a day's attempt, not an append-only log.
func (s *Server) recordBackupStart(ctx context.Context, ownerID, connectionID, date string) error {
	_, err := s.db.Pool().Exec(ctx, `
		insert into backup_runs(owner_id, connection_id, snapshot, state)
		values($1::uuid,$2::uuid,$3::date,'running')
		on conflict (owner_id, connection_id, snapshot) do update
		set state='running', error=null, files=0, folders=0, bytes=0, started_at=now(), finished_at=null`,
		ownerID, connectionID, date)
	return err
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
	if action == "snapshot" {
		// Measure the destination at the moment it is used, not when it is recorded. A
		// drive can be added while it is unreachable — a NAS that is off, a tunnel that is
		// down — and must work as soon as it is back, without re-adding it. Publication is
		// the only thing that needs this proof; a restore or a prune reads and deletes.
		effective, err := target.ApplyEnvironment(*s.cfg)
		if err != nil {
			fail(err)
			return
		}
		if err := storagebackend.EnsureAdmission(ctx, &effective); err != nil {
			fail(err)
			return
		}
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
		result, err := snapshot.Copy(ctx, src, "", writer, storage.Path(date))
		s.recordBackupRun(ctx, ownerID, target.ID, date, result, err)
	case "prune":
		remover, ok := dst.(storage.Remover)
		if !ok {
			fail(fmt.Errorf("storage connection %q cannot prune", target.Name))
			return
		}
		if keep <= 0 {
			keep = 14
		}
		if _, err := snapshot.Prune(ctx, dst, remover, "", keep); err != nil {
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
		result, err := snapshot.Copy(ctx, src, storage.Path(date), writer, storage.Path("restore-"+date))
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

func connectionLocation(c connections.Connection) string {
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
