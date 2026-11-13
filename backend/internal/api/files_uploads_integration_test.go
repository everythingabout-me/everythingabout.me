package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storagebackend"
)

// These HTTP tests reuse the explicit disposable database fixture. All byte paths
// are newly created temporary native roots; application environment is never read.
type uploadsHTTPFixture struct {
	f       *fileWorkerFixture
	root    fileops.Root
	server  *Server
	handler http.Handler
	bearer  string
	cookie  string
}

func newUploadsHTTPFixture(t *testing.T, f *fileWorkerFixture) *uploadsHTTPFixture {
	t.Helper()
	root, _ := f.owner(t)
	// P5 separately reserves chunks, the assembled stage and concurrent ingress.
	if _, err := f.db.Pool().Exec(f.ctx, `update file_budgets set logical_limit=$2,physical_limit=$3 where owner_id=$1`, root.OwnerID, int64(1<<30), int64(2<<30)); err != nil {
		t.Fatal(err)
	}
	bearer, _ := f.token(t, root.OwnerID, "", true, true)
	cookie, _, err := f.auth.CreateSession(f.ctx, root.OwnerID, "P5 HTTP test", "")
	if err != nil {
		t.Fatal(err)
	}
	h := &uploadsHTTPFixture{f: f, root: root, bearer: bearer, cookie: cookie}
	h.restart(t)
	return h
}
func (h *uploadsHTTPFixture) restart(t *testing.T) {
	t.Helper()
	s := h.f.server()
	workerCtx, cancel := context.WithCancel(h.f.ctx)
	s.workers().ctx = workerCtx
	t.Cleanup(func() {
		cancel()
		deadline := time.Now().Add(5 * time.Second)
		for {
			w := s.workers()
			w.mu.Lock()
			active := len(w.active)
			w.mu.Unlock()
			if active == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Error("upload worker did not stop before fixture cleanup")
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
	h.server = s
	h.handler = s.Handler()
}
func (h *uploadsHTTPFixture) request(method, target, credential string, body []byte, headers http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://files.example"+target, bytes.NewReader(body)).WithContext(h.f.ctx)
	r.Header = headers.Clone()
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	if credential == "cookie" {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: h.cookie})
	} else if credential != "" {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	if length := r.Header.Get("Content-Length"); length != "" {
		r.ContentLength, _ = strconv.ParseInt(length, 10, 64)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}
func uploadsHTTPJSON(t *testing.T, w *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP %d want %d headers=%v body=%s", w.Code, status, w.Header(), w.Body.String())
	}
	var value map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if _, ok := value["ok"]; ok {
		t.Fatal("legacy envelope exposed")
	}
	return value
}
func uploadsHTTPProblem(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	value := uploadsHTTPJSON(t, w, status)
	if value["code"] != code || w.Header().Get("Content-Type") != "application/problem+json" || value["requestId"] == "" {
		t.Fatalf("invalid Problem: %v headers=%v", value, w.Header())
	}
}
func (h *uploadsHTTPFixture) create(path, key string, size int64, credential string, headers http.Header) *httptest.ResponseRecorder {
	data, _ := json.Marshal(map[string]any{"path": path, "size": size, "mediaType": "application/octet-stream"})
	headers = headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Idempotency-Key", key)
	headers.Set("Content-Type", "application/json")
	return h.request("POST", "/api/files/uploads", credential, data, headers)
}
func (h *uploadsHTTPFixture) newUpload(t *testing.T, path, key string, size int64) string {
	t.Helper()
	response := h.create(path, key, size, h.bearer, nil)
	value := uploadsHTTPJSON(t, response, 201)
	id, ok := value["id"].(string)
	if !ok || id == "" || response.Header().Get("Location") != "/api/files/uploads/"+id {
		t.Fatalf("upload identity %v", value)
	}
	if value["state"] != "open" || value["partSize"] != float64(filesUploadPartSize) || value["ordering"] != "any" {
		t.Fatalf("geometry %v", value)
	}
	for _, secret := range []string{"Handle", "NativeRef", "Checkpoint", h.root.SystemRoot} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("private upload state leaked: %s", response.Body.String())
		}
	}
	return id
}
func uploadRoute(id, suffix string) string { return "/api/files/uploads/" + id + suffix }
func (h *uploadsHTTPFixture) part(id string, n int, data []byte, headers http.Header) *httptest.ResponseRecorder {
	return h.request("PUT", uploadRoute(id, "/parts/"+strconv.Itoa(n)), h.bearer, data, headers)
}
func (h *uploadsHTTPFixture) poll(t *testing.T, route string) map[string]any {
	return h.pollAs(t, route, h.bearer)
}
func (h *uploadsHTTPFixture) pollAs(t *testing.T, route, credential string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		op := uploadsHTTPJSON(t, h.request("GET", route, credential, nil, nil), 200)
		switch op["state"] {
		case "succeeded", "failed", "cancelled", "outcome_unknown":
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation did not settle: %v", op)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
func (h *uploadsHTTPFixture) commit(t *testing.T, id string) map[string]any {
	t.Helper()
	response := h.request("POST", uploadRoute(id, "/commit"), h.bearer, nil, nil)
	// Commit settles inside the request when it can (200 + file); a 202 means another owner
	// or a full pool took it, and the durable Operation is the answer.
	if response.Code == http.StatusAccepted {
		location := response.Header().Get("Location")
		if !strings.HasPrefix(location, "/api/files/operations/") || response.Header().Get("Retry-After") == "" {
			t.Fatalf("missing async operation headers: %v", response.Header())
		}
		op := h.poll(t, location)
		if op["state"] != "succeeded" {
			t.Fatalf("upload publication failed: %v", op)
		}
	} else if response.Code != http.StatusOK {
		t.Fatalf("commit HTTP%d %s", response.Code, response.Body.String())
	}
	return uploadsHTTPJSON(t, h.request("POST", uploadRoute(id, "/commit"), h.bearer, nil, nil), 200)
}
func (h *uploadsHTTPFixture) waitCleaned(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		op, err := h.f.journal.Get(h.f.ctx, h.root, workerActor(h.root), id)
		if err != nil {
			t.Fatal(err)
		}
		if op.CleanedAt != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("temporary bytes not cleaned: %+v", op)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFilesUploadsPostgresLocalConformance(t *testing.T) {
	f := newFileWorkerFixture(t)
	f.cfg.FileCookieOrigins = []string{"https://files.example"}
	t.Run("out of order receipts lost ACK watermark restart and commit", func(t *testing.T) {
		h := newUploadsHTTPFixture(t, f)
		data := bytes.Repeat([]byte("a"), int(2*filesUploadPartSize+17))
		copy(data[len(data)-17:], []byte("binary-tail\x00\xff"))
		id := h.newUpload(t, "/large.bin", "large-create", int64(len(data)))
		uploadsHTTPProblem(t, h.request("POST", uploadRoute(id, "/commit"), h.bearer, nil, nil), 409, "upload_incomplete")
		first := uploadsHTTPJSON(t, h.part(id, 2, data[filesUploadPartSize:2*filesUploadPartSize], nil), 201)
		// Discarding the first response models a lost ACK; the next identical request
		// must return the durable slot receipt instead of creating more progress.
		again := uploadsHTTPJSON(t, h.part(id, 2, data[filesUploadPartSize:2*filesUploadPartSize], nil), 200)
		one, _ := json.Marshal(first)
		two, _ := json.Marshal(again)
		if !bytes.Equal(one, two) {
			t.Fatal("lost ACK replay changed receipt")
		}
		uploadsHTTPJSON(t, h.part(id, 1, data[:filesUploadPartSize], nil), 201)
		page := uploadsHTTPJSON(t, h.request("GET", uploadRoute(id, "/parts?limit=1"), h.bearer, nil, nil), 200)
		if page["watermark"] != float64(2) || page["items"].([]any)[0].(map[string]any)["number"] != float64(2) {
			t.Fatalf("ACK ordering %v", page)
		}
		cursor := page["nextCursor"].(string)
		tailHash := sha256.Sum256(data[2*filesUploadPartSize:])
		uploadsHTTPJSON(t, h.part(id, 3, data[2*filesUploadPartSize:], http.Header{"Content-Digest": {"sha-256=:" + base64.StdEncoding.EncodeToString(tailHash[:]) + ":"}}), 201)
		next := uploadsHTTPJSON(t, h.request("GET", uploadRoute(id, "/parts?limit=1&cursor="+url.QueryEscape(cursor)), h.bearer, nil, nil), 200)
		if next["watermark"] != page["watermark"] || next["items"].([]any)[0].(map[string]any)["number"] != float64(1) || next["nextCursor"] != nil {
			t.Fatalf("receipt pages shifted with later ACK: %v", next)
		}
		h.restart(t)
		status := uploadsHTTPJSON(t, h.request("GET", uploadRoute(id, ""), h.bearer, nil, nil), 200)
		if status["receivedParts"] != float64(3) || status["receivedBytes"] != float64(len(data)) {
			t.Fatalf("restart lost durable receipts %v", status)
		}
		metadata := h.commit(t, id)
		if metadata["path"] != "/large.bin" || metadata["size"] != float64(len(data)) || metadata["contentRevision"] == nil {
			t.Fatal(metadata)
		}
		got, err := os.ReadFile(filepath.Join(h.root.ContentRoot, "large.bin"))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("native file differs after restart: %v", err)
		}
		status = uploadsHTTPJSON(t, h.request("GET", uploadRoute(id, ""), h.bearer, nil, nil), 200)
		settled, _ := json.Marshal(status["result"])
		expected, _ := json.Marshal(metadata)
		if status["state"] != "committed" || !bytes.Equal(settled, expected) {
			t.Fatalf("settled session differs from repeated commit %v", status)
		}
		h.waitCleaned(t, id)
		f.checkNoLiability(t, h.root)
		uploadsHTTPProblem(t, h.request("DELETE", uploadRoute(id, ""), h.bearer, nil, nil), 409, "already_committed")
		// Creation replay must still succeed after the destination now exists.
		replay := uploadsHTTPJSON(t, h.create("/large.bin", "large-create", int64(len(data)), h.bearer, nil), 200)
		if replay["id"] != id {
			t.Fatal("creation replay made another upload")
		}
	})
	t.Run("same slot concurrency conflicts integrity and independent targets", func(t *testing.T) {
		h := newUploadsHTTPFixture(t, f)
		var createWG sync.WaitGroup
		createStart := make(chan struct{})
		created := make(chan *httptest.ResponseRecorder, 2)
		for range 2 {
			createWG.Add(1)
			go func() {
				defer createWG.Done()
				<-createStart
				created <- h.create("/concurrent", "concurrent", 4, h.bearer, nil)
			}()
		}
		close(createStart)
		createWG.Wait()
		close(created)
		id := ""
		for response := range created {
			if response.Code != 200 && response.Code != 201 {
				t.Fatalf("concurrent creation HTTP%d %s", response.Code, response.Body.String())
			}
			value := uploadsHTTPJSON(t, response, response.Code)
			if id != "" && id != value["id"] {
				t.Fatal("same client key admitted multiple operations")
			}
			id = value["id"].(string)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		responses := make(chan *httptest.ResponseRecorder, 2)
		for range 2 {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; responses <- h.part(id, 1, []byte("same"), nil) }()
		}
		close(start)
		wg.Wait()
		close(responses)
		codes := map[int]int{}
		for response := range responses {
			codes[response.Code]++
		}
		if codes[201] != 1 || codes[200] != 1 {
			t.Fatalf("concurrent identical slot ACKs %v", codes)
		}
		uploadsHTTPProblem(t, h.part(id, 1, []byte("else"), nil), 409, "part_conflict")
		// A second session for the same path does not wait for the first: it replaces it,
		// and the first one's next request says which session did.
		older := h.newUpload(t, "/same-path", "same-path-older", 4)
		newer := h.newUpload(t, "/same-path", "same-path-newer", 4)
		late := h.part(older, 1, []byte("same"), nil)
		uploadsHTTPProblem(t, late, 410, "upload_aborted")
		if !strings.Contains(late.Body.String(), newer) {
			t.Fatalf("superseded session does not name its successor: %s", late.Body.String())
		}
		gone := h.request("POST", uploadRoute(older, "/commit"), h.bearer, nil, nil)
		uploadsHTTPProblem(t, gone, 410, "upload_gone")
		if !strings.Contains(gone.Body.String(), newer) {
			t.Fatalf("commit of a superseded session does not name its successor: %s", gone.Body.String())
		}
		uploadsHTTPJSON(t, h.part(newer, 1, []byte("kept"), nil), 201)
		h.commit(t, newer)
		if data, err := os.ReadFile(filepath.Join(h.root.ContentRoot, "same-path")); err != nil || string(data) != "kept" {
			t.Fatalf("the newer session did not publish: %q %v", data, err)
		}
		other := h.newUpload(t, "/other-target", "independent-other", 3)
		wrong := sha256.Sum256([]byte("wrong"))
		uploadsHTTPProblem(t, h.part(other, 1, []byte("abc"), http.Header{"Content-Digest": {"sha-256=:" + base64.StdEncoding.EncodeToString(wrong[:]) + ":"}}), 422, "checksum_mismatch")
		status := uploadsHTTPJSON(t, h.request("GET", uploadRoute(other, ""), h.bearer, nil, nil), 200)
		if status["receivedParts"] != float64(0) {
			t.Fatal("checksum failure acquired receipt")
		}
		uploadsHTTPProblem(t, h.part(other, 1, []byte("ab"), nil), 400, "length_mismatch")
		uploadsHTTPJSON(t, h.part(other, 1, []byte("abc"), nil), 201)
		conflictingID := h.newUpload(t, "/conflicting-slot", "conflicting-slot", 4)
		conflicting := make(chan *httptest.ResponseRecorder, 2)
		var conflictWG sync.WaitGroup
		conflictStart := make(chan struct{})
		for _, body := range [][]byte{[]byte("aaaa"), []byte("bbbb")} {
			conflictWG.Add(1)
			go func(data []byte) {
				defer conflictWG.Done()
				<-conflictStart
				conflicting <- h.part(conflictingID, 1, data, nil)
			}(body)
		}
		close(conflictStart)
		conflictWG.Wait()
		close(conflicting)
		conflictCodes := map[int]int{}
		for response := range conflicting {
			conflictCodes[response.Code]++
		}
		if conflictCodes[201] != 1 || conflictCodes[409] != 1 {
			t.Fatalf("concurrent conflicting payloads acquired multiple slots: %v", conflictCodes)
		}
		h.commit(t, id)
		h.commit(t, other)
		h.commit(t, conflictingID)
	})
	t.Run("zero bytes idempotency and cookie or scoped bearer isolation", func(t *testing.T) {
		h := newUploadsHTTPFixture(t, f)
		csrf := uploadsHTTPJSON(t, h.request("GET", "/api/files/me", "cookie", nil, nil), 200)["csrfToken"].(string)
		uploadsHTTPProblem(t, h.create("/cookie", "cookie", 0, "cookie", nil), 403, "forbidden")
		csrfHeaders := http.Header{"Origin": {"https://files.example"}, "X-CSRF-Token": {csrf}}
		cookieUpload := uploadsHTTPJSON(t, h.create("/cookie", "cookie", 0, "cookie", csrfHeaders), 201)["id"].(string)
		uploadsHTTPProblem(t, h.request("POST", uploadRoute(cookieUpload, "/commit"), "cookie", nil, nil), 403, "forbidden")
		committed := h.request("POST", uploadRoute(cookieUpload, "/commit"), "cookie", nil, csrfHeaders)
		if committed.Code == http.StatusAccepted {
			// Owner management can observe the owner-created session through another client.
			h.pollAs(t, committed.Header().Get("Location"), "cookie")
		} else {
			uploadsHTTPJSON(t, committed, 200)
		}
		zero := h.newUpload(t, "/zero", "zero", 0)
		h.commit(t, zero)
		data, err := os.ReadFile(filepath.Join(h.root.ContentRoot, "zero"))
		if err != nil || len(data) != 0 {
			t.Fatalf("zero native file: %v", err)
		}
		uploadsHTTPProblem(t, h.create("/changed", "zero", 1, h.bearer, nil), 409, "idempotency_conflict")
		if err = os.Mkdir(filepath.Join(h.root.ContentRoot, "Scope"), 0700); err != nil {
			t.Fatal(err)
		}
		scoped, _ := f.token(t, h.root.OwnerID, "Scope", true, true)
		scopedUpload := uploadsHTTPJSON(t, h.create("/Scope/file", "scoped", 1, scoped, nil), 201)["id"].(string)
		uploadsHTTPProblem(t, h.create("/Scope-old/file", "scope-sibling", 1, scoped, nil), 404, "not_found")
		unrelated, _ := f.token(t, h.root.OwnerID, "", true, true)
		uploadsHTTPProblem(t, h.request("GET", uploadRoute(scopedUpload, ""), unrelated, nil, nil), 403, "forbidden")
		uploadsHTTPJSON(t, h.request("GET", uploadRoute(scopedUpload, ""), "cookie", nil, nil), 200)
		otherRoot, _ := f.owner(t)
		otherToken, _ := f.token(t, otherRoot.OwnerID, "", true, true)
		uploadsHTTPProblem(t, h.request("GET", uploadRoute(scopedUpload, ""), otherToken, nil, nil), 404, "not_found")
		readOnly, _ := f.token(t, h.root.OwnerID, "", true, false)
		uploadsHTTPProblem(t, h.create("/read-only", "read-only", 0, readOnly, nil), 403, "forbidden")
		secondScoped := uploadsHTTPJSON(t, h.create("/Scope/second", "scoped-second", 1, scoped, nil), 201)["id"].(string)
		list := uploadsHTTPJSON(t, h.request("GET", "/api/files/uploads?limit=1", scoped, nil, nil), 200)
		if len(list["items"].([]any)) != 1 || list["nextCursor"] == nil {
			t.Fatalf("session list principal/scope isolation %v", list)
		}
		cursor := list["nextCursor"].(string)
		next := uploadsHTTPJSON(t, h.request("GET", "/api/files/uploads?limit=1&cursor="+url.QueryEscape(cursor), scoped, nil, nil), 200)
		seen := map[string]bool{list["items"].([]any)[0].(map[string]any)["id"].(string): true, next["items"].([]any)[0].(map[string]any)["id"].(string): true}
		if len(seen) != 2 || !seen[scopedUpload] || !seen[secondScoped] || next["nextCursor"] != nil {
			t.Fatalf("session keyset pages diverge: %v / %v", list, next)
		}
		uploadsHTTPProblem(t, h.request("GET", "/api/files/uploads?limit=1&cursor="+url.QueryEscape(cursor), unrelated, nil, nil), 400, "invalid_request")
	})
	t.Run("abort fences late body and expiry retains tombstones", func(t *testing.T) {
		h := newUploadsHTTPFixture(t, f)
		id := h.newUpload(t, "/late", "late", 4)
		entered, release := make(chan struct{}), make(chan struct{})
		body := &uploadsBlockedReader{entered: entered, release: release, data: bytes.NewReader([]byte("late"))}
		req := httptest.NewRequest("PUT", "https://files.example"+uploadRoute(id, "/parts/1"), body).WithContext(f.ctx)
		req.ContentLength = 4
		req.Header.Set("Authorization", "Bearer "+h.bearer)
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { w := httptest.NewRecorder(); h.handler.ServeHTTP(w, req); done <- w }()
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("part body never started")
		}
		aborted := h.request("DELETE", uploadRoute(id, ""), h.bearer, nil, nil)
		if aborted.Code != 204 {
			close(release)
			t.Fatalf("abort HTTP%d %s", aborted.Code, aborted.Body.String())
		}
		close(release)
		select {
		case response := <-done:
			if response.Code != 410 {
				t.Fatalf("late body acquired an ACK HTTP%d %s", response.Code, response.Body.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("late body did not return")
		}
		status := uploadsHTTPJSON(t, h.request("GET", uploadRoute(id, ""), h.bearer, nil, nil), 200)
		if status["state"] != "aborted" || status["receivedParts"] != float64(0) {
			t.Fatalf("late receipt after abort %v", status)
		}
		if response := h.request("DELETE", uploadRoute(id, ""), h.bearer, nil, nil); response.Code != 204 {
			t.Fatalf("abort replay HTTP%d %s", response.Code, response.Body.String())
		}
		h.waitCleaned(t, id)
		f.checkNoLiability(t, h.root)
		expired := h.newUpload(t, "/expired", "expiry", 1)
		if _, err := f.db.Pool().Exec(f.ctx, `update file_uploads set expires_at=clock_timestamp()-interval '1 second' where operation_id=$1`, expired); err != nil {
			t.Fatal(err)
		}
		status = uploadsHTTPJSON(t, h.request("GET", uploadRoute(expired, ""), h.bearer, nil, nil), 200)
		if status["state"] != "expired" {
			t.Fatalf("status stayed open past hard deadline: %v", status)
		}
		terminal, err := f.journal.ExpireUploads(f.ctx, 1)
		if err != nil || len(terminal) != 0 {
			t.Fatalf("status expiry was not durable: %v %v", terminal, err)
		}
		uploadsHTTPProblem(t, h.part(expired, 1, []byte("x"), nil), 410, "upload_expired")
		status = uploadsHTTPJSON(t, h.request("GET", uploadRoute(expired, ""), h.bearer, nil, nil), 200)
		if status["state"] != "expired" {
			t.Fatal(status)
		}
		if _, err = os.Stat(filepath.Join(h.root.ContentRoot, "expired")); !os.IsNotExist(err) {
			t.Fatal("expired upload became visible")
		}
	})
	t.Run("quota admission is atomic and parent ABA cannot retarget", func(t *testing.T) {
		h := newUploadsHTTPFixture(t, f)
		if _, err := f.db.Pool().Exec(f.ctx, `update file_budgets set logical_limit=3 where owner_id=$1`, h.root.OwnerID); err != nil {
			t.Fatal(err)
		}
		uploadsHTTPProblem(t, h.create("/too-large-for-quota", "quota", 4, h.bearer, nil), 507, "quota_exceeded")
		var count int
		if err := f.db.Pool().QueryRow(f.ctx, `select count(*) from file_uploads where owner_id=$1`, h.root.OwnerID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rejected admission persisted: %d %v", count, err)
		}
		if _, err := f.db.Pool().Exec(f.ctx, `update file_budgets set logical_limit=100 where owner_id=$1`, h.root.OwnerID); err != nil {
			t.Fatal(err)
		}
		// The folder is created through the service, so the index holds it and the session
		// binds its identity. What follows changes the native folder behind the index's
		// back, which only the native revision captured at admission can notice.
		parent := filepath.Join(h.root.ContentRoot, "Parent")
		uploadsHTTPJSON(t, h.request("POST", "/api/files", h.bearer, []byte(`{"kind":"folder","path":"/Parent"}`), http.Header{"Content-Type": {"application/json"}}), 201)
		id := h.newUpload(t, "/Parent/file", "parent-aba", 4)
		uploadsHTTPJSON(t, h.part(id, 1, []byte("safe"), nil), 201)
		if err := os.Rename(parent, parent+"-moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(parent, 0700); err != nil {
			t.Fatal(err)
		}
		response := h.request("POST", uploadRoute(id, "/commit"), h.bearer, nil, nil)
		// A settlement that fits in the request reports its failure there; one that is still
		// being settled elsewhere reports it through the Operation.
		var outcome map[string]any
		switch {
		case response.Code == http.StatusAccepted:
			outcome = h.poll(t, response.Header().Get("Location"))
		case response.Code >= 400:
			outcome = uploadsHTTPJSON(t, h.request("GET", "/api/files/operations/"+id, h.bearer, nil, nil), 200)
		default:
			t.Fatalf("changed parent accepted HTTP%d %s", response.Code, response.Body.String())
		}
		if outcome["state"] != "failed" && outcome["state"] != "cancelled" {
			t.Fatalf("changed parent not rejected %v", outcome)
		}
		for _, base := range []string{parent, parent + "-moved"} {
			if _, err := os.Stat(filepath.Join(base, "file")); !os.IsNotExist(err) {
				t.Fatalf("retargeted file under %s: %v", base, err)
			}
		}
		h.waitCleaned(t, id)
		f.checkNoLiability(t, h.root)
	})
}

// An open session fences nothing, so everything below is admitted while one is pending;
// what the session bound is checked when it commits.
func TestFilesUploadsPostgresLocalCommitTimeFence(t *testing.T) {
	f := newFileWorkerFixture(t)
	f.cfg.FileCookieOrigins = []string{"https://files.example"}
	h := newUploadsHTTPFixture(t, f)
	jsonHeader := http.Header{"Content-Type": {"application/json"}}
	mkdir := func(path string) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"kind": "folder", "path": path})
		return uploadsHTTPJSON(t, h.request("POST", "/api/files", h.bearer, body, jsonHeader), 201)
	}
	pending := func(path, key string) string {
		t.Helper()
		id := h.newUpload(t, path, key, 4)
		uploadsHTTPJSON(t, h.part(id, 1, []byte("data"), nil), 201)
		return id
	}
	remove := func(path string) {
		t.Helper()
		w := h.request("DELETE", "/api/files?"+url.Values{"path": {path}, "recursive": {"true"}}.Encode(), h.bearer, nil, nil)
		switch w.Code {
		case http.StatusNoContent:
		case http.StatusAccepted:
			if op := h.poll(t, w.Header().Get("Location")); op["state"] != "succeeded" {
				t.Fatalf("delete of %s: %v", path, op)
			}
		default:
			t.Fatalf("delete of %s above an open upload: HTTP %d %s", path, w.Code, w.Body.String())
		}
	}
	// refused commits the session and requires the commit to end it with this answer,
	// before anything was published.
	refused := func(id string, status int, code, reason string) {
		t.Helper()
		w := h.request("POST", uploadRoute(id, "/commit"), h.bearer, nil, nil)
		uploadsHTTPProblem(t, w, status, code)
		if !strings.Contains(w.Body.String(), reason) {
			t.Fatalf("refusal does not say why (%q): %s", reason, w.Body.String())
		}
		if session := uploadsHTTPJSON(t, h.request("GET", uploadRoute(id, ""), h.bearer, nil, nil), 200); session["state"] != "failed" {
			t.Fatalf("a session that can never commit is %v", session["state"])
		}
		if op := uploadsHTTPJSON(t, h.request("GET", "/api/files/operations/"+id, h.bearer, nil, nil), 200); op["state"] != "failed" {
			t.Fatalf("operation after a refused commit: %v", op)
		}
		// The answer is the session's, not this request's: asking again explains it again.
		again := h.request("POST", uploadRoute(id, "/commit"), h.bearer, nil, nil)
		uploadsHTTPProblem(t, again, 410, "upload_gone")
		if !strings.Contains(again.Body.String(), reason) {
			t.Fatalf("repeated commit lost the reason (%q): %s", reason, again.Body.String())
		}
		h.waitCleaned(t, id)
	}
	absent := func(relative string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(h.root.ContentRoot, relative)); !os.IsNotExist(err) {
			t.Fatalf("%s exists natively: %v", relative, err)
		}
	}

	t.Run("a deleted folder is not resurrected by a session opened inside it", func(t *testing.T) {
		mkdir("/Gone")
		id := pending("/Gone/file", "fence-gone")
		remove("/Gone")
		refused(id, 409, "parent_missing", `no folder is indexed at \"/Gone\"`)
		absent("Gone")
	})
	t.Run("a folder created again is not the folder the session was opened for", func(t *testing.T) {
		mkdir("/Again")
		id := pending("/Again/file", "fence-again")
		remove("/Again")
		mkdir("/Again")
		refused(id, 412, "target_changed", "deleted and created again")
		absent("Again/file")
	})
	t.Run("a folder may be renamed while a session is open inside it", func(t *testing.T) {
		folder := mkdir("/Before")
		id := pending("/Before/file", "fence-moved")
		body, _ := json.Marshal(map[string]any{"from": "/Before", "to": "/After", "ifRevision": folder["revision"]})
		uploadsHTTPJSON(t, h.request("POST", "/api/files/move", h.bearer, body, jsonHeader), 200)
		refused(id, 409, "parent_missing", `no folder is indexed at \"/Before\"`)
		absent("Before")
		absent("After/file")
	})
	t.Run("a file with a pending replacement stays readable by ID", func(t *testing.T) {
		created := uploadsHTTPJSON(t, h.request("PUT", "/api/files/content?"+url.Values{"path": {"/doc.txt"}}.Encode(), h.bearer, []byte("old"), http.Header{"If-None-Match": {"*"}}), 201)
		body, _ := json.Marshal(map[string]any{"target": map[string]any{"id": created["id"]}, "size": 3, "mediaType": "text/plain",
			"condition": map[string]any{"mode": "replace", "contentRevision": created["contentRevision"]}})
		headers := jsonHeader.Clone()
		headers.Set("Idempotency-Key", "fence-replace")
		id := uploadsHTTPJSON(t, h.request("POST", "/api/files/uploads", h.bearer, body, headers), 201)["id"].(string)
		read := h.request("GET", "/api/files/content?"+url.Values{"id": {created["id"].(string)}}.Encode(), h.bearer, nil, nil)
		if read.Code != 200 || read.Body.String() != "old" {
			t.Fatalf("read by ID under an open replacement: HTTP %d %s", read.Code, read.Body.String())
		}
		if w := h.request("DELETE", uploadRoute(id, ""), h.bearer, nil, nil); w.Code != 204 {
			t.Fatalf("abort: HTTP %d %s", w.Code, w.Body.String())
		}
		h.waitCleaned(t, id)
	})
	f.checkNoLiability(t, h.root)
}

