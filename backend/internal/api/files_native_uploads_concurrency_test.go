package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
