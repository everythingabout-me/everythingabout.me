// Package jobs runs directory-level long operations (move / copy) in the background.
//
// Directories have no "rename in one shot" primitive: the key is the path, so moving N
// objects means N server-side copies. Externally, though, there is no "task" concept —
// while the action is unfinished the API returns 202 + Location, and the progress URL is the
// action's own subpath (/move/{id}, /copy/{id}). This table is purely an implementation
// detail; clients never need to know it exists.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
)

// Op is the action a background job performs. It never appears in the protocol; it only
// decides the id prefix and the progress URL.
type Op string

const (
	OpMove   Op = "move"
	OpCopy   Op = "copy"
	OpDelete Op = "delete"
)

func (o Op) prefix() string {
	if o == OpDelete {
		return "d_"
	}
	if o == OpCopy {
		return "c_"
	}
	return "m_"
}

// Upper bound on how long a job may run, so a runaway goroutine cannot hold on forever.
const jobTimeout = 6 * time.Hour

// Job is the shape returned at the progress URL. Move and copy share the same set of fields.
type Job struct {
	ID              string  `json:"id"`
	Op              Op      `json:"op"`
	From            string  `json:"from"`
	To              string  `json:"to"`
	Status          string  `json:"status"`
	Total           int     `json:"total"`
	Done            int     `json:"done"`
	Error           string  `json:"error,omitempty"`
	CancelRequested bool    `json:"cancelRequested"`
	CreatedAt       string  `json:"createdAt"`
	StartedAt       *string `json:"startedAt"`
	FinishedAt      *string `json:"finishedAt"`

	userID        string
	deleteBinding string
}

// Terminal reports that the action has ended, whether it succeeded or failed.
func (j *Job) Terminal() bool {
	switch j.Status {
	case "done", "failed", "cancelled":
		return true
	}
	return false
}

type Service struct {
	db            *store.DB
	storage       storage.Storage
	nodes         *nodes.Service
	deleteBinding string
	deleteWake    chan struct{}
}

func New(db *store.DB, objectStorage storage.Storage, nodeService *nodes.Service) *Service {
	service := &Service{db: db, storage: objectStorage, nodes: nodeService, deleteWake: make(chan struct{}, 1)}
	service.recoverStale()
	return service
}

// recoverStale marks unfinished jobs left behind by the previous process as failed, so they
// do not sit in running forever. This assumes a single instance; multiple instances would
// need heartbeats instead.
func (s *Service) recoverStale() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.db.Pool().Exec(ctx,
		`update fs_jobs
		    set status = 'failed', error = 'server restarted; job interrupted', finished_at = now()
		  where status in ('pending', 'running') and op <> 'delete'`); err != nil {
		slog.Warn("jobs: failed to mark interrupted jobs", "error", err)
	}
}

func newJobID(op Op) (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return op.prefix() + hex.EncodeToString(buf), nil
}

const jobColumns = `id, op, from_path, to_path, status, total, done, coalesce(error, ''),
	cancel_requested_at is not null,
	to_char(created_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
	case when started_at is null then null else to_char(started_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') end,
	case when finished_at is null then null else to_char(finished_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') end,
	user_id::text, delete_binding`

func scanJob(row pgx.Row) (*Job, error) {
	var (
		job Job
		op  string
	)
	err := row.Scan(&job.ID, &op, &job.From, &job.To, &job.Status, &job.Total, &job.Done,
		&job.Error, &job.CancelRequested, &job.CreatedAt, &job.StartedAt, &job.FinishedAt, &job.userID, &job.deleteBinding)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	job.Op = Op(op)
	return &job, nil
}

// Start creates a job and starts it asynchronously. The caller is responsible for checking
// that the source exists and the target does not collide.
func (s *Service) Start(ctx context.Context, op Op, userID, from, to string) (*Job, error) {
	if op != OpMove && op != OpCopy {
		return nil, fmt.Errorf("use durable deletion admission for delete operations")
	}
	if err := s.nodes.EnsureParents(ctx, to, userID); err != nil {
		return nil, err
	}
	id, err := newJobID(op)
	if err != nil {
		return nil, err
	}

	job, err := scanJob(s.db.Pool().QueryRow(ctx,
		`insert into fs_jobs (id, user_id, op, from_path, to_path)
		 values ($1, $2::uuid, $3, $4, $5)
		 returning `+jobColumns, id, userID, string(op), from, to))
	if err != nil {
		return nil, err
	}

	go s.run(id)
	return job, nil
}