type uploadsBlockedReader struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	data    io.Reader
}

func (r *uploadsBlockedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered); <-r.release })
	return r.data.Read(p)
}

func TestFilesUploadsPostgresObjectPublicationLimitAndUnknown(t *testing.T) {
	f := newObjectsAPIFixture(t)
	if _, err := f.db.Pool().Exec(f.ctx, `update file_budgets set logical_limit=$1,physical_limit=$2`, int64(1<<30), int64(2<<30)); err != nil {
		t.Fatal(err)
	}
	create := func(path, key string, size int64) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"path": path, "size": size})
		return f.request(t, "POST", "/uploads", "", body, http.Header{"Idempotency-Key": {key}, "Content-Type": {"application/json"}})
	}
	uploadsHTTPProblem(t, create("/beyond-native-limit", "limit", filesUploadPartSize*filesUploadMaxParts+1), 413, "file_too_large")
	usage := func() fileops.Usage {
		t.Helper()
		u, err := fileops.NewPostgres(f.db.Pool()).Usage(f.ctx, f.roots[0], workerActor(f.roots[0]))
		if err != nil {
			t.Fatal(err)
		}
		return u
	}

	// A lost final reply is not by itself uncertainty. This provider applied the
	// completion and then dropped the connection; the server reconciled that against
	// the destination, whose intent metadata and size are this session's own, so the
	// commit settles in the request and answers 200 with the file. The durable
	// Operation reports the same settled publication, which is what makes the answer
	// evidence rather than a guess.
	observedID := uploadsHTTPJSON(t, create("/observed-upload", "observed", 4), 201)["id"].(string)
	uploadsHTTPJSON(t, f.request(t, "PUT", "/uploads/"+observedID+"/parts/1", "", []byte("data"), nil), 201)
	observedKey := "drive/" + f.owners[0] + "/observed-upload"
	f.provider.mu.Lock()
	f.provider.lostReplyKey = observedKey
	f.provider.mu.Unlock()
	committed := uploadsHTTPJSON(t, f.request(t, "POST", "/uploads/"+observedID+"/commit", "", nil, nil), 200)
	if committed["path"] != "/observed-upload" || committed["size"] != float64(4) {
		t.Fatalf("lost reply was not reconciled to the published file: %v", committed)
	}
	settled := uploadsHTTPJSON(t, f.request(t, "GET", "/operations/"+observedID, "", nil, nil), 200)
	if settled["state"] != "succeeded" || settled["result"] == nil {
		t.Fatalf("in-request settlement left an unsettled operation: %v", settled)
	}
	if session := uploadsHTTPJSON(t, f.request(t, "GET", "/uploads/"+observedID, "", nil, nil), 200); session["state"] != "committed" {
		t.Fatalf("session did not settle with the file: %v", session)
	}
	f.provider.mu.Lock()
	observedPuts, observedCompletes := f.provider.puts[observedKey], f.provider.completeRequests[observedKey]
	observedNative, observedExists := f.provider.current(observedKey)
	f.provider.mu.Unlock()
	if !observedExists || !bytes.Equal(observedNative.Body, []byte("data")) || observedPuts != 0 || observedCompletes != 1 {
		t.Fatalf("observed reply was replayed or lost: puts=%d completes=%d exists=%v", observedPuts, observedCompletes, observedExists)
	}
	if u := usage(); u.LogicalReserved != 0 || u.PhysicalReserved != 0 || u.LogicalBytes != 4 {
		t.Fatalf("settled publication kept a reservation: %+v", u)
	}

	// Uncertainty is the case where the destination cannot be observed after the lost
	// reply: the completion applied, but every read of the key fails. The commit cannot
	// settle, so the in-request answer is the durable Operation (202 + Location), and
	// the same uncertainty is visible on the operation and on the session. Nothing may
	// replay the native effect, and the liability must stay reserved until it is
	// resolved. A proxy in front of the fixture's provider drops destination reads after
	// the completion, while the provider itself keeps the writer it already applied.
	var proxiedOwner string
	if err := f.db.Pool().QueryRow(f.ctx, `insert into users(email,password_hash) values('proxied@example.test','') returning id::text`).Scan(&proxiedOwner); err != nil {
		t.Fatal(err)
	}
	key := "drive/" + proxiedOwner + "/unknown-upload"
	var unobservable atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := strings.TrimPrefix(r.URL.Path, "/bucket/")
		if r.Method == http.MethodPost && r.URL.Query().Get("uploadId") != "" && target == key {
			unobservable.Store(true)
		}
		if unobservable.Load() && (r.Method == http.MethodHead || r.Method == http.MethodGet) && target == key {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		f.provider.serve(t, w, r)
	}))
	t.Cleanup(proxy.Close)
	evidence, err := os.ReadFile(f.cfg.Storage.ObjectAdmissionFile)
	if err != nil {
		t.Fatal(err)
	}
	var admission map[string]any
	if err = json.Unmarshal(evidence, &admission); err != nil {
		t.Fatal(err)
	}
	admission["endpoint"] = proxy.URL
	if evidence, err = json.Marshal(admission); err != nil {
		t.Fatal(err)
	}
	evidencePath := filepath.Join(t.TempDir(), "admission.json")
	if err = os.WriteFile(evidencePath, evidence, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := *f.cfg
	cfg.Storage.S3.Endpoint = proxy.URL
	cfg.Storage.ObjectAdmissionFile = evidencePath
	proxied, err := storagebackend.New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A storage binding includes the endpoint digest, so the proxied connection is a
	// distinct root. One budget row exists per owner, so use a fresh owner whose empty
	// native prefix makes the admission evidence true instead of rebinding an existing
	// root.
	driver, err := proxied.Open(f.ctx, proxiedOwner)
	if err != nil {
		t.Fatal(err)
	}
	proxiedNative, ok := driver.(fileops.NativeActions)
	if !ok {
		driver.Close()
		t.Fatal("proxied object tree omitted native actions")
	}
	binding := proxiedNative.StorageBinding()
	rules := proxiedNative.StorageNameRules()
	retains := proxiedNative.Describe().RetainsVersions
	driver.Close()
	proxiedRoot := fileops.Root{OwnerID: binding.OwnerID, ConnectionID: binding.ConnectionID, Generation: binding.Generation, ContentRoot: binding.ContentRoot, SystemRoot: binding.SystemRoot, CaseSensitive: rules.CaseSensitive, RetainsVersions: retains}
	proxiedJournal := fileops.NewPostgres(f.db.Pool())
	if err = proxiedJournal.Initialize(f.ctx, proxiedRoot, fileops.Baseline{LogicalLimit: 1 << 30, PhysicalLimit: 2 << 30, NativeComplete: true, IndexCompared: true, LegacyWritersDrained: true, Evidence: json.RawMessage(`{"fixture":"fresh proxied owner prefix, no native objects and no index rows"}`)}); err != nil {
		t.Fatal(err)
	}
	proxiedToken, _, err := auth.New(f.db, f.cfg).CreateToken(f.ctx, auth.NewTokenInput{UserID: proxiedOwner, Name: "proxied observation", Kind: auth.KindApp, CanRead: true, CanWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, target string, body []byte, headers http.Header) *httptest.ResponseRecorder {
		headers = headers.Clone()
		if headers == nil {
			headers = make(http.Header)
		}
		headers.Set("Authorization", "Bearer "+proxiedToken)
		return f.request(t, method, target, "", body, headers)
	}
	createProxied := func(path, idempotencyKey string, size int64) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"path": path, "size": size})
		return request("POST", "/uploads", body, http.Header{"Idempotency-Key": {idempotencyKey}, "Content-Type": {"application/json"}})
	}
	t.Cleanup(func() {
		// The fixture's own worker wait is registered later and therefore runs after
		// this cleanup, so retire the retries that the last polls may have queued
		// before closing the provider they would use.
		deadline := time.Now().Add(5 * time.Second)
		for {
			workers := f.server.workers()
			workers.mu.Lock()
			active := len(workers.active)
			workers.mu.Unlock()
			if active == 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		_ = proxied.Close()
	})
	f.server.SetStorageFactory(proxied)

	id := uploadsHTTPJSON(t, createProxied("/unknown-upload", "unknown", 4), 201)["id"].(string)
	uploadsHTTPJSON(t, request("PUT", "/uploads/"+id+"/parts/1", []byte("data"), nil), 201)
	f.provider.mu.Lock()
	f.provider.lostReplyKey = key
	f.provider.mu.Unlock()
	recorder := request("POST", "/uploads/"+id+"/commit", nil, nil)
	response := uploadsHTTPJSON(t, recorder, 202)
	if response["state"] != "outcome_unknown" || response["id"] != id {
		t.Fatalf("unresolved commit did not return its durable operation: %v", response)
	}
	if location := recorder.Header().Get("Location"); location != "/api/files/operations/"+id {
		t.Fatalf("unresolved commit Location = %q", location)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("unresolved commit has no Retry-After")
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		op := uploadsHTTPJSON(t, request("GET", "/operations/"+id, nil, nil), 200)
		switch op["state"] {
		case "outcome_unknown":
		case "succeeded", "failed", "cancelled":
			t.Fatalf("unobservable publication settled as %v: %v", op["state"], op)
		default:
			if time.Now().After(deadline) {
				t.Fatalf("lost native reply did not retain uncertainty: %v", op)
			}
			time.Sleep(2 * time.Millisecond)
			continue
		}
		break
	}
	status := uploadsHTTPJSON(t, request("GET", "/uploads/"+id, nil, nil), 200)
	if status["state"] != "outcome_unknown" {
		t.Fatalf("session hid unresolved publication %v", status)
	}
	uploadsHTTPProblem(t, request("DELETE", "/uploads/"+id, nil, nil), 409, "commit_in_progress")
	f.provider.mu.Lock()
	puts := f.provider.puts[key]
	completes := f.provider.completeRequests[key]
	proxiedNativeObject, exists := f.provider.current(key)
	f.provider.mu.Unlock()
	if !exists || !bytes.Equal(proxiedNativeObject.Body, []byte("data")) || puts != 0 || completes != 1 {
		t.Fatalf("unexpected native attempts puts=%d completes=%d exists=%v", puts, completes, exists)
	}
	if u, e := proxiedJournal.Usage(f.ctx, proxiedRoot, workerActor(proxiedRoot)); e != nil {
		t.Fatal(e)
	} else if u.LogicalReserved == 0 || u.PhysicalReserved == 0 {
		t.Fatalf("unknown effect released liability %+v", u)
	}
}

