package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	ID       string `json:"id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
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
	// Probe is the last measurement this account asked for, nil if it never did.
	Probe json.RawMessage `json:"probe,omitempty"`
	// ProbeRunning reports a measurement this process is running right now, so a page
	// reloaded mid-test waits instead of showing the stale result as the answer.
	ProbeRunning bool `json:"probeRunning,omitempty"`
	// ProbeProgress is that run revealed phase by phase, so the page fills each row when
	// its phase finishes instead of leaving the sheet empty until the whole test ends.
	// It is set only on the provider's own page: the list shows a card, not a sheet.
	ProbeProgress *probeProgressView `json:"probeProgress,omitempty"`
}

func connectionView(c connections.Connection, runCount int, last *backupRunView) storageConnectionView {
	return storageConnectionView{
		ID: c.ID, Name: c.Name, Role: c.Role, Backend: c.Backend,
		Location: connectionLocation(c), Enabled: c.Enabled, Mine: c.OwnerID != "",
		BackupFolder: c.Config.BackupFolder, Runs: runCount, LastRun: last, Probe: c.LastProbe,
	}
}

// backupTargetView names a provider rather than just identifying it: a provider's own
// page has to say which other provider choosing this one would turn off, and an id
// would leave the person to go and look it up.
type backupTargetView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// storageTestView is one measurement, returned when it finishes and cached on the row.
//
// Each phase is its own field rather than one blended score: a large upload says nothing
// about whether a backup of 100k small files can finish before morning, and the phases are
// what the page lists side by side. The file-operation phases are pointers because a probe
// cached before they existed has no answer for them — absent is "never measured", which
// the page can show, while a zero would read as "measured zero".
type storageTestView struct {
	TestedAt string `json:"testedAt"`
	// RunID identifies the run that produced this result. A start request answers with the
	// id before any bytes move, so a page can wait for exactly this answer instead of
	// comparing timestamps.
	RunID                  string               `json:"runId,omitempty"`
	OK                     bool                 `json:"ok"`
	Writable               bool                 `json:"writable"`
	Error                  string               `json:"error,omitempty"`
	LatencyMS              int64                `json:"latencyMs"`
	UploadBytes            int64                `json:"uploadBytes"`
	DownloadBytes          int64                `json:"downloadBytes"`
	UploadBytesPerSecond   int64                `json:"uploadBytesPerSecond"`
	DownloadBytesPerSecond int64                `json:"downloadBytesPerSecond"`
	SmallFiles             *fileOpsTestView     `json:"smallFiles,omitempty"`
	Renames                *fileOpsTestView     `json:"renames,omitempty"`
	Deletes                *fileOpsTestView     `json:"deletes,omitempty"`
	Concurrency            *concurrencyTestView `json:"concurrency,omitempty"`
}

// fileOpsTestView is one file-operation phase. The rate is files per second rather than
// bytes per second, because the cost of a small file is its round trip, not its length:
// at 1 KB the two numbers differ only by a constant.
type fileOpsTestView struct {
	Files        int     `json:"files"`
	BytesPerFile int64   `json:"bytesPerFile,omitempty"`
	ElapsedMS    int64   `json:"elapsedMs"`
	PerSecond    float64 `json:"perSecond"`
	Error        string  `json:"error,omitempty"`
}

// concurrencyTestView is the overlapping-write phase. PerStreamBytesPerSecond is the
// average part of the aggregate that each stream contributed, which is the number to
// compare against the single-stream upload rate to see whether concurrency helps.
type concurrencyTestView struct {
	Streams                 int    `json:"streams"`
	BytesPerStream          int64  `json:"bytesPerStream"`
	BytesPerSecond          int64  `json:"bytesPerSecond"`
	PerStreamBytesPerSecond int64  `json:"perStreamBytesPerSecond"`
	Error                   string `json:"error,omitempty"`
}

// probeProgressView is a measurement still running: the cached result's shape, the phase it
// is on, and the phases that already finished. The page prints a row from Done and a spinner
// on Phase, and it needs both as separate facts: "finished" is not the same as "current is
// not this one", because a file-operation phase that failed still finished and still owns
// the error its row prints.
//
// Only a phase listed in Done has a number. A phase that has not finished yet still carries
// zero fields, and zero is not an answer here — a close store can measure a whole millisecond
// of latency, so the absence of a value is expressed by Done, never by the value itself.
type probeProgressView struct {
	storageTestView
	// Phase is the phase measuring right now; "" between phases and while the
	// publishability check runs after them, when there is nothing left to spin.
	Phase string `json:"phase"`
	// Done lists finished phases in completion order.
	Done []string `json:"done"`
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
		view := connectionView(c, runCounts[c.ID], lastRuns[c.ID])
		view.ProbeRunning = s.probeRunning(c.ID)
		views = append(views, view)
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
	view.ProbeRunning = s.probeRunning(target.ID)
	view.ProbeProgress = s.probeProgress(target.ID)
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
	// The row id is generated before the INSERT because this connection's evidence and
	// spool are keyed by it; the name is what the account reads, not what separates two
	// accounts' directories.
	id, err := connections.NewID()
	if err != nil {
		return err
	}
	c := connections.Connection{
		ID: id, OwnerID: user.ID, Name: in.Name, Backend: backend, Role: connections.RoleBackup,
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
	admissionFile, spoolRoot, err := connections.ObjectPaths(s.cfg.Storage, c, "", "")
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

// handleStorageConnectionTest starts a measurement of one destination. It answers
// `202` with the run id and measures off the request.
//
// The work is a 100 MB transfer plus a few hundred file operations, bounded by what it
// does rather than by a clock — which can be minutes on a home NAS, longer than a browser
// request should hold, and a request cancelled by navigation must not leave a
// half-measured provider. The answer is cached on the row, which is what the page polls;
// there is no task resource because the result belongs to the provider, not to the run.
// While it runs, the same poll reads `probeProgress` — the phases that already finished and
// the one now running — because a minute of an unchanged sheet is indistinguishable from a
// hung test.
//
// The measurement runs the phases `storagebackend.Measure` defines — metadata latency, a
// large upload and read-back, a small-file batch written, renamed and deleted, overlapping
// writes — then runs the admission probe the backup path itself depends on. Every answer
// is only worth something if it was produced by the same native path a backup would use.
// The admission half is what turns "the bytes went up" into "a snapshot can be published
// here": conditional create is a separate promise from reachability.
func (s *Server) handleStorageConnectionTest(w http.ResponseWriter, r *http.Request) error {
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
	target, ok, err := s.visibleConnection(r, user.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	if !ok {
		return httpx.NotFound("No such storage connection on this account.")
	}
	runID, err := newProbeRunID()
	if err != nil {
		return err
	}
	result := storageTestView{TestedAt: time.Now().UTC().Format(time.RFC3339), RunID: runID}
	// The run is claimed before it starts, so the id that was just handed out belongs to
	// exactly one measurement and a second click is a conflict rather than a second probe.
	if !s.claimProbe(target.ID, runID, result.TestedAt) {
		return httpx.Conflict("A test is already running for this provider.")
	}
	go s.runStorageTest(target, result)
	// The start time travels back because it is also the finished result's TestedAt: the
	// page can name the moment it asked, not the moment it happened to read.
	httpx.WriteOKStatus(w, http.StatusAccepted, map[string]any{
		"started": true, "runId": runID, "testedAt": result.TestedAt,
	})
	return nil
}

// runStorageTest is the measurement itself. It is started by handleStorageConnectionTest
// and owns the probe claim until the answer is cached.
//
// The phases are reported to the claim as they finish, so a page polling this provider sees
// each row fill in while the rest is still measuring. That record is dropped when the answer
// is cached: the row's probe is the answer, and the in-process record only exists to make the
// wait readable.
func (s *Server) runStorageTest(target connections.Connection, result storageTestView) {
	defer s.releaseProbe(target.ID)
	ctx := context.Background()
	effective, err := target.ApplyEnvironment(*s.cfg)
	switch {
	case err != nil:
		result.Error = err.Error()
	default:
		measurement, measureErr := storagebackend.Measure(ctx, &effective, func(progress storagebackend.Progress) {
			s.recordProbePhase(target.ID, progress)
		})
		if measureErr != nil {
			result.Error = measureErr.Error()
			break
		}
		result.OK = true
		applyMeasurement(&result, measurement)
		// Publishability is a different question from reachability, so a store that
		// answered but cannot be admitted is "reachable, not ready" rather than a
		// failed connection test.
		if _, probeErr := storagebackend.ProbeConnection(ctx, &effective); probeErr != nil {
			result.Error = probeErr.Error()
		} else {
			result.Writable = true
		}
	}
	encoded, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		slog.Error("encode storage test result", "connection", target.ID, "error", marshalErr)
		return
	}
	if recordErr := s.conns.SetProbe(ctx, target.ID, encoded); recordErr != nil {
		// The measurement itself may have been fine; a failed cache write loses the
		// answer, which is the only way anyone reads it, so it is logged as an error.
		slog.Error("record storage test result", "connection", target.ID, "error", recordErr)
	}
}

// applyMeasurement copies one measurement's numbers into the view the page reads. The
// running record and the cached answer both go through it, so a row cannot show one number
// while in flight and a different one once it is written.
func applyMeasurement(view *storageTestView, m storagebackend.Measurement) {
	view.LatencyMS = m.Latency.Milliseconds()
	view.UploadBytes, view.DownloadBytes = m.Uploaded, m.Downloaded
	view.UploadBytesPerSecond, view.DownloadBytesPerSecond = m.UploadRate, m.DownloadRate
	view.SmallFiles = fileOpsView(m.SmallFiles)
	view.Renames = fileOpsView(m.Renames)
	view.Deletes = fileOpsView(m.Deletes)
	view.Concurrency = &concurrencyTestView{
		Streams:                 m.Concurrency.Streams,
		BytesPerStream:          m.Concurrency.BytesPerStream,
		BytesPerSecond:          m.Concurrency.Rate,
		PerStreamBytesPerSecond: m.Concurrency.Share,
		Error:                   m.Concurrency.Error,
	}
}

func fileOpsView(phase storagebackend.FileOpsPhase) *fileOpsTestView {
	return &fileOpsTestView{
		Files:        phase.Files,
		BytesPerFile: phase.BytesPerFile,
		ElapsedMS:    phase.Elapsed.Milliseconds(),
		PerSecond:    phase.PerSecond,
		Error:        phase.Error,
	}
}

// probeRun is one measurement this process is running: the run's identity, the numbers its
// finished phases produced, the phase it is on, and the phases already done.
//
// It is in memory for the same reason the claim is: the transfers it describes die with the
// process, and the answer that outlives them is written to the row when the run finishes.
// Persisting it would put a second, disagreeing copy of a disposable measurement in the
// database.
type probeRun struct {
	view  storageTestView
	phase string
	done  []string
}

// claimProbe marks a measurement as running for one provider. It lives in this process,
// like the backup runner: a deployment runs one instance, and a probe that outlived its
// process is meaningless because its transfers are gone.
func (s *Server) claimProbe(id, runID, testedAt string) bool {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if s.probeRuns == nil {
		s.probeRuns = map[string]*probeRun{}
	}
	if _, running := s.probeRuns[id]; running {
		return false
	}
	s.probeRuns[id] = &probeRun{view: storageTestView{RunID: runID, TestedAt: testedAt}}
	return true
}

// recordProbePhase moves one provider's running record forward. A finished phase contributes
// its numbers; a starting phase contributes only which row the page should be spinning, so a
// row never prints a number before the phase that produced it has completed.
func (s *Server) recordProbePhase(id string, progress storagebackend.Progress) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	run, running := s.probeRuns[id]
	if !running {
		return
	}
	if !progress.Done {
		run.phase = string(progress.Phase)
		return
	}
	run.phase = ""
	applyMeasurement(&run.view, progress.Measurement)
	run.done = append(run.done, string(progress.Phase))
}

// probeProgress is the measurement this process is running for one provider, or nil. The
// copy is taken under the lock; the phase views it points at are replaced, never mutated, so
// the page can marshal it after the lock is released.
func (s *Server) probeProgress(id string) *probeProgressView {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	run, running := s.probeRuns[id]
	if !running {
		return nil
	}
	return &probeProgressView{
		storageTestView: run.view,
		Phase:           run.phase,
		Done:            append([]string{}, run.done...),
	}
}

func (s *Server) releaseProbe(id string) {
	s.probeMu.Lock()
	delete(s.probeRuns, id)
	s.probeMu.Unlock()
}

func (s *Server) probeRunning(id string) bool {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	_, running := s.probeRuns[id]
	return running
}

// newProbeRunID names one measurement so a page can wait for exactly the answer it asked
// for. A timestamp would not do: two runs within the same second are indistinguishable.
func newProbeRunID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate a measurement run id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
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
