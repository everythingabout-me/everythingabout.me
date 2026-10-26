// Package mover 在后台执行目录移动。
//
// 目录没有“一次性改名”的原语：key 就是路径，所以移动 N 个文件 = N 次服务端复制。
// POST /move 建一条任务立刻返回，客户端再轮询 GET /move/{id}；
// 服务重启时未完成的任务会被标成失败，用户重试即可（重试会接着剩下的做）。
package mover

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// Job 是暴露给接口的任务形状。
type Job struct {
	ID         string  `json:"id"`
	From       string  `json:"from"`
	To         string  `json:"to"`
	Status     string  `json:"status"`
	Total      int     `json:"total"`
	Done       int     `json:"done"`
	Error      string  `json:"error,omitempty"`
	CreatedAt  string  `json:"createdAt"`
	StartedAt  *string `json:"startedAt"`
	FinishedAt *string `json:"finishedAt"`

	userID string
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

// recoverStale 把上个进程留下的未完成任务标成失败，避免它们永远停在 running。
// 单实例部署的假设；多实例要改成心跳。
func (s *Service) recoverStale() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.db.Pool().Exec(ctx,
		`update move_jobs
		    set status = 'failed', error = '服务重启，任务中断', finished_at = now()
		  where status in ('pending', 'running')`); err != nil {
		slog.Warn("mover: 标记中断任务失败", "error", err)
	}
}

func newJobID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "m_" + hex.EncodeToString(buf), nil
}

const jobColumns = `id, from_path, to_path, status, total, done, coalesce(error, ''),
	to_char(created_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
	case when started_at is null then null else to_char(started_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') end,
	case when finished_at is null then null else to_char(finished_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') end,
	user_id::text`

func scanJob(row pgx.Row) (*Job, error) {
	var job Job
	err := row.Scan(&job.ID, &job.From, &job.To, &job.Status, &job.Total, &job.Done,
		&job.Error, &job.CreatedAt, &job.StartedAt, &job.FinishedAt, &job.userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &job, nil
}

// Start 建任务并异步开跑。调用方负责校验来源是目录、目标不存在。
func (s *Service) Start(ctx context.Context, userID, from, to string) (*Job, error) {
	if err := s.nodes.EnsureParents(ctx, to, userID); err != nil {
		return nil, err
	}
	id, err := newJobID()
	if err != nil {
		return nil, err
	}

	job, err := scanJob(s.db.Pool().QueryRow(ctx,
		`insert into move_jobs (id, user_id, from_path, to_path)
		 values ($1, $2::uuid, $3, $4)
		 returning `+jobColumns, id, userID, from, to))
	if err != nil {
		return nil, err
	}

	go s.run(id)
	return job, nil
}

// Get 按用户查任务；别人的任务返回 (nil, nil)。
func (s *Service) Get(ctx context.Context, userID, id string) (*Job, error) {
	job, err := scanJob(s.db.Pool().QueryRow(ctx,
		`select `+jobColumns+` from move_jobs where id = $1 and user_id = $2::uuid`, id, userID))
	if err != nil {
		return nil, err
	}
	return job, nil
}

// Retry 从失败的地方接着做：重新扫一遍来源子树，已经搬走的记录不在里面了。
func (s *Service) Retry(ctx context.Context, userID, id string) (*Job, error) {
	job, err := s.Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, nil
	}
	if job.Status == "running" || job.Status == "done" {
		return job, fmt.Errorf("任务状态是 %s，不能重试", job.Status)
	}

	if _, err := s.db.Pool().Exec(ctx,
		`update move_jobs
		    set status = 'pending', error = null, done = 0,
		        started_at = null, finished_at = null
		  where id = $1`, id); err != nil {
		return nil, err
	}
	go s.run(id)
	return s.Get(ctx, userID, id)
}

func (s *Service) run(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()

	job, err := scanJob(s.db.Pool().QueryRow(ctx, `select `+jobColumns+` from move_jobs where id = $1`, id))
	if err != nil || job == nil {
		slog.Warn("mover: 读取任务失败", "job", id, "error", err)
		return
	}
	if job.Status == "done" {
		return
	}

	if _, err := s.db.Pool().Exec(ctx,
		`update move_jobs set status = 'running', started_at = now() where id = $1`, id); err != nil {
		s.fail(ctx, id, err)
		return
	}

	rows, err := s.nodes.Subtree(ctx, job.userID, job.From)
	if err != nil {
		s.fail(ctx, id, err)
		return
	}
	if _, err := s.db.Pool().Exec(ctx,
		`update move_jobs set total = $2 where id = $1`, id, len(rows)); err != nil {
		s.fail(ctx, id, err)
		return
	}

	done := 0
	for i := range rows {
		row := &rows[i]
		target := job.To + strings.TrimPrefix(row.Path, job.From)
		if err := s.moveOne(ctx, job.userID, row, target); err != nil {
			s.fail(ctx, id, fmt.Errorf("%s: %w", row.Path, err))
			return
		}
		done++
		if done%10 == 0 {
			_, _ = s.db.Pool().Exec(ctx, `update move_jobs set done = $2 where id = $1`, id, done)
		}
	}

	if _, err := s.db.Pool().Exec(ctx,
		`update move_jobs set status = 'done', done = $2, finished_at = now() where id = $1`, id, done); err != nil {
		slog.Warn("mover: 标记完成失败", "job", id, "error", err)
	}
}

func (s *Service) moveOne(ctx context.Context, userID string, row *nodes.Row, target string) error {
	if row.Path == target {
		return nil
	}

	newKey := paths.KeyFor(target, userID)
	if row.IsDir {
		newKey = paths.DirKeyFor(target, userID)
	}

	// 目标位置可能有历史软删除记录占着同一个 key。

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
				// 旧对象删不掉最多留个孤儿，不能因此把整个任务判失败。
				slog.Warn("mover: 删除旧对象失败", "key", row.Key, "error", err)
			}
		}
	}

	_, err := s.nodes.MoveNode(ctx, row.ID, target, userID, row.IsDir)
	return err
}

func (s *Service) fail(ctx context.Context, id string, cause error) {
	slog.Warn("mover: 移动失败", "job", id, "error", cause)
	_, _ = s.db.Pool().Exec(ctx,
		`update move_jobs set status = 'failed', error = $2, finished_at = now() where id = $1`,
		id, cause.Error())
}
