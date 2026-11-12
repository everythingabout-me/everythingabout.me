package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/paths"
)

// UploadIndex recovers old direct-upload grants and temporary folder markers.
// New browser files use server-owned temporary upload receipts instead.
type UploadIndex interface {
	Register(context.Context, string, string, bool, time.Time) error
	Settle(context.Context, string, string, bool) error
	Run(context.Context)
}

func uploadIndexBinding(cfg *config.Config) string {
	// Root identity excludes credentials: rotating secrets must not strand old
	// observations, while switching bucket/connection generation must isolate them.
	binding, _ := json.Marshal([]any{"legacy-cos", cfg.Storage.ConnectionID, cfg.Storage.Generation, cfg.COS.Bucket, cfg.COS.Region, "drive/<owner>/"})
	digest := sha256.Sum256(binding)
	return hex.EncodeToString(digest[:])
}

func (s *Server) observeUploadedObject(ctx context.Context, ownerID, path string, isDir bool) (string, error) {
	unlock, err := s.lockBrowserNamespace(ctx, ownerID)
	if err != nil {
		return "", err
	}
	defer unlock()
	var deleting bool
	if err := s.db.Pool().QueryRow(ctx, `select exists(select 1 from fs_jobs
		where user_id=$1::uuid and op='delete' and status<>'done' and delete_binding=$3
		and (from_path=$2 or left($2,length(from_path)+1)=from_path||'/'))`, ownerID, path, uploadIndexBinding(s.cfg)).Scan(&deleting); err != nil {
		return "", err
	}
	if deleting {
		return "", httpx.Conflict("background deletion reserves this path")
	}
	key := paths.KeyFor(path, ownerID)
	if isDir {
		key = paths.DirKeyFor(path, ownerID)
	}
	info, err := s.storage.Head(ctx, key)
	if err != nil {
		return "", err
	}
	if info == nil {
		return "", nil
	}
	changed, err := s.nodes.ObserveUpload(ctx, ownerID, path, isDir)
	if err != nil {
		return "", err
	}
	if changed {
		s.timeline.Notify(ownerID)
	}
	if isDir {
		node, err := s.nodes.Live(ctx, path, ownerID)
		if err != nil {
			return "", err
		}
		if node == nil || !node.IsDir {
			return "", nil
		}
	}
	// Directory markers created by the backend retire once indexed. Legacy
	// unresolved file grants retain separate observation until cutover is drained.
	return "observed", nil
}

// StartUploadIndex runs independently of the optional timeline organizer. Each Run
// drains its own in-flight calls before returning; the goroutines are tracked so
// WaitBackground can keep the pool open until they have.
func (s *Server) StartUploadIndex(ctx context.Context) {
	if s.cfg.Storage.CoordinatedWrites {
		return
	}
	if s.browserUploads != nil {
		s.background.Add(1)
		go func() {
			defer s.background.Done()
			s.browserUploads.Run(ctx)
		}()
	}
	if s.uploadIndex != nil {
		s.background.Add(1)
		go func() {
			defer s.background.Done()
			s.uploadIndex.Run(ctx)
		}()
	}
}