// A page of uploads hands each item to the single create handler as the bytes the client
// sent, never as a re-encoding of the decoded request. Re-encoding wrote the absent
// destination and target aliases as null, and the single handler refuses a path that also
// carries a null alias: every item answered invalid_selector while the page still answered
// 200, which is how a 23,373-file import showed "0 / 23,373" with no session row, no
// failed request of its own, and nothing in the log but a wall of 200s.
func TestFilesUploadBatchPageAsSent(t *testing.T) {
	f := newFileWorkerFixture(t)
	h := newUploadsHTTPFixture(t, f)
	body := []byte(`{"items":[` +
		`{"path":"/Pictures/trip/one.jpg","size":3,"mediaType":"image/jpeg","parents":true,"idempotencyKey":"page-one"},` +
		`{"path":"/Pictures/trip/two.jpg","size":4,"mediaType":"image/jpeg","parents":true,"idempotencyKey":"page-two"},` +
		`{"path":"/Pictures/trip/bad.jpg","target":{"id":"6f1c0d0e-0000-4000-8000-000000000000"},"size":5}]}`)
	value := uploadsHTTPJSON(t, h.request("POST", "/api/files/uploads/batch", h.bearer, body, http.Header{"Content-Type": {"application/json"}}), 200)
	items, _ := value["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("page items %v", value)
	}
	ids := map[string]bool{}
	for index := 0; index < 2; index++ {
		upload, _ := items[index].(map[string]any)["upload"].(map[string]any)
		id, _ := upload["id"].(string)
		if id == "" || upload["state"] != "open" || ids[id] {
			t.Fatalf("item %d was not admitted: %v", index, items[index])
		}
		if !strings.HasPrefix(upload["path"].(string), "/Pictures/trip/") {
			t.Fatalf("item %d bound the wrong target: %v", index, upload)
		}
		ids[id] = true
	}
	// One refused item leaves its neighbours admitted instead of failing the page, and the
	// refusal is the single handler's own Problem, not a batch-shaped substitute.
	if problem, _ := items[2].(map[string]any)["error"].(map[string]any); problem["code"] != "invalid_selector" {
		t.Fatalf("neighbour isolation %v", items[2])
	}
	// parents:true created the destination tree once; both sessions share the one folder.
	parent := uploadsHTTPJSON(t, h.request("GET", "/api/files/list?path=%2FPictures%2Ftrip", h.bearer, nil, nil), 200)
	if list, _ := parent["items"].([]any); len(list) != 0 {
		t.Fatalf("the upload page published entries before their bytes arrived: %v", list)
	}
}

