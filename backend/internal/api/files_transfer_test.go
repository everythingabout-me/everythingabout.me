package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/config"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

func TestGrantExpiryRoundsUpToAStableWindow(t *testing.T) {
	base := time.Date(2026, 11, 6, 10, 1, 2, 0, time.UTC)
	first := filesGrantExpiry(base)
	second := filesGrantExpiry(base.Add(58 * time.Minute))
	boundary := time.Date(2026, 11, 6, 11, 0, 0, 0, time.UTC)
	if first != second || first.Before(base) || first.Sub(base) > time.Hour || !filesGrantExpiry(boundary).After(boundary) {
		t.Fatalf("grant window is not stable: %v %v", first, second)
	}
}

func TestPlannedCoverageRejectsIncompleteOrWrongGeometry(t *testing.T) {
	intent := storage.WriteIntent{Target: "a", Size: filesUploadPartSize + 4}
	full := []storage.NativeUploadPart{{Number: 1, Size: filesUploadPartSize}, {Number: 2, Size: 4}}
	if err := filesPlannedCoverage(intent, full); err != nil {
		t.Fatal(err)
	}
	for _, parts := range [][]storage.NativeUploadPart{
		{{Number: 1, Size: filesUploadPartSize}},
		{{Number: 1, Size: filesUploadPartSize - 1}, {Number: 2, Size: 4}},
		{{Number: 2, Size: filesUploadPartSize}, {Number: 1, Size: 4}},
	} {
		if err := filesPlannedCoverage(intent, parts); err == nil {
			t.Fatalf("accepted provider listing %+v", parts)
		}
	}
	if err := filesPlannedCoverage(storage.WriteIntent{Target: "zero"}, nil); err != nil {
		t.Fatal("zero-byte session needs no provider parts")
	}
}

func TestContentSelectorURLPreservesNativeSpellingOnce(t *testing.T) {
	path := fc.Path("/Notes/ +%23?#中文.md")
	got := filesContentSelectorURL(fc.EntrySelector{Path: path}, true)
	parsed, err := url.Parse(got)
	if err != nil || parsed.Path != fc.Base+"/content" {
		t.Fatal(got, err)
	}
	if parsed.Query().Get("path") != string(path) || parsed.Query().Get("disposition") != "inline" {
		t.Fatalf("selector was decoded or re-encoded: %q", got)
	}
}

func TestUploadIntentRejectsMalformedStoredJSON(t *testing.T) {
	_, err := filesUploadIntent(fileops.Upload{Operation: fileops.Operation{Intent: []byte("{")}})
	if err == nil {
		t.Fatal("malformed stored intent was accepted")
	}
}

type plannedTestWriter struct {
	fileops.NativeActions
	plan storage.PlannedUpload
}

func (w *plannedTestWriter) BeginPlannedUpload(context.Context, storage.WriteIntent, storage.UploadMetadata, time.Time) (storage.PlannedUpload, error) {
	return w.plan, nil
}
func (w *plannedTestWriter) SignUploadParts(context.Context, storage.WriteIntent, []int, storage.UploadMetadata, time.Time) ([]storage.SignedPartGrant, error) {
	return nil, nil
}
func (w *plannedTestWriter) ListUploadParts(context.Context, storage.WriteIntent, int, int, storage.UploadMetadata) ([]storage.NativeUploadPart, bool, error) {
	return nil, false, nil
}
func (w *plannedTestWriter) SealPlannedUpload(context.Context, storage.WriteIntent, storage.UploadMetadata, storage.Checkpoint) (storage.Staged, error) {
	return storage.Staged{}, nil
}
func (w *plannedTestWriter) DiscardPlannedUpload(context.Context, storage.WriteIntent, storage.UploadMetadata) error {
	return nil
}

func TestUploadTransportForClassAAndC(t *testing.T) {
	intent, _ := json.Marshal(storage.WriteIntent{Target: "a", Size: 4})
	u := fileops.Upload{ID: "u_1", PartSize: filesUploadPartSize, ExpiresAt: time.Now().Add(24 * time.Hour), Operation: fileops.Operation{Intent: intent}}
	writer := &plannedTestWriter{plan: storage.PlannedUpload{PartSize: filesUploadPartSize, ExpiresAt: time.Now().Add(time.Hour), Parts: []storage.SignedPartGrant{{Number: 1, Size: 4, Method: "PUT", URL: "https://bucket.example/drive/owner/a?partNumber=1&uploadId=n", Headers: map[string]string{"Content-Type": "application/octet-stream"}, ExpiresAt: time.Now().Add(time.Hour)}}}}
	s := &Server{cfg: &config.Config{Storage: config.Storage{TransferClass: storage.TransferPresignedParts}, FileUploadIngress: "edgeone"}}
	transport, err := s.filesUploadTransport(context.Background(), writer, u)
	if err != nil {
		t.Fatal(err)
	}
	payload := transport.(map[string]any)
	if payload["kind"] != storage.TransferPresignedParts {
		t.Fatalf("unexpected transport %+v", payload)
	}
	part := payload["parts"].([]any)[0].(map[string]any)
	if !strings.HasPrefix(part["url"].(string), filesTransferPrefix) {
		t.Fatalf("edge relay did not rewrite provider URL: %v", part)
	}

	s.cfg.Storage.TransferClass = storage.TransferRelay
	transport, err = s.filesUploadTransport(context.Background(), writer, u)
	if err != nil {
		t.Fatal(err)
	}
	payload = transport.(map[string]any)
	if payload["kind"] != storage.TransferRelay || !strings.HasSuffix(payload["url"].(string), "/uploads/u_1/parts") {
		t.Fatalf("unexpected relay transport %+v", payload)
	}
}

func TestClassAContentAndSimplePutAreRefusedBeforeBytes(t *testing.T) {
	s := &Server{cfg: &config.Config{Storage: config.Storage{TransferClass: storage.TransferPresignedParts}}}
	f := &fileRequest{}
	w := httptest.NewRecorder()
	err := s.handleFilesContent(w, httptest.NewRequest("GET", "/api/files/content?path=%2Fa", nil), f)
	var problem *fc.Problem
	if !errors.As(err, &problem) || problem.Status != 409 || problem.Code != "grant_required" {
		t.Fatalf("class A content GET = %v", err)
	}
	err = s.handleFilesPut(w, httptest.NewRequest("PUT", "/api/files/content?path=%2Fa", nil), f)
	if !errors.As(err, &problem) || problem.Status != 409 || problem.Code != "session_required" {
		t.Fatalf("class A simple PUT = %v", err)
	}
}
