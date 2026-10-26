package api

import (
	"net/http"

	"everythingabout.me/backend/internal/httpx"
)

// handleAPICapabilities 把服务端的上限亮出来，客户端不用把 32 MiB / 64 MiB
// 抄进自己的源码。免鉴权，也没有任何用户信息。
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
		"features": []string{
			"range", "if-match", "overwrite", "recursive-delete",
			"copy", "search", "upload-resume",
		},
	})
	return nil
}
