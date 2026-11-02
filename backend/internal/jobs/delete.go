package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SetDeleteBinding is called once at startup, before admission or workers start.
// Credentials are deliberately excluded; bucket/root generations are not.
func (s *Service) SetDeleteBinding(binding string) { s.deleteBinding = binding }

// StartDelete captures a finite manifest and detaches the index in one transaction.
// The caller holds the owner's browser-write namespace lock and checks all live
// upload grants and conflicting operations before calling. No provider I/O occurs.
func (s *Service) StartDelete(ctx context.Context, owner, path string) (*Job, error) {
	if s.deleteBinding == "" {
		return nil, fmt.Errorf("deletion storage binding is unavailable")
	}
	if normalized, err := paths.Normalize(path); err != nil || normalized != path || path == "" {
		return nil, httpx.BadRequest("deletion requires a normalized non-root path")
	}
	id, err := newJobID(OpDelete)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	_, err = tx.Exec(ctx, `insert into fs_jobs(id,user_id,op,from_path,to_path,delete_binding)
 values($1,$2::uuid,'delete',$3,'',$4)`, id, owner, path, s.deleteBinding)
	if err != nil {
		return nil, err
	}
	// DELETE RETURNING locks the captured rows. Concurrent observations cannot
	// resurrect them through a stale serializable upload snapshot.
	tag, err := tx.Exec(ctx, `with captured as (
 delete from nodes where owner_id=$2::uuid and (path=$3 or path like $4)
 returning path,key,is_dir,etag,size
 ) insert into fs_delete_entries(job_id,owner_id,path,key,is_dir,etag,size)
 select $1,$2::uuid,path,key,is_dir,etag,size from captured`, id, owner, path, nodes.EscapeLike(path)+"/%")
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, httpx.NotFound("nothing to delete")
	}
	_, err = tx.Exec(ctx, `update fs_jobs set total=$2 where id=$1`, id, tag.RowsAffected())
	if err != nil {
		return nil, err
	}
	job, err := scanJob(tx.QueryRow(ctx, `select `+jobColumns+` from fs_jobs where id=$1 and user_id=$2::uuid`, id, owner))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	select {
	case s.deleteWake <- struct{}{}:
	default:
	}
	return job, nil
}

