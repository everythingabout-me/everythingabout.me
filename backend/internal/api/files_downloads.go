package api

import (
	"mime"
	"net/http"
	"strconv"
	"time"

	"everythingabout.me/backend/internal/filechanges"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/storage"
)

type filesDownloadsRequest struct {
	Items       []fc.EntrySelector `json:"items"`
	Disposition string             `json:"disposition"`
}

func (s *Server) handleFilesDownloads(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	var req filesDownloadsRequest
	if err := strictFilesJSON(r, &req); err != nil {
		return err
	}
	if len(req.Items) == 0 || len(req.Items) > 100 {
		return fc.NewProblem(400, "invalid_request", "Between one and 100 read selectors are required.")
	}
	disposition := "attachment"
	if req.Disposition != "" {
		if req.Disposition != "attachment" && req.Disposition != "inline" {
			return fc.NewProblem(400, "invalid_request", "Disposition must be attachment or inline.")
		}
		disposition = req.Disposition
	}
	now := time.Now().UTC()
	grantExpiry := filesGrantExpiry(now)
	class := s.filesTransferClass()
	items := make([]any, 0, len(req.Items))
	for _, selector := range req.Items {
		item, path, entryID, err := s.filesDownloadGrant(r, f, selector, disposition, class, grantExpiry)
		if err != nil {
			problem := filesProblem(err)
			if problem.RequestID == "" {
				problem.RequestID = w.Header().Get("X-Request-Id")
			}
			items = append(items, problem)
			continue
		}
		// A direct class A grant bypasses the application byte path, so issuing it
		// is the durable admission point. A failure to record it must not silently
		// grant provider authority to a third-party credential.
		allowed := true
		audit := &filechanges.Audit{OwnerID: f.Principal.User.ID, Action: "download_grant", Outcome: "completed", Allowed: &allowed, Status: http.StatusOK, Path: &path, EntryID: &entryID, Actor: filesAuditActor(f.Principal), RequestID: w.Header().Get("X-Request-Id"), Origin: "http", ObservedAt: time.Now().UTC()}
		if _, auditErr := s.recordFilesAudit(r.Context(), audit); auditErr != nil {
			if f.Principal.Token != nil {
				problem := fc.NewProblem(503, "audit_unavailable", "The read grant audit could not be recorded.")
				problem.RequestID = w.Header().Get("X-Request-Id")
				items = append(items, problem)
				continue
			}
			filesAuditIncomplete(audit, "grant")
		}
		items = append(items, item)
	}
	return writeFilesJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) filesDownloadGrant(r *http.Request, f *fileRequest, selector fc.EntrySelector, disposition, class string, expiry time.Time) (map[string]any, fc.Path, string, error) {
	if err := selector.Validate(); err != nil {
		return nil, "", "", err
	}
	p, err := s.filesResolveSelector(r.Context(), f, selector)
	if err != nil {
		return nil, "", "", err
	}
	if err := f.RequireRead(p); err != nil {
		return nil, "", "", err
	}
	metadata, err := s.filesIndexedMetadata(r.Context(), f, p)
	if err != nil {
		return nil, "", "", err
	}
	if metadata.Kind != "file" {
		return nil, "", "", fc.NewProblem(409, "not_a_file", "The selected entry is not a file.")
	}
	mediaType := "application/octet-stream"
	if metadata.MediaType != nil && *metadata.MediaType != "" {
		if kind, params, parseErr := mime.ParseMediaType(*metadata.MediaType); parseErr == nil {
			mediaType = mime.FormatMediaType(kind, params)
		}
	}
	// Only the safe media allowlist may use inline disposition. A caller asking
	// for inline HTML/SVG would otherwise turn a read grant into active content.
	inline := disposition == "inline" && filesSafeInline(mediaType)
	kind := "url"
	urlValue := ""
	headers := map[string]string{}
	conditions := []string{}
	etag := ""
	size := int64(0)
	if metadata.Size != nil {
		size = *metadata.Size
	}
	descriptor := f.Driver.Describe()
	if class == storage.TransferRelay {
		selected := selector
		if !selector.IsID() {
			selected = fc.EntrySelector{Path: p}
		}
		urlValue = filesContentSelectorURL(selected, inline)
		if descriptor.PinnedRead && descriptor.RangeRead {
			conditions = []string{"if-range"}
		}
		if descriptor.PinnedRead && metadata.ContentRevision != nil {
			etag = strconv.Quote(*metadata.ContentRevision)
		}
	} else {
		driver := filesUnderlyingDriver(f.Driver)
		granter, ok := driver.(storage.ReadGranter)
		if !ok {
			return nil, "", "", fc.NewProblem(503, "temporarily_unavailable", "This connection cannot issue direct read grants.")
		}
		grant, grantErr := granter.GrantRead(r.Context(), storage.Path(p.Relative()), storage.ReadGrantOptions{MediaType: mediaType, Disposition: func() string {
			if inline {
				return "inline"
			}
			return "attachment"
		}(), ExpiresAt: expiry})
		if grantErr != nil {
			return nil, "", "", grantErr
		}
		kind = grant.Kind
		urlValue = grant.URL
		if grant.Headers != nil {
			headers = grant.Headers
		}
		conditions = append(conditions, grant.Conditions...)
		expiry = grant.ExpiresAt
	}
	item := map[string]any{
		"id": metadata.ID, "kind": kind, "url": urlValue, "headers": headers,
		"expiresAt": expiry.UTC(), "size": size, "contentType": mediaType,
		"acceptRanges": descriptor.RangeRead, "conditions": conditions,
	}
	if etag != "" {
		item["etag"] = etag
	}
	return item, metadata.Path, metadata.ID, nil
}