// A commit page carries sixteen sessions. The page, not the transfer, is what gated an
// import: sixteen settle inside half of the worker pool, and a seventeenth is refused
// rather than silently truncated, so a client cannot believe it published a file it did not.
func TestFilesUploadCommitBatchPageLimit(t *testing.T) {
	f := newFileWorkerFixture(t)
	h := newUploadsHTTPFixture(t, f)
	page := func(count int) []byte {
		items := make([]string, count)
		for i := range items {
			items[i] = `{"id":"6f1c0d0e-0000-4000-8000-0000000000` + fmt.Sprintf("%02d", i) + `"}`
		}
		return []byte(`{"items":[` + strings.Join(items, ",") + `]}`)
	}
	value := uploadsHTTPJSON(t, h.request("POST", "/api/files/uploads/commit/batch", h.bearer, page(16), http.Header{"Content-Type": {"application/json"}}), 200)
	if items, _ := value["items"].([]any); len(items) != 16 {
		t.Fatalf("a full page answered %d items", len(items))
	}
	uploadsHTTPProblem(t, h.request("POST", "/api/files/uploads/commit/batch", h.bearer, page(17), http.Header{"Content-Type": {"application/json"}}), 400, "invalid_request")
}

// A commit answer names the folder it published into: the client's next rename, move or
// delete of that folder sends its revision as If-Revision, and the settle that published the
// file changed it. The value has to be the one a metadata read returns, present on a single
// commit, a repeated commit and a batch item, or the client keeps paying that read after
// every import for a value the settlement already had.
func TestFilesUploadCommitAnswersParentRevision(t *testing.T) {
	f := newFileWorkerFixture(t)
	h := newUploadsHTTPFixture(t, f)
	jsonHeader := http.Header{"Content-Type": {"application/json"}}
	parentRevision := func(path string) string {
		t.Helper()
		value := uploadsHTTPJSON(t, h.request("GET", "/api/files?path="+url.QueryEscape(path), h.bearer, nil, nil), 200)
		revision, _ := value["revision"].(string)
		if revision == "" {
			t.Fatalf("metadata %s names no revision: %v", path, value)
		}
		return revision
	}
	upload := func(path, key string) string {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"path": path, "size": 4, "mediaType": "text/plain", "parents": true})
		value := uploadsHTTPJSON(t, h.request("POST", "/api/files/uploads", h.bearer, body, http.Header{"Idempotency-Key": {key}, "Content-Type": {"application/json"}}), 201)
		id, _ := value["id"].(string)
		if id == "" {
			t.Fatalf("upload admission %v", value)
		}
		uploadsHTTPJSON(t, h.part(id, 1, []byte("data"), nil), 201)
		return id
	}
	first := h.commit(t, upload("/Trip/one.txt", "parent-revision-one"))
	if first["parentRevision"] != parentRevision("/Trip") {
		t.Fatalf("commit answered parentRevision %v, metadata answers %q", first["parentRevision"], parentRevision("/Trip"))
	}
	// A second settle into the same folder advances its membership, and the batch item that
	// reports it must carry the new value rather than the one the first settle answered.
	secondID := upload("/Trip/two.txt", "parent-revision-two")
	page := uploadsHTTPJSON(t, h.request("POST", "/api/files/uploads/commit/batch", h.bearer, []byte(`{"items":[{"id":"`+secondID+`"}]}`), jsonHeader), 200)
	items, _ := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("settle page %v", page)
	}
	settled, _ := items[0].(map[string]any)["result"].(map[string]any)
	after := parentRevision("/Trip")
	if settled["parentRevision"] != after || after == first["parentRevision"] {
		t.Fatalf("batch item answered parentRevision %v, metadata answers %q (before %v)", settled["parentRevision"], after, first["parentRevision"])
	}
	// The root is a folder like any other; its revision is synthetic but has to advance too.
	rootBefore := parentRevision("/")
	root := h.commit(t, upload("/top.txt", "parent-revision-root"))
	rootAfter := parentRevision("/")
	if root["parentRevision"] != rootAfter || rootAfter == rootBefore {
		t.Fatalf("commit at the root answered parentRevision %v, metadata answers %q (before %q)", root["parentRevision"], rootAfter, rootBefore)
	}
}