// Get loads a job scoped to the user; someone else's job returns (nil, nil).
func (s *Service) Get(ctx context.Context, userID, id string) (*Job, error) {
	job, err := scanJob(s.db.Pool().QueryRow(ctx,
		`select `+jobColumns+` from fs_jobs where id = $1 and user_id = $2::uuid`, id, userID))
	s.presentDelete(job)
	return job, err
}

// Wait blocks for at most budget, then returns the job's latest state. It returns even when
// the job is unfinished (that is not an error) — the caller answers 202 on a non-terminal state.
func (s *Service) Wait(ctx context.Context, userID, id string, budget time.Duration) (*Job, error) {
	deadline := time.Now().Add(budget)
	for {
		job, err := s.Get(ctx, userID, id)
		if err != nil || job == nil {
			return job, err
		}
		if job.Terminal() || !time.Now().Before(deadline) {
			return job, nil
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return job, ctx.Err()
		case <-timer.C:
		}
	}
}

// Cancel asks the job to stop. One that has not started is marked cancelled outright; a
// running one stops after finishing the item at hand. On a finished job this is a no-op that
// returns it unchanged.
func (s *Service) Cancel(ctx context.Context, userID, id string) (*Job, error) {
	job, err := s.Get(ctx, userID, id)
	if err != nil || job == nil {
		return job, err
	}
	if job.Op == OpDelete {
		return nil, fmt.Errorf("accepted permanent deletions cannot be cancelled")
	}
	if job.Terminal() {
		return job, nil
	}
	if _, err := s.db.Pool().Exec(ctx,
		`update fs_jobs
		    set cancel_requested_at = now(),
		        status       = case when status = 'pending' then 'cancelled' else status end,
		        finished_at  = case when status = 'pending' then now() else finished_at end
		  where id = $1 and user_id = $2::uuid`, id, userID); err != nil {
		return nil, err
	}
	return s.Get(ctx, userID, id)
}

// Retry picks up where failure or cancellation left off: it rescans the source, and rows
// already moved are no longer in it.
func (s *Service) Retry(ctx context.Context, userID, id string) (*Job, error) {
	job, err := s.Get(ctx, userID, id)
	if err != nil || job == nil {
		return job, err
	}
	if job.Op == OpDelete {
		return nil, fmt.Errorf("uncertain deletions require operator recovery")
	}
	switch job.Status {
	case "pending", "running":
		return job, fmt.Errorf("job is still running; cannot retry")
	case "done":
		return job, fmt.Errorf("job already finished; no retry needed")
	}

	if _, err := s.db.Pool().Exec(ctx,
		`update fs_jobs
		    set status = 'pending', error = null, done = 0,
		        cancel_requested_at = null, started_at = null, finished_at = null
		  where id = $1`, id); err != nil {
		return nil, err
	}
	go s.run(id)
	return s.Get(ctx, userID, id)
}

// jobByID is the internal read, with no user filter.
func (s *Service) jobByID(ctx context.Context, id string) (*Job, error) {
	return scanJob(s.db.Pool().QueryRow(ctx,
		`select `+jobColumns+` from fs_jobs where id = $1`, id))
}

func (s *Service) run(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()

	job, err := s.jobByID(ctx, id)
	if err != nil || job == nil {
		slog.Warn("jobs: failed to load job", "job", id, "error", err)
		return
	}
	if job.Status == "done" || job.Status == "cancelled" {
		return
	}
	if job.CancelRequested {
		s.finish(ctx, id, "cancelled", 0, nil)
		return
	}

	if _, err := s.db.Pool().Exec(ctx,
		`update fs_jobs set status = 'running', started_at = now() where id = $1`, id); err != nil {
		s.fail(ctx, id, err)
		return
	}

	rows, err := s.nodes.Subtree(ctx, job.userID, job.From)
	if err != nil {
		s.fail(ctx, id, err)
		return
	}
	if _, err := s.db.Pool().Exec(ctx,
		`update fs_jobs set total = $2 where id = $1`, id, len(rows)); err != nil {
		s.fail(ctx, id, err)
		return
	}

	done := 0
	for i := range rows {
		row := &rows[i]
		// Check for a stop request every 10 items, so each object does not cost an extra query.
		if i%10 == 0 && s.cancelRequested(ctx, id) {
			s.finish(ctx, id, "cancelled", done, nil)
			return
		}
		target := job.To + strings.TrimPrefix(row.Path, job.From)
		if err := s.one(ctx, job, row, target); err != nil {
			s.fail(ctx, id, fmt.Errorf("%s: %w", row.Path, err))
			return
		}
		done++
		if done%10 == 0 {
			_, _ = s.db.Pool().Exec(ctx, `update fs_jobs set done = $2 where id = $1`, id, done)
		}
	}

	s.finish(ctx, id, "done", done, nil)
}

