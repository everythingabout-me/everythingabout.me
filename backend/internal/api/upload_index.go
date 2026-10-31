package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/httpx"
)

// UploadIndex observes legacy native effects; it never owns publication or bytes.
// Persisting a watch is required before the browser receives write credentials.
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
	changed, err := s.nodes.ObserveUpload(ctx, ownerID, path, isDir)
	if err != nil {
		return "", err
	}
	if changed {
		s.timeline.Notify(ownerID)
	}
	// The observer schedules independently of this value: presence is never
	// proof that all potentially delayed native requests are terminal.
	return "observed", nil
}

func (s *Server) watchUpload(ctx context.Context, ownerID, path string, isDir bool, until time.Time) error {
	if s.uploadIndex == nil {
		return httpx.NewError(http.StatusServiceUnavailable, "upload index recovery unavailable")
	}
	return s.uploadIndex.Register(ctx, ownerID, path, isDir, until)
}

func (s *Server) settleUpload(ctx context.Context, ownerID, path string, isDir bool) {
	if s.uploadIndex != nil {
		if err := s.uploadIndex.Settle(ctx, ownerID, path, isDir); err != nil {
			// The index already committed. Keeping the durable watch hot is safe;
			// failure to lower its polling frequency does not undo that success.
			slog.Warn("upload index: could not lower observation frequency", "error", err)
		}
	}
}

// StartUploadIndex runs independently of the optional timeline organizer.
func (s *Server) StartUploadIndex(ctx context.Context) {
	if s.uploadIndex != nil {
		go s.uploadIndex.Run(ctx)
	}
}