// A destination whose folders do not exist yet is admitted without creating them; the
// ancestors enter the index when the file lands, because only then does the tree carry the
// prefix they name. Materializing them at admission cost one provider-heavy mkdir per folder
// — six provider round trips at 240 ms — and paid it again on every import of the same tree,
// which is where an 11-year health export spent two hours before the first file appeared.
func TestFilesUploadsMaterializeParentsAtSettlement(t *testing.T) {
	f := newObjectsAPIFixture(t)
	if _, err := f.db.Pool().Exec(f.ctx, `update file_budgets set logical_limit=$1,physical_limit=$2`, int64(1<<30), int64(2<<30)); err != nil {
		t.Fatal(err)
	}
	create := func(path, key string, size int64, parents bool) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"path": path, "size": size, "mediaType": "application/json", "parents": parents})
		return f.request(t, "POST", "/uploads", "", body, http.Header{"Idempotency-Key": {key}, "Content-Type": {"application/json"}})
	}
	rows := func(prefix string) int {
		t.Helper()
		var count int
		if err := f.db.Pool().QueryRow(f.ctx, `select count(*) from file_entries where owner_id=$1::uuid and path like $2`, f.owners[0], prefix).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	// Without parents the missing ancestor is still refused: the opt-in is what changed.
	uploadsHTTPProblem(t, create("/Health/2015/09/26/step-count.json", "plain", 4, false), 409, "parent_missing")
	// With parents the session is admitted and nothing has been created yet.
	id := uploadsHTTPJSON(t, create("/Health/2015/09/26/step-count.json", "promised", 4, true), 201)["id"].(string)
	if count := rows("Health%"); count != 0 {
		t.Fatalf("admission created %d ancestor rows for a destination the tree does not have", count)
	}
	uploadsHTTPJSON(t, f.request(t, "PUT", "/uploads/"+id+"/parts/1", "", []byte("data"), nil), 201)
	response := f.request(t, "POST", "/uploads/"+id+"/commit", "", nil, nil)
	if response.Code != 200 && response.Code != 202 {
		op := uploadsHTTPJSON(t, f.request(t, "GET", "/operations/"+id, "", nil, nil), 200)
		status := uploadsHTTPJSON(t, f.request(t, "GET", "/uploads/"+id, "", nil, nil), 200)
		key := "drive/" + f.owners[0] + "/Health/2015/09/26/step-count.json"
		f.provider.mu.Lock()
		native, exists := f.provider.current(key)
		puts, completes := f.provider.puts[key], f.provider.completeRequests[key]
		f.provider.mu.Unlock()
		t.Fatalf("commit HTTP%d body=%s operation=%v status=%v native=%v exists=%v puts=%d completes=%d",
			response.Code, response.Body.String(), op, status, native, exists, puts, completes)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		status := uploadsHTTPJSON(t, f.request(t, "GET", "/uploads/"+id, "", nil, nil), 200)
		if status["state"] == "committed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("upload did not settle: %v", status)
		}
		time.Sleep(2 * time.Millisecond)
	}
	// The settlement wrote the ancestors parent first, so the chain is addressable.
	for _, folder := range []string{"Health", "Health/2015", "Health/2015/09", "Health/2015/09/26"} {
		if count := rows(folder); count != 1 {
			t.Fatalf("folder %q has %d rows after settlement", folder, count)
		}
	}
	listing := objectsAPIJSON(t, f.request(t, "GET", "/list", "/Health/2015/09/26", nil, nil), 200)
	entries, _ := listing["items"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["path"] != "/Health/2015/09/26/step-count.json" {
		t.Fatalf("listing %v", listing)
	}
	// A second file in the same, now-existing folder takes the ordinary path.
	second := uploadsHTTPJSON(t, create("/Health/2015/09/26/flights-climbed.json", "existing", 4, true), 201)["id"].(string)
	uploadsHTTPJSON(t, f.request(t, "PUT", "/uploads/"+second+"/parts/1", "", []byte("data"), nil), 201)
	if response := f.request(t, "POST", "/uploads/"+second+"/commit", "", nil, nil); response.Code != 200 && response.Code != 202 {
		t.Fatalf("second commit HTTP%d body=%s", response.Code, response.Body.String())
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		status := uploadsHTTPJSON(t, f.request(t, "GET", "/uploads/"+second, "", nil, nil), 200)
		if status["state"] == "committed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second upload did not settle: %v", status)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if count := rows("Health/2015/09/26/%"); count != 2 {
		t.Fatalf("folder rows after two files = %d", count)
	}
}