func (s *Service) one(ctx context.Context, job *Job, row *nodes.Row, target string) error {
	if job.Op == OpCopy {
		return s.copyOne(ctx, job.userID, row, target)
	}
	return s.moveOne(ctx, job.userID, row, target)
}

func (s *Service) moveOne(ctx context.Context, userID string, row *nodes.Row, target string) error {
	if row.Path == target {
		return nil
	}

	newKey := paths.KeyFor(target, userID)
	if row.IsDir {
		newKey = paths.DirKeyFor(target, userID)
	}

	if row.Key != newKey {
		info, err := s.storage.Head(ctx, row.Key)
		if err != nil {
			return err
		}
		if info != nil {
			if err := s.storage.Copy(ctx, row.Key, newKey); err != nil {
				return err
			}
			if err := s.storage.Remove(ctx, row.Key); err != nil {
				// A leftover old object is at worst an orphan; it must not fail the whole job.
				slog.Warn("jobs: failed to delete old object", "key", row.Key, "error", err)
			}
		}
	}

	_, err := s.nodes.MoveNode(ctx, row.ID, target, userID, row.IsDir)
	return err
}

// copyOne copies the object and creates a new index row. What gets copied is a **new
// object**: a new row with a new creation time, and the source stays as it is.
func (s *Service) copyOne(ctx context.Context, userID string, row *nodes.Row, target string) error {
	newKey := paths.KeyFor(target, userID)
	if row.IsDir {
		newKey = paths.DirKeyFor(target, userID)
	}

	info, err := s.storage.Head(ctx, row.Key)
	if err != nil {
		return err
	}
	if info != nil {
		if err := s.storage.Copy(ctx, row.Key, newKey); err != nil {
			return err
		}
	}

	if row.IsDir {
		_, err := s.nodes.CreateDirectory(ctx, target, userID)
		return err
	}

	// Metadata (sha256 and the like) is copied over together with the content.
	var meta map[string]any
	if len(row.Meta) > 0 {
		_ = json.Unmarshal(row.Meta, &meta)
	}
	var modified *time.Time
	if row.SourceModifiedAt != nil {
		if parsed, err := time.Parse(time.RFC3339, *row.SourceModifiedAt); err == nil {
			modified = &parsed
		}
	}
	_, err = s.nodes.UpsertFile(ctx, nodes.UpsertInput{
		OwnerID:          userID,
		Path:             target,
		Size:             row.Size,
		Mime:             row.Mime,
		ETag:             row.ETag,
		StorageClass:     row.StorageClass,
		SourceModifiedAt: modified,
		Meta:             meta,
	})
	return err
}

func (s *Service) cancelRequested(ctx context.Context, id string) bool {
	var requested bool
	if err := s.db.Pool().QueryRow(ctx,
		`select cancel_requested_at is not null from fs_jobs where id = $1`, id).Scan(&requested); err != nil {
		slog.Warn("jobs: failed to read cancel flag", "job", id, "error", err)
		return false
	}
	return requested
}

func (s *Service) finish(ctx context.Context, id, status string, done int, cause error) {
	if status == "done" {
		if _, err := s.db.Pool().Exec(ctx,
			`update fs_jobs set status = 'done', done = $2, finished_at = now() where id = $1`,
			id, done); err != nil {
			slog.Warn("jobs: failed to mark job done", "job", id, "error", err)
		}
		return
	}
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	if _, err := s.db.Pool().Exec(ctx,
		`update fs_jobs set status = $3, done = $2, error = nullif($4, ''), finished_at = now()
		  where id = $1`, id, done, status, message); err != nil {
		slog.Warn("jobs: failed to mark job finished", "job", id, "status", status, "error", err)
	}
}

func (s *Service) fail(ctx context.Context, id string, cause error) {
	slog.Warn("jobs: job failed", "job", id, "error", cause)
	if _, err := s.db.Pool().Exec(ctx,
		`update fs_jobs set status = 'failed', error = $2, finished_at = now() where id = $1`,
		id, cause.Error()); err != nil {
		slog.Warn("jobs: failed to mark job failed", "job", id, "error", err)
	}
}
