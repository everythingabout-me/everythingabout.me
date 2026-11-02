package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"everythingabout.me/backend/internal/browserupload"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/nodes"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func browserUploadError(err error) error {
	var uploadErr *browserupload.Error
	if errors.As(err, &uploadErr) {
		return httpx.NewError(uploadErr.Status, uploadErr.Message)
	}
	return err
}

func (s *Server) indexBrowserUpload(ctx context.Context, session browserupload.Session) (json.RawMessage, error) {
	if verifier, ok := s.storage.(interface {
		BrowserUploadMarker(context.Context, string, string, string) error
	}); ok {
		if err := verifier.BrowserUploadMarker(ctx, session.OwnerID, session.Path, session.ID); err != nil {
			return nil, err
		}
	}
	var modified *time.Time
	if session.ModifiedAt > 0 {
		candidate := time.UnixMilli(session.ModifiedAt)
		if candidate.Year() >= 1990 && candidate.Before(time.Now().AddDate(0, 0, 2)) {
			modified = &candidate
		}
	}
	mime := session.Mime
	node, err := s.nodes.ConfirmUpload(ctx, nodes.UpsertInput{OwnerID: session.OwnerID, Path: session.Path, Mime: &mime, SourceModifiedAt: modified, Meta: map[string]any{"mime": mime}})
	if err != nil {
		return nil, err
	}
	s.timeline.Notify(session.OwnerID)
	return json.Marshal(node)
}

func (s *Server) requireBrowserUploads() error {
	if s.browserUploads == nil {
		return httpx.NewError(http.StatusServiceUnavailable, "server-owned uploads are unavailable")
	}
	return nil
}

// lockBrowserNamespace coordinates admission with legacy browser namespace actions.
// The lock covers the conflict check and native action, never upload body reception.
func (s *Server) lockBrowserNamespace(ctx context.Context, owner string) (func(), error) {
	conn, err := s.db.Pool().Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var acquired bool
	if err = conn.QueryRow(ctx, `select pg_try_advisory_lock(hashtextextended($1,0))`, "browser-write:"+owner).Scan(&acquired); err != nil {
		conn.Release()
		return nil, err
	}
	if !acquired {
		conn.Release()
		return nil, httpx.Conflict("another file change is in progress; retry shortly")
	}
	return func() { unlockBrowserNamespace(conn, owner) }, nil
}
func unlockBrowserNamespace(conn *pgxpool.Conn, owner string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := conn.Exec(ctx, `select pg_advisory_unlock(hashtextextended($1,0))`, "browser-write:"+owner); err != nil {
		// A session-scoped lock must never escape back into the shared pool.
		_ = conn.Conn().Close(context.Background())
	}
	conn.Release()
}

func (s *Server) checkBrowserUploadConflict(ctx context.Context, owner, path string, subtree bool) error {
	if s.browserUploads != nil {
		if err := s.browserUploads.CheckConflict(ctx, owner, path, subtree); err != nil {
			return browserUploadError(err)
		}
	}
	// Only an explicit completed deletion releases a waiting upload. Return its
	// owner-scoped identity so clients never infer completion from a missing path
	// or mistake this reservation for an overwrite/name collision.
	var deletionID, deletionPath, deletionStatus string
	err := s.db.Pool().QueryRow(ctx, `select id,from_path,status from fs_jobs
		where user_id=$1::uuid and op='delete' and status<>'done' and delete_binding=$3
		and (from_path=$2 or left(from_path,length($2)+1)=$2||'/'
		  or left($2,length(from_path)+1)=from_path||'/')
		order by created_at,id limit 1`, owner, path, uploadIndexBinding(s.cfg)).Scan(&deletionID, &deletionPath, &deletionStatus)
	if err == nil {
		return &httpx.Error{Status: http.StatusConflict,
			Message: "this path is waiting for permanent deletion to finish",
			Code:    "deletion_in_progress", OperationID: deletionID, Path: deletionPath, DeletionStatus: deletionStatus}
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	// Other operations retain their existing generic conflict behavior.
	var active bool
	err = s.db.Pool().QueryRow(ctx, `select exists(select 1 from fs_jobs
		where user_id=$1::uuid and op<>'delete' and status in ('pending','running')
		and exists(select 1 from unnest(array[from_path,to_path]) as target(path)
		  where target.path=$2 or target.path=''
		    or left(target.path,length($2)+1)=$2||'/'
		    or left($2,length(target.path)+1)=target.path||'/'))`, owner, path).Scan(&active)
	if err != nil {
		return err
	}
	if active {
		return httpx.Conflict("a file operation affecting this path is unfinished")
	}
	return nil
}

// lockBrowserMutation admits legacy public actions into the browser namespace.
// Internal job helpers run under their caller's lock and must not acquire it again.
func (s *Server) lockBrowserMutation(ctx context.Context, owner string, targets ...string) (func(), error) {
	unlock, err := s.lockBrowserNamespace(ctx, owner)
	if err != nil {
		return nil, err
	}
	for _, path := range targets {
		if err := s.checkBrowserUploadConflict(ctx, owner, path, true); err != nil {
			unlock()
			return nil, err
		}
	}
	return unlock, nil
}

// Presigned requests can outlive expiry, completion, or cancellation. Legacy
// session evidence and unresolved watches require explicit operator retirement.
func (s *Server) checkDeleteLegacyUploads(ctx context.Context, owner, path string) error {
	var active bool
	err := s.db.Pool().QueryRow(ctx, `select
		exists(select 1 from upload_sessions where user_id=$1::uuid
		  and (path=$2 or left(path,length($2)+1)=$2||'/'))
		or exists(select 1 from upload_index_watches
		  where owner_id=$1::uuid and binding=$3 and not settled
		  and (path=$2 or left(path,length($2)+1)=$2||'/'))`, owner, path, uploadIndexBinding(s.cfg)).Scan(&active)
	if err != nil {
		return err
	}
	if active {
		return httpx.Conflict("legacy uploads affecting this path require verified settlement before deletion")
	}
	return nil
}