// An overwrite is the condition a re-import sends after the person answered "replace every
// file that already exists": it names no revision, so the client never reads the destination
// first, and the service binds the revision it observes itself.
func TestFilesUploadsOverwriteCondition(t *testing.T) {
	f := newFileWorkerFixture(t)
	h := newUploadsHTTPFixture(t, f)
	open := func(path, key string, condition map[string]any, size int) *httptest.ResponseRecorder {
		data, _ := json.Marshal(map[string]any{"path": path, "size": size, "mediaType": "text/plain", "condition": condition})
		headers := make(http.Header)
		headers.Set("Idempotency-Key", key)
		headers.Set("Content-Type", "application/json")
		return h.request("POST", "/api/files/uploads", h.bearer, data, headers)
	}
	overwrite := func(t *testing.T, path, key, body string) map[string]any {
		t.Helper()
		data := []byte(body)
		id := uploadsHTTPJSON(t, open(path, key, map[string]any{"mode": "overwrite"}, len(data)), 201)["id"].(string)
		uploadsHTTPJSON(t, h.part(id, 1, data, nil), 201)
		return h.commit(t, id)
	}

	first := uploadsHTTPJSON(t, h.request("PUT", "/api/files/content?"+url.Values{"path": {"/overwrite.txt"}}.Encode(), h.bearer,
		[]byte("one"), http.Header{"If-None-Match": {"*"}}), 201)

	// A refused create names the revision the service observed, so the client's next attempt
	// can name it without a metadata read of its own.
	problem := uploadsHTTPJSON(t, open("/overwrite.txt", "overwrite-refused", map[string]any{"mode": "create"}, 3), 409)
	if problem["code"] != "name_conflict" || problem["observedRevision"] != first["contentRevision"] {
		t.Fatalf("refusal did not name the destination: %v (metadata %v)", problem, first["contentRevision"])
	}

	// Overwriting an existing file replaces the bytes the condition names and keeps the
	// identity: a replacement never allocates a new ID.
	replaced := overwrite(t, "/overwrite.txt", "overwrite-existing", "two")
	if replaced["id"] != first["id"] {
		t.Fatalf("overwrite changed the file identity: %v then %v", first["id"], replaced["id"])
	}
	if content := h.request("GET", "/api/files/content?"+url.Values{"path": {"/overwrite.txt"}}.Encode(), h.bearer, nil, nil); content.Body.String() != "two" {
		t.Fatalf("replaced content = %q", content.Body.String())
	}

	// Overwriting a destination that holds nothing creates it, exactly as a create would.
	created := overwrite(t, "/overwrite-new.txt", "overwrite-absent", "new")
	if created["id"] == "" || created["id"] == first["id"] {
		t.Fatalf("an absent destination did not get a fresh identity: %v", created)
	}
	if content := h.request("GET", "/api/files/content?"+url.Values{"path": {"/overwrite-new.txt"}}.Encode(), h.bearer, nil, nil); content.Body.String() != "new" {
		t.Fatalf("created content = %q", content.Body.String())
	}

	// An overwrite names no revision, so one sent with a revision is a client bug that must
	// be refused rather than silently treated as a replacement.
	uploadsHTTPProblem(t, open("/overwrite.txt", "overwrite-with-revision", map[string]any{"mode": "overwrite", "contentRevision": "cr_x"}, 3), 400, "invalid_request")
	f.checkNoLiability(t, h.root)
}
