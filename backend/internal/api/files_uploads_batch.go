package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"

	fc "everythingabout.me/backend/internal/filecontract"
)

// Batch bounds. Create is the only call whose cost is paid per file before any bytes
// move, so one request admits a page of a small-file import; commit is bounded by the
// worker pool that settles it, eight being what one request can finish inside a couple of
// seconds. Both are page sizes, not rates: the client sends the next page when it needs it.
const (
	filesUploadCreateBatchLimit = 32
	filesUploadCommitBatchLimit = 8
)

// filesUploadBatchParallel bounds how many items of one page are admitted at once. An
// admission is mostly waiting: one provider round trip for the parent, and, for a path
// whose folders do not exist yet, a chain of four to seven more. Nothing in it is
// CPU-bound, so run one at a time a page of eight costs the sum of eight provider waits and
// the client can start its next page only when the last item returns — which is how a page
// came back in 44 seconds. Overlapped, the page costs about its slowest item. The bound is
// the queue's own in-flight window, so a page never outruns the clients it serves.
const filesUploadBatchParallel = 8

// batchRecorder is a ResponseWriter for one inner handler: the batch reuses the single
// resource's handler instead of a second implementation of admission, authorization and
// error mapping, so the two cannot drift.
type batchRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newBatchRecorder() *batchRecorder {
	return &batchRecorder{header: http.Header{}, status: http.StatusOK}
}
func (b *batchRecorder) Header() http.Header         { return b.header }
func (b *batchRecorder) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *batchRecorder) WriteHeader(code int) {
	if b.status == http.StatusOK {
		b.status = code
	}
}

// handleFilesCreateUploadBatch admits up to 32 sessions in one request. Each item is the
// same body the single create endpoint takes, plus its own idempotency key, and each item
// answers independently: one name conflict does not fail the page.
//
// An item is forwarded to the single handler as the bytes the client sent, never
// re-encoded from the decoded request: re-encoding writes every absent field as null, and
// the single handler refuses a path carrying a null destination alias. That made every
// item answer invalid_selector while the page still answered 200, so a 23k-file import
// read as "0 / 23,373" with no session and no failed request to point at.
func (s *Server) handleFilesCreateUploadBatch(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	var req struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := strictFilesJSON(r, &req); err != nil {
		return err
	}
	if len(req.Items) == 0 || len(req.Items) > filesUploadCreateBatchLimit {
		return fc.NewProblem(400, "invalid_request", fmt.Sprintf("Between one and %d items are required.", filesUploadCreateBatchLimit))
	}
	items := make([]any, len(req.Items))
	slots := make(chan struct{}, min(filesUploadBatchParallel, len(req.Items)))
	var page sync.WaitGroup
	for i, body := range req.Items {
		// The idempotency key moves into the header the single handler reads. Everything
		// else in the item is the inner handler's business.
		var item filesCreateUploadRequest
		if err := json.Unmarshal(body, &item); err != nil {
			items[i] = map[string]any{"error": fc.NewProblem(400, "invalid_request", "The item is not a JSON upload request.")}
			continue
		}
		key := item.IdempotencyKey
		if key == "" {
			key = randomFilesKey()
		}
		page.Add(1)
		slots <- struct{}{}
		go func(index int, body []byte, key string) {
			defer page.Done()
			defer func() { <-slots }()
			items[index] = s.admitUploadPageItem(r, f, body, key)
		}(i, body, key)
	}
	page.Wait()
	return writeFilesJSON(w, http.StatusOK, map[string]any{"items": items})
}

// admitUploadPageItem answers one item of a page, with the single handler doing the work.
// A panic stays inside its own item: the single handler runs under fileWrap's recoverer,
// and a goroutine does not, so one malformed item must not take the page — or the process
// — with it.
func (s *Server) admitUploadPageItem(r *http.Request, f *fileRequest, body []byte, key string) (answer any) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("upload page item panicked", "request_id", r.Header.Get("X-Request-Id"), "panic", recovered)
			answer = map[string]any{"error": fc.NewProblem(500, "internal_error", "The item could not be admitted.")}
		}
	}()
	inner := r.Clone(r.Context())
	inner.Method = http.MethodPost
	inner.URL.Path = fc.Base + "/uploads"
	inner.URL.RawQuery = ""
	inner.Body = io.NopCloser(bytes.NewReader(body))
	inner.ContentLength = int64(len(body))
	inner.Header = r.Header.Clone()
	inner.Header.Set("Idempotency-Key", key)
	rec := newBatchRecorder()
	if err := s.handleFilesCreateUpload(rec, inner, f); err != nil {
		return map[string]any{"error": filesProblem(err)}
	}
	return map[string]any{"upload": json.RawMessage(rec.body.Bytes())}
}

// commitUploadPageItem answers one item of a settle page. A commit spends a provider
// observation and a settlement, so the page has the same reason to overlap its items as
// admission does, and the same reason to keep a panic inside one item.
func (s *Server) commitUploadPageItem(r *http.Request, f *fileRequest, id string) (answer any) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("upload commit page item panicked", "request_id", r.Header.Get("X-Request-Id"), "panic", recovered)
			answer = map[string]any{"error": fc.NewProblem(500, "internal_error", "The item could not be settled.")}
		}
	}()
	inner := r.Clone(r.Context())
	inner.Method = http.MethodPost
	inner.URL.Path = fc.Base + "/uploads/" + id + "/commit"
	inner.URL.RawQuery = ""
	inner.Body = http.NoBody
	inner.ContentLength = 0
	inner.Header = r.Header.Clone()
	rec := newBatchRecorder()
	if err := s.handleFilesCommitUpload(rec, inner, f); err != nil {
		return map[string]any{"error": filesProblem(err)}
	}
	return map[string]any{"result": json.RawMessage(rec.body.Bytes())}
}

// handleFilesCommitUploadBatch settles up to eight sessions in one request. The answer per
// item is whatever the single commit answered: the published file, or the Operation that
// is still publishing it.
func (s *Server) handleFilesCommitUploadBatch(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	var req struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := strictFilesJSON(r, &req); err != nil {
		return err
	}
	if len(req.Items) == 0 || len(req.Items) > filesUploadCommitBatchLimit {
		return fc.NewProblem(400, "invalid_request", fmt.Sprintf("Between one and %d items are required.", filesUploadCommitBatchLimit))
	}
	items := make([]any, len(req.Items))
	slots := make(chan struct{}, min(filesUploadBatchParallel, len(req.Items)))
	var page sync.WaitGroup
	for i, item := range req.Items {
		if item.ID == "" {
			items[i] = map[string]any{"error": fc.NewProblem(400, "invalid_request", "Each item needs an upload id.")}
			continue
		}
		page.Add(1)
		slots <- struct{}{}
		go func(index int, id string) {
			defer page.Done()
			defer func() { <-slots }()
			items[index] = s.commitUploadPageItem(r, f, id)
		}(i, item.ID)
	}
	page.Wait()
	return writeFilesJSON(w, http.StatusOK, map[string]any{"items": items})
}
