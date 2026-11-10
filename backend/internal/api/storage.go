package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"everythingabout.me/backend/internal/connections"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/snapshot"
	"everythingabout.me/backend/internal/storage"
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
	Bucket    string `json:"bucket"`
	Region    string `json:"region"`
	SecretID  string `json:"secretId"`
	SecretKey string `json:"secretKey"`
}

// handleStorageConnectionAdd lets an account attach its own COS bucket as a backup
// destination. It cannot pick a backend: the row is forced to cos, because a local
// root would be a host path and an arbitrary endpoint would be an outbound request.
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
	c := connections.Connection{
		OwnerID: user.ID, Name: in.Name, Backend: "cos", Role: connections.RoleBackup,
		Generation: 1, Enabled: true,
		Config: connections.Spec{
			COS: connections.COSSpec{Bucket: in.Bucket, Region: in.Region},
			// A destination buckets the operator already admitted are the only ones
			// this process can publish into safely; anything else is unproven.
			CoordinatedWrites:   s.cfg.Storage.CoordinatedWrites,
			ObjectAdmissionFile: s.cfg.Storage.ObjectAdmissionFile,
			ObjectSpoolRoot:     s.cfg.Storage.ObjectSpoolRoot,
			TransferClass:       s.cfg.Storage.TransferClass,
		},
		Secret: connections.Secret{COS: connections.COSCredential{SecretID: in.SecretID, SecretKey: in.SecretKey}},
	}
	identity, err := c.ComputeIdentity()
	if err != nil {
		return err
	}
	c.Identity = identity
	if err := c.Validate(); err != nil {
		return httpx.BadRequest(err.Error())
	}
	if err := s.conns.Insert(r.Context(), c); err != nil {
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
	go s.runBackupJob(key, user.ID, *target, in.Action, date, in.Keep)
	httpx.WriteOKStatus(w, http.StatusAccepted, map[string]any{"started": true})
	return nil
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
	defer func() {
		s.backupMu.Lock()
		delete(s.backupJobs, key)
		s.backupMu.Unlock()
	}()
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
		return c.Config.S3.Bucket + " · " + c.Config.S3.Region
	case "local":
		return c.Config.Local.ContentRoot
	}
	return c.Backend
}
