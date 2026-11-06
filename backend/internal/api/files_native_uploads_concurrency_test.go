package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storagebackend"
)

func TestFilesNativeUploadsPostgresPartsActuallyRunConcurrently(t *testing.T) {
	f := newObjectsAPIFixture(t)
	if _, err := f.db.Pool().Exec(f.ctx, `update file_budgets set logical_limit=$1,physical_limit=$2`, int64(1<<30), int64(3<<30)); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"path": "/concurrent-native", "size": 2*filesUploadPartSize + 1})
	id := uploadsHTTPJSON(t, f.request(t, "POST", "/uploads", "", body, http.Header{"Idempotency-Key": {"concurrent"}, "Content-Type": {"application/json"}}), 201)["id"].(string)
	// Establish the allocation before any parallel request, so an exclusive
	// allocation lookup on every part would deterministically serialize the test.
	uploadsHTTPJSON(t, f.request(t, "PUT", "/uploads/"+id+"/parts/3", "", []byte("x"), nil), 201)
	entered, release := make(chan int, 2), make(chan struct{})
	f.provider.mu.Lock()
	f.provider.partEntered = entered
	f.provider.partRelease = release
	f.provider.mu.Unlock()
	done := make(chan *httptest.ResponseRecorder, 2)
	send := func(n int) {
		done <- f.request(t, "PUT", fmt.Sprintf("/uploads/%s/parts/%d", id, n), "", bytes.Repeat([]byte("a"), int(filesUploadPartSize)), nil)
	}
	go send(1)
	select {
	case n := <-entered:
		if n != 1 {
			t.Error("wrong first native request", n)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("first native part did not enter")
	}
	go send(2)
	select {
	case n := <-entered:
		if n != 2 {
			t.Error("wrong parallel native request", n)
		}
	case <-time.After(3 * time.Second):
		close(release)
		<-done
		<-done
		t.Fatal("second native part waited for first: same-file ingress is serialized")
	}
	close(release)
	uploadsHTTPJSON(t, <-done, 201)
	uploadsHTTPJSON(t, <-done, 201)
}

func TestFilesNativeUploadsPostgresRequireMultipartAdmission(t *testing.T) {
	f := newObjectsAPIFixture(t)
	encoded, err := os.ReadFile(f.cfg.Storage.ObjectAdmissionFile)
	if err != nil {
		t.Fatal(err)
	}
	var proof objectmutation.Admission
	if err = json.Unmarshal(encoded, &proof); err != nil {
		t.Fatal(err)
	}
	proof.MultipartUploads = false
	encoded, _ = json.Marshal(proof)
	path := filepath.Join(t.TempDir(), "without-multipart.json")
	if err = os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := *f.cfg
	cfg.Storage = f.cfg.Storage
	cfg.Storage.ObjectAdmissionFile = path
	backend, err := storagebackend.New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := New(&cfg, f.db, backend.Legacy(), nil)
	server.SetStorageFactory(backend)
	f.server = server
	caps := uploadsHTTPJSON(t, f.request(t, "GET", "/capabilities", "/", nil, nil), 200)
	if caps["uploads"] != false || caps["maxUploadParallelism"] != nil {
		t.Fatalf("unproven native upload advertised: %v", caps)
	}
	body, _ := json.Marshal(map[string]any{"path": "/unproven", "size": 4})
	uploadsHTTPProblem(t, f.request(t, "POST", "/uploads", "", body, http.Header{"Idempotency-Key": {"unproven"}, "Content-Type": {"application/json"}}), 422, "unsupported_option")
	var sessions int
	if err = f.db.Pool().QueryRow(f.ctx, `select count(*) from file_uploads`).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("unproven upload allocated: %d %v", sessions, err)
	}
	// The independent ordinary content PUT retains its existing admitted policy.
	uploadsHTTPJSON(t, f.request(t, "PUT", "/content", "/small", []byte("data"), http.Header{"If-None-Match": {"*"}}), 201)
}
