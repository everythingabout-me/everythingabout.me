package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestFilesCreateOnlyPostgresUnversionedUploads(t *testing.T) {
	f := newObjectsAPIFixture(t, true)
	if _, err := f.db.Pool().Exec(f.ctx, `update file_budgets set logical_limit=$1,physical_limit=$2`, int64(1<<30), int64(3<<30)); err != nil {
		t.Fatal(err)
	}
	capabilities := objectsAPIJSON(t, f.request(t, "GET", "/capabilities", "/", nil, nil), 200)
	for _, name := range []string{"write", "uploads", "createOnly"} {
		if capabilities[name] != true {
			t.Fatalf("missing capability %s: %v", name, capabilities)
		}
	}
	for _, name := range []string{"pinnedRead", "strongContentValidators", "sourceModifiedAt"} {
		if capabilities[name] != false {
			t.Fatalf("unsupported capability %s advertised: %v", name, capabilities)
		}
	}
	headers := http.Header{"Content-Type": {"application/octet-stream"}, "If-None-Match": {"*"}, "Idempotency-Key": {"small-create"}}
	small := []byte("ordinary unversioned file\x00\xff")
	result := objectsAPIJSON(t, f.request(t, "PUT", "/content", "/small+中文.txt", small, headers), 201)
	if result["contentRevision"] != nil {
		t.Fatalf("unversioned receipt fabricated a revision: %v", result)
	}
	read := f.request(t, "GET", "/content", "/small+中文.txt", nil, nil)
	if read.Code != 200 || !bytes.Equal(read.Body.Bytes(), small) || read.Header().Get("ETag") != "" {
		t.Fatalf("unversioned content read failed: HTTP %d headers=%v body=%q", read.Code, read.Header(), read.Body.Bytes())
	}
	headers.Set("Idempotency-Key", "occupied-create")
	uploadsHTTPProblem(t, f.request(t, "PUT", "/content", "/small+中文.txt", []byte("overwrite"), headers), 412, "precondition_failed")
	replace, _ := json.Marshal(map[string]any{"path": "/small+中文.txt", "size": 3, "condition": map[string]any{"mode": "replace", "contentRevision": "cr_unsupported"}})
	uploadsHTTPProblem(t, f.request(t, "POST", "/uploads", "", replace, http.Header{"Idempotency-Key": {"replace"}, "Content-Type": {"application/json"}}), 422, "unsupported_option")
	uploadsHTTPProblem(t, f.request(t, "DELETE", "", "/small+中文.txt", nil, nil), 422, "unsupported_option")
	uploadsHTTPProblem(t, f.request(t, "POST", "", "", []byte(`{"path":"/new-folder","kind":"folder"}`), http.Header{"Content-Type": {"application/json"}}), 422, "unsupported_option")

	data := bytes.Repeat([]byte("a"), int(filesUploadPartSize)+23)
	input, _ := json.Marshal(map[string]any{"path": "/multipart+中文.bin", "size": len(data)})
	id := uploadsHTTPJSON(t, f.request(t, "POST", "/uploads", "", input, http.Header{"Idempotency-Key": {"multipart-create"}, "Content-Type": {"application/json"}}), 201)["id"].(string)
	for _, number := range []int{2, 1} {
		lo := int64(number-1) * filesUploadPartSize
		hi := min(lo+filesUploadPartSize, int64(len(data)))
		uploadsHTTPJSON(t, f.request(t, "PUT", fmt.Sprintf("/uploads/%s/parts/%d", id, number), "", data[lo:hi], http.Header{"Content-Type": {"application/octet-stream"}}), 201)
	}
	uploadsHTTPJSON(t, f.request(t, "POST", "/uploads/"+id+"/commit", "", nil, nil), 202)
	deadline := time.Now().Add(10 * time.Second)
	for {
		status := uploadsHTTPJSON(t, f.request(t, "GET", "/uploads/"+id, "", nil, nil), 200)
		if status["state"] == "committed" {
			break
		}
		if status["state"] == "failed" || status["state"] == "outcome_unknown" || time.Now().After(deadline) {
			t.Fatalf("unversioned native upload did not commit: %v", status)
		}
		time.Sleep(time.Millisecond)
	}
	read = f.request(t, "GET", "/content", "/multipart+中文.bin", nil, nil)
	if read.Code != 200 || !bytes.Equal(read.Body.Bytes(), data) {
		t.Fatalf("multipart readback differs: HTTP %d len=%d", read.Code, read.Body.Len())
	}
	key := "drive/" + f.owners[0] + "/multipart+中文.bin"
	f.provider.mu.Lock()
	completes, puts := f.provider.completeRequests[key], f.provider.puts[key]
	f.provider.mu.Unlock()
	if completes != 1 || puts != 0 {
		t.Fatalf("native completion retransmitted content: completes=%d puts=%d", completes, puts)
	}
	var indexed int
	if err := f.db.Pool().QueryRow(f.ctx, `select count(*) from nodes where owner_id=$1::uuid and is_dir=false`, f.owners[0]).Scan(&indexed); err != nil || indexed != 2 {
		t.Fatalf("native publication did not settle index: count=%d err=%v", indexed, err)
	}
}
