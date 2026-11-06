package api

import (
	"net/http"

	"everythingabout.me/backend/internal/events"
	"everythingabout.me/backend/internal/httpx"
)

// handleAPICapabilities exposes the server's limits so clients need not copy 16 MiB / 64 MiB
// into their own source. No auth, and no user information at all.
func (s *Server) handleAPICapabilities(w http.ResponseWriter, r *http.Request) error {
	threshold := s.cfg.MultipartThreshold
	if threshold <= 0 {
		threshold = uploadSingleMax
	}
	partSize := s.cfg.UploadPartSize
	if partSize < uploadMinPartSize {
		partSize = uploadMinPartSize
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"maxPutBytes":        apiMaxPutSize,
		"maxUploadBytes":     uploadMaxObject,
		"multipartThreshold": threshold,
		"partSize":           partSize,
		"maxParts":           uploadMaxParts,
		"list": map[string]any{
			"defaultLimit": apiDefaultLimit,
			"maxLimit":     apiMaxLimit,
		},
		"rateLimit":  map[string]any{"perMinute": apiRatePerMinute},
		"quotaBytes": s.cfg.QuotaBytes,
		"async": map[string]any{
			"header":             "Prefer",
			"value":              "respond-async",
			"defaultWaitSeconds": int(defaultSyncBudget.Seconds()),
			"maxWaitSeconds":     int(maxAsyncWait.Seconds()),
		},
		"events": map[string]any{
			"types":        []string{events.TypePhoto, events.TypeNote, events.TypeMetric, events.TypeHTML},
			"maxTitle":     events.MaxTitle,
			"maxBody":      events.MaxBody,
			"maxMetrics":   events.MaxMetrics,
			"maxMedia":     events.MaxMedia,
			"maxHTMLBytes": events.MaxHTMLLen,
			"htmlHeight":   map[string]any{"min": events.MinHTMLSize, "max": events.MaxHTMLSize},
		},
		"features": []string{
			"range", "if-match", "overwrite", "recursive-delete",
			"copy", "search", "upload-resume", "events",
		},
	})
	return nil
}