// PendingDeletes also returns blocked failures: hiding a path is not proof that
// its native files are gone. The UI must keep those outcomes visible after reload.
func (s *Service) PendingDeletes(ctx context.Context, owner string) ([]Job, error) {
	rows, err := s.db.Pool().Query(ctx, `select `+jobColumns+` from fs_jobs
 where user_id=$1::uuid and op='delete' and status<>'done'
 order by created_at,id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Job, 0)
	for rows.Next() {
		j, e := scanJob(rows)
		if e != nil {
			return nil, e
		}
		s.presentDelete(j)
		out = append(out, *j)
	}
	return out, rows.Err()
}

// RunDeletes uses bounded passes and a session lock per operation. A second
// process may run this loop, but cannot issue the same native request concurrently.
func (s *Service) RunDeletes(ctx context.Context) {
	timer := time.NewTicker(2 * time.Second)
	defer timer.Stop()
	for {
		if err := s.RunDeletesOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("delete cleanup: pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-s.deleteWake:
		}
	}
}

func (s *Service) RunDeletesOnce(ctx context.Context) error {
	if s.deleteBinding == "" {
		return nil
	}
	rows, err := s.db.Pool().Query(ctx, `select id from fs_jobs where op='delete'
 and delete_binding=$1 and status in ('pending','running') and next_attempt_at<=now()
 order by created_at,id limit 4`, s.deleteBinding)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.runDeleteBatch(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

type deleteEntry struct {
	path, key, state string
	isDir            bool
	etag             *string
	size             int64
}

func (s *Service) runDeleteBatch(parent context.Context, id string) error {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	conn, err := s.db.Pool().Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	lockKey := "delete-worker:" + id
	if err = conn.QueryRow(ctx, `select pg_try_advisory_lock(hashtextextended($1,0))`, lockKey).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	defer func() {
		unlockCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, e := conn.Exec(unlockCtx, `select pg_advisory_unlock(hashtextextended($1,0))`, lockKey); e != nil {
			_ = conn.Conn().Close(context.Background())
		}
	}()
	job, err := scanJob(conn.QueryRow(ctx, `select `+jobColumns+` from fs_jobs where id=$1 and op='delete' and delete_binding=$2 and status in ('pending','running')`, id, s.deleteBinding))
	if err != nil || job == nil {
		return err
	}
	_, err = conn.Exec(ctx, `update fs_jobs set status='running',started_at=coalesce(started_at,now()) where id=$1 and user_id=$2::uuid`, id, job.userID)
	if err != nil {
		return err
	}
	for i := 0; i < 32; i++ {
		var entry deleteEntry
		err = conn.QueryRow(ctx, `select path,key,is_dir,etag,size,state from fs_delete_entries
   where job_id=$1 and owner_id=$2::uuid and state<>'done'
   order by length(path) desc,path limit 1`, id, job.userID).Scan(&entry.path, &entry.key, &entry.isDir, &entry.etag, &entry.size, &entry.state)
		if errors.Is(err, pgx.ErrNoRows) {
			_, err = conn.Exec(ctx, `update fs_jobs set status='done',done=total,error=null,finished_at=now() where id=$1 and user_id=$2::uuid`, id, job.userID)
			return err
		}
		if err != nil {
			return err
		}
		if entry.state == "removing" {
			return s.blockDelete(conn, id, job.userID, "A previous delete request has an unknown outcome; operator recovery is required.")
		}
		info, headErr := s.storage.Head(ctx, entry.key)
		if headErr != nil {
			// No mutation has started; retrying this read is safe even after restart.
			slog.Warn("delete cleanup: will retry metadata read", "job", id, "error", headErr)
			retryCtx, stop := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
			defer stop()
			_, err = conn.Exec(retryCtx, `update fs_jobs set status='pending',error='Storage is temporarily unavailable; cleanup will retry.',next_attempt_at=now()+interval '15 seconds' where id=$1 and user_id=$2::uuid`, id, job.userID)
			return err
		}
		if info != nil {
			if !deleteEntryMatches(entry, info) {
				return s.blockDelete(conn, id, job.userID, "The stored file changed after deletion was accepted; operator recovery is required.")
			}
			// This write MUST survive before any native request. A lost response or
			// crash cannot turn an old unconditional delete into an automatic replay.
			_, err = conn.Exec(ctx, `update fs_delete_entries set state='removing' where job_id=$1 and owner_id=$2::uuid and path=$3`, id, job.userID, entry.path)
			if err != nil {
				return err
			}
			if err = s.storage.Remove(ctx, entry.key); err != nil {
				slog.Warn("delete cleanup: native outcome unresolved", "job", id, "error", err)
				return s.blockDelete(conn, id, job.userID, "A delete request could not be confirmed; operator recovery is required.")
			}
		}
		// A lost checkpoint leaves removing, and therefore retains the reservation.
		tx, e := conn.Begin(ctx)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `update fs_delete_entries set state='done' where job_id=$1 and owner_id=$2::uuid and path=$3`, id, job.userID, entry.path)
		if e == nil {
			_, e = tx.Exec(ctx, `update fs_jobs set done=done+1,error=null where id=$1 and user_id=$2::uuid`, id, job.userID)
		}
		if e == nil {
			e = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(context.WithoutCancel(parent))
		}
		if e != nil {
			return e
		}
	}
	select {
	case s.deleteWake <- struct{}{}:
	default:
	}
	return nil
}

func deleteEntryMatches(entry deleteEntry, info *storage.ObjectInfo) bool {
	if info.Key != "" && info.Key != entry.key {
		return false
	}
	if !entry.isDir && info.Size != entry.size {
		return false
	}
	return entry.etag == nil || *entry.etag == "" || *entry.etag == info.ETag
}

func (s *Service) blockDelete(conn *pgxpool.Conn, id, owner, message string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := conn.Exec(ctx, `update fs_jobs set status='failed',error=$3,finished_at=now() where id=$1 and user_id=$2::uuid and op='delete'`, id, owner, message)
	return err
}

// Present paused jobs from other roots as blocked without executing them against
// the currently configured provider. Restoring the original binding can resume.
func (s *Service) presentDelete(job *Job) {
	if job != nil && job.Op == OpDelete && job.Status != "done" && job.deleteBinding != s.deleteBinding {
		job.Status = "failed"
		job.Error = "Storage configuration changed; operator recovery is required."
	}
}
