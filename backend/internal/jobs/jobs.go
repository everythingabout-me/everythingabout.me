// Package jobs 在后台执行目录级的长操作（移动 / 复制）。
//
// 目录没有“一次性改名”的原语：key 就是路径，所以搬 N 个对象 = N 次服务端复制。
// 但对外没有“任务”这个概念——动作没做完时接口回 202 + Location，
// 进度地址就是动作自己的子路径（/move/{id}、/copy/{id}）。
// 这张表纯粹是实现细节，客户端永远不需要知道它的存在。
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

// Op 是后台作业对应的动作。它不出现在协议里，只决定 id 前缀和进度地址。
type Op string

const (
	OpMove Op = "move"
	OpCopy Op = "copy"
)

func (o Op) prefix() string {
	if o == OpCopy {
		return "c_"
	}
	return "m_"
}

// 作业的最长执行时间，防止一个跑飞的 goroutine 永远占着。
const jobTimeout = 6 * time.Hour

// Job 是进度地址返回的形状。移动和复制共用同一套字段。
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

	userID string
}

// Terminal 表示这次动作已经结束（不管成功还是失败）。
func (j *Job) Terminal() bool {
	switch j.Status {
	case "done", "failed", "cancelled":
		return true
	}
	return false
}

type Service struct {
	db      *store.DB
	storage storage.Storage
	nodes   *nodes.Service
}

func New(db *store.DB, objectStorage storage.Storage, nodeService *nodes.Service) *Service {
	service := &Service{db: db, storage: objectStorage, nodes: nodeService}
	service.recoverStale()
	return service
}

// recoverStale 把上个进程留下的未完成作业标成失败，避免它们永远停在 running。
// 单实例部署的假设；多实例要改成心跳。
func (s *Service) recoverStale() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.db.Pool().Exec(ctx,
		`update fs_jobs
		    set status = 'failed', error = '服务重启，任务中断', finished_at = now()
		  where status in ('pending', 'running')`); err != nil {
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
	user_id::text`

func scanJob(row pgx.Row) (*Job, error) {
	var (
		job Job
		op  string
	)
	err := row.Scan(&job.ID, &op, &job.From, &job.To, &job.Status, &job.Total, &job.Done,
		&job.Error, &job.CancelRequested, &job.CreatedAt, &job.StartedAt, &job.FinishedAt, &job.userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	job.Op = Op(op)
	return &job, nil
}

// Start 建作业并异步开跑。调用方负责校验来源存不存在、目标会不会撞车。
func (s *Service) Start(ctx context.Context, op Op, userID, from, to string) (*Job, error) {
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

// Get 按用户查作业；别人的作业返回 (nil, nil)。
func (s *Service) Get(ctx context.Context, userID, id string) (*Job, error) {
	return scanJob(s.db.Pool().QueryRow(ctx,
		`select `+jobColumns+` from fs_jobs where id = $1 and user_id = $2::uuid`, id, userID))
}

// Wait 最多等 budget 这么久，然后返回作业的最新状态。
// 没做完也返回（不报错）——调用方看到非终态就回 202。
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

// Cancel 请求停下。还没开始的直接判 cancelled；正在跑的搬完当前这一项就停。
// 已经结束的作业是幂等的，原样返回。
func (s *Service) Cancel(ctx context.Context, userID, id string) (*Job, error) {
	job, err := s.Get(ctx, userID, id)
	if err != nil || job == nil {
		return job, err
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

// Retry 从失败/取消的地方接着做：重新扫一遍来源，已经搬走的记录不在里面了。
func (s *Service) Retry(ctx context.Context, userID, id string) (*Job, error) {
	job, err := s.Get(ctx, userID, id)
	if err != nil || job == nil {
		return job, err
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

// jobByID 是内部用的读取，不带用户过滤。
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
		// 每 10 个看一眼要不要停，免得每个对象都多一次查询。
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
				// 旧对象删不掉最多留个孤儿，不能因此把整个作业判失败。
				slog.Warn("jobs: failed to delete old object", "key", row.Key, "error", err)
			}
		}
	}

	_, err := s.nodes.MoveNode(ctx, row.ID, target, userID, row.IsDir)
	return err
}

// copyOne 复制对象并建一条新索引。复制出来的东西是**新对象**：
// 新的一行、新的创建时间，源不动。
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

	// 元数据（sha256 之类）跟着内容一起复制过去。
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
