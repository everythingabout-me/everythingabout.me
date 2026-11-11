package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/storagebackend"
)

func TestFilesNativeUploadsPostgresIndependentReceiverAndCommit(t *testing.T) {
	f := newObjectsAPIFixture(t)
	if _, err := f.db.Pool().Exec(f.ctx, `update file_budgets set logical_limit=$1,physical_limit=$2`, int64(1<<30), int64(3<<30)); err != nil {
		t.Fatal(err)
	}
	receiverConfig := *f.cfg
	receiverConfig.Storage = f.cfg.Storage
	receiverConfig.Storage.ObjectSpoolRoot = t.TempDir()
	receiverConfig.FileUploadIngress = "embedded"
	backend, err := storagebackend.New(&receiverConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	receiver := New(&receiverConfig, f.db, backend.Legacy(), nil)
	receiver.SetStorageFactory(backend)
	receiver.uploadSpools = func(storage.Config) (FileUploadSpool, error) {
		t.Error("native receiver invoked local chunk spool")
		return nil, fmt.Errorf("unexpected spool")
	}
	f.server.uploadSpools = func(storage.Config) (FileUploadSpool, error) {
		t.Error("native commit invoked local chunk spool")
		return nil, fmt.Errorf("unexpected spool")
	}
	create := func(path, key string, size int64) string {
		body, _ := json.Marshal(map[string]any{"path": path, "size": size})
		response := f.request(t, "POST", "/uploads", "", body, http.Header{"Idempotency-Key": {key}, "Content-Type": {"application/json"}})
		return uploadsHTTPJSON(t, response, 201)["id"].(string)
	}
	send := func(id string, n int, data []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "https://files.example"+uploadRoute(id, fmt.Sprintf("/parts/%d", n)), bytes.NewReader(data)).WithContext(f.ctx)
		req.Header.Set("Authorization", "Bearer "+f.tokens[0])
		response := httptest.NewRecorder()
		receiver.Handler().ServeHTTP(response, req)
		return response
	}
	data := bytes.Repeat([]byte("x"), int(5*filesUploadPartSize+13))
	copy(data[len(data)-13:], []byte("binary-tail\x00\xff"))
	id := create("/native-large", "large-native", int64(len(data)))
	// Out-of-order bytes are already durable at the provider before each public ACK.
	for _, n := range []int{6, 2, 1, 5, 3, 4} {
		lo := int64(n-1) * filesUploadPartSize
		hi := min(lo+filesUploadPartSize, int64(len(data)))
		uploadsHTTPJSON(t, send(id, n, data[lo:hi]), 201)
		f.provider.mu.Lock()
		visible := false
		for _, v := range f.provider.multipart {
			if v.key == "drive/"+f.owners[0]+"/native-large" && bytes.Equal(v.parts[n], data[lo:hi]) {
				visible = true
			}
		}
		_, published := f.provider.current("drive/" + f.owners[0] + "/native-large")
		f.provider.mu.Unlock()
		if !visible || published {
			t.Fatalf("part%d ACK did not represent unpublished native bytes", n)
		}
	}
	uploadsHTTPJSON(t, send(id, 1, data[:filesUploadPartSize]), 200)
	uploadsHTTPProblem(t, send(id, 1, bytes.Repeat([]byte("z"), int(filesUploadPartSize))), 409, "part_conflict")
	f.provider.mu.Lock()
	partsBefore := f.provider.partRequests
	f.provider.mu.Unlock()
	if partsBefore != 6 {
		t.Fatalf("replay resent native bytes: %d", partsBefore)
	}
	response := f.request(t, "POST", "/uploads/"+id+"/commit", "", nil, nil)
	if response.Code == http.StatusAccepted {
		deadline := time.Now().Add(12 * time.Second)
		for {
			state := uploadsHTTPJSON(t, f.request(t, "GET", "/operations/"+id, "", nil, nil), 200)
			if state["state"] == "succeeded" {
				break
			}
			if state["state"] == "failed" || state["state"] == "outcome_unknown" || time.Now().After(deadline) {
				t.Fatalf("native commit did not settle: %v", state)
			}
			time.Sleep(2 * time.Millisecond)
		}
	} else {
		uploadsHTTPJSON(t, response, 200)
	}
	uploadsHTTPJSON(t, f.request(t, "POST", "/uploads/"+id+"/commit", "", nil, nil), 200)
	key := "drive/" + f.owners[0] + "/native-large"
	f.provider.mu.Lock()
	object, found := f.provider.current(key)
	puts, completes := f.provider.puts[key], f.provider.completeRequests[key]
	f.provider.mu.Unlock()
	if !found || !bytes.Equal(object.Body, data) || puts != 0 || completes != 1 {
		t.Fatalf("commit retransmitted content: found=%t puts=%d complete=%d", found, puts, completes)
	}
	for _, root := range []string{receiverConfig.Storage.ObjectSpoolRoot, f.cfg.Storage.ObjectSpoolRoot} {
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && strings.HasSuffix(path, ".data") {
				t.Errorf("whole file retained in native path: %s", path)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Empty files still use a one-shot conditional zero-byte PUT at commit only.
	empty := create("/native-empty", "empty-native", 0)
	response = f.request(t, "POST", "/uploads/"+empty+"/commit", "", nil, nil)
	if response.Code == http.StatusAccepted {
		deadline := time.Now().Add(8 * time.Second)
		for {
			status := uploadsHTTPJSON(t, f.request(t, "GET", "/uploads/"+empty, "", nil, nil), 200)
			if status["state"] == "committed" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("empty native upload did not commit: %v", status)
			}
			time.Sleep(time.Millisecond)
		}
	} else {
		uploadsHTTPJSON(t, response, 200)
	}
}

func TestFilesNativeMetadataPostgresPoolCapacityAndSharedFence(t *testing.T) {
	f := newObjectsAPIFixture(t)
	journal := fileops.NewPostgres(f.db.Pool())
	body, _ := json.Marshal(map[string]any{"path": "/metadata-concurrency", "size": 4})
	id := uploadsHTTPJSON(t, f.request(t, "POST", "/uploads", "", body, http.Header{"Idempotency-Key": {"pool-test"}, "Content-Type": {"application/json"}}), 201)["id"].(string)
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	var group sync.WaitGroup
	failures := make(chan error, 16)
	for i := 1; i <= 16; i++ {
		group.Add(1)
		go func(number int) {
			defer group.Done()
			err := journal.WithUploadMetadata(ctx, "capacity", id, number, func(record storage.UploadMetadataRecord) error {
				// The receipt callback requires a second pool connection. Sixteen requests
				// against the default ten-connection pool must never deadlock each other.
				var one int
				return f.db.Pool().QueryRow(ctx, `select 1`).Scan(&one)
			})
			if storage.IsCode(err, "upload_busy") {
				return
			}
			if err != nil {
				failures <- err
			}
		}(i)
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- journal.WithUploadMetadata(ctx, "fence", id, 1, func(storage.UploadMetadataRecord) error { close(entered); <-release; return nil })
	}()
	<-entered
	short, stop := context.WithTimeout(ctx, 40*time.Millisecond)
	err := journal.WithUploadMetadata(short, "fence", id, 0, func(storage.UploadMetadataRecord) error {
		t.Error("cleanup entered while receiver held shared fence")
		return nil
	})
	stop()
	if err == nil {
		t.Error("exclusive session fence ignored active receiver")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
