package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

func TestFilesMutationDigestAndStrictJSON(t *testing.T) {
	sum := sha256.Sum256([]byte("original"))
	value := "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
	digest, err := filesDigest(value)
	if err != nil || digest.Algorithm != "sha-256" || !bytes.Equal(digest.Value, sum[:]) {
		t.Fatal("valid structured digest rejected", err)
	}
	for _, bad := range []string{"sha256=00", "sha-256=:invalid:", "sha-256=:YWJj:", value + ", sha-256=:YWJj:", "SHA-256=:YWJj:"} {
		if _, err := filesDigest(bad); err == nil {
			t.Fatalf("bad digest accepted %q", bad)
		}
	}
	for _, body := range []string{`{"path":"/a","unknown":true}`, `{"path":"/a"} {}`, `{"path":"/a"}` + strings.Repeat(" ", 1<<20), `null`} {
		var request struct {
			Path string `json:"path"`
		}
		r := httptest.NewRequest("POST", "/api/files", strings.NewReader(body))
		err := strictFilesJSON(r, &request)
		// null is a valid JSON value but action-specific required field validation rejects it.
		if body != "null" && err == nil {
			t.Fatalf("invalid JSON accepted %s", body[:min(len(body), 30)])
		}
	}
	var request struct {
		Path string `json:"path"`
	}
	if err := strictFilesJSON(httptest.NewRequest("POST", "/api/files", strings.NewReader(`{"path":"/ +%2F?# "}`)), &request); err != nil || request.Path != "/ +%2F?# " {
		t.Fatal("JSON path spelling changed", err)
	}
}
func TestFilesPUTReplayAlwaysConsumesAndVerifies(t *testing.T) {
	data := []byte("payload")
	sum := sha256.Sum256(data)
	intent := storage.WriteIntent{Size: int64(len(data))}
	stage := &storage.Staged{Digest: &storage.Digest{Algorithm: "sha-256", Value: sum[:]}}
	for _, tc := range []struct {
		body string
		code string
	}{{"payload", ""}, {"changed", "idempotency_conflict"}, {"short", "length_mismatch"}, {"payload-extra", "length_mismatch"}} {
		body := strings.NewReader(tc.body)
		err := filesValidatePutReplay(context.Background(), body, intent, stage)
		if tc.code == "" && err != nil || tc.code != "" && !fileops.IsCode(err, tc.code) {
			t.Fatalf("replay %q got %v", tc.body, err)
		}
		if body.Len() != 0 && len(tc.body) <= len(data)+1 {
			t.Fatal("retry body not consumed")
		}
	}
	if err := filesValidatePutReplay(context.Background(), strings.NewReader("payload"), intent, nil); !fileops.IsCode(err, "idempotency_conflict") {
		t.Fatal("unproven replay claimed identical")
	}
}
func TestFilesMutationPublicOperationConcealsNativeEvidence(t *testing.T) {
	op := fileops.Operation{ID: "operation", Kind: "copy", State: fileops.Unknown, Checkpoint: []byte("private-upload-id"), Root: fileops.Root{OwnerID: "private-owner", ContentRoot: "private-root"}, PrincipalID: "private-token", Entries: []fileops.EntryEffect{{State: "applied", Evidence: []byte("private-native-version")}}, Result: json.RawMessage(`{"remaining":["/target"]}`)}
	w := httptest.NewRecorder()
	if err := writeFilesOperation(w, op, 202); err != nil {
		t.Fatal(err)
	}
	text := w.Body.String()
	for _, secret := range []string{"private-owner", "private-root", "private-token", "private-upload-id", "private-native-version", "Checkpoint"} {
		if strings.Contains(text, secret) {
			t.Fatal("native operation identity escaped", secret)
		}
	}
	if w.Header().Get("Location") != "/api/files/operations/operation" || w.Header().Get("Retry-After") == "" {
		t.Fatal("operation recovery links missing")
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result["state"] != "outcome_unknown" {
		t.Fatal("invalid public operation", err)
	}
}
func TestFilesMutationProblemsAndPUTUnknownLocation(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
	}{{"quota_exceeded", 507}, {"staging_full", 507}, {"checksum_mismatch", 422}, {"baseline_required", 503}, {"operation_in_progress", 409}, {"precondition_required", 428}, {"idempotency_conflict", 409}, {"target_changed", 412}} {
		err := filesMutationProblem(fileops.E(tc.code, io.ErrUnexpectedEOF))
		var p *fc.Problem
		if !errors.As(err, &p) || p.Status != tc.status {
			t.Fatalf("mapping %s: %v", tc.code, err)
		}
	}
	s := &Server{}
	w := httptest.NewRecorder()
	err := s.filesPutResponse(w, httptest.NewRequest(http.MethodPut, "/api/files/content?path=%2Fa", nil), fileops.Operation{ID: "op", State: fileops.Unknown}, nil, false)
	var p *fc.Problem
	if !errors.As(err, &p) || p.Status != 503 || p.OperationID != "op" || w.Header().Get("Location") != "/api/files/operations/op" {
		t.Fatal("unknown publication lost recoverable identity", err)
	}
}

func TestFilesGuardConflictPreservesOperationID(t *testing.T) {
	err := filesMutationProblem(&fileops.Error{Code: "operation_in_progress", OperationID: "guard-operation", OperationKind: "upload_commit"})
	var p *fc.Problem
	if !errors.As(err, &p) || p.Status != 409 || p.OperationID != "guard-operation" {
		t.Fatalf("guard conflict lost recoverable identity: %v", err)
	}
	if !strings.Contains(p.Detail, "unfinished upload") || !strings.Contains(p.Detail, "guard-operation") {
		t.Fatalf("guard conflict detail names neither the cause nor the operation: %q", p.Detail)
	}
}
