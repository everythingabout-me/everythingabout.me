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
	if err := filesPlannedCoverage(intent, filesUploadPartSize, full); err != nil {
		t.Fatal(err)
	}
	for _, parts := range [][]storage.NativeUploadPart{
		{{Number: 1, Size: filesUploadPartSize}},
		{{Number: 1, Size: filesUploadPartSize - 1}, {Number: 2, Size: 4}},
		{{Number: 2, Size: filesUploadPartSize}, {Number: 1, Size: 4}},
	} {
		if err := filesPlannedCoverage(intent, filesUploadPartSize, parts); err == nil {
			t.Fatalf("accepted provider listing %+v", parts)
		}
	}
	if err := filesPlannedCoverage(storage.WriteIntent{Target: "zero"}, filesUploadPartSize, nil); err != nil {
		t.Fatal("zero-byte session needs no provider parts")
	}
}

// The commit must describe the gap, not only report it: the cause is computed from the
// provider's own part listing and is the only text that says which part to send.
func TestUploadIncompleteProblemNamesTheMissingParts(t *testing.T) {
	const size = int64(537131743) // 33 parts of 16 MiB plus a 260831-byte final part.
	listed := make([]storage.NativeUploadPart, 32)
	for index := range listed {
		listed[index] = storage.NativeUploadPart{Number: index + 1, Size: filesUploadPartSize}
	}
	err := filesPlannedCoverage(storage.WriteIntent{Target: "large", Size: size}, filesUploadPartSize, listed)
	var cause *fileops.Error
	if !errors.As(err, &cause) || cause.Code != "upload_incomplete" {
		t.Fatalf("coverage error = %v", err)
	}
	problemErr := filesMutationProblem(err)
	var problem *fc.Problem
	if !errors.As(problemErr, &problem) || problem.Status != 409 || problem.Code != "upload_incomplete" {
		t.Fatalf("problem = %v", problemErr)
	}
	if !strings.Contains(problem.Detail, "needs 33 parts") || !strings.Contains(problem.Detail, "listed 32") {
		t.Fatalf("detail does not name the gap: %q", problem.Detail)
	}
	// The session's geometry is the authority, not the package constant: a connection
	// with 8 MiB parts must report the count its own partSize implies.
	smaller := filesPlannedCoverage(storage.WriteIntent{Target: "large", Size: size}, 8<<20, listed)
	var smallerCause *fileops.Error
	if !errors.As(smaller, &smallerCause) || smallerCause.Cause == nil || !strings.Contains(smallerCause.Cause.Error(), "needs 65 parts") {
		t.Fatalf("partSize argument was ignored: %v", smaller)
	}
	// A missing cause still keeps an actionable sentence instead of the bare code.
	var bare *fc.Problem
	if !errors.As(filesMutationProblem(fileops.E("upload_incomplete", nil)), &bare) ||
		!strings.Contains(bare.Detail, "missing parts") {
		t.Fatalf("generic upload_incomplete detail = %v", bare)
	}
}

// A window that does not cover the session must say so in the payload. The 33-part
// (537131743-byte) case is the failure this field exists for: 32 grants, not complete.
func TestPlannedTransportReportsTheWindow(t *testing.T) {
	intent, _ := json.Marshal(storage.WriteIntent{Target: "a", Size: 537131743})
	expires := time.Now().Add(time.Hour)
	window := func(count int) []storage.SignedPartGrant {
		grants := make([]storage.SignedPartGrant, 0, count)
		for number := 1; number <= count; number++ {
			grants = append(grants, storage.SignedPartGrant{Number: number, Size: filesUploadPartSize, Method: "PUT", URL: "https://bucket.example/a", Headers: map[string]string{}, ExpiresAt: expires})
		}
		return grants
	}
	for _, test := range []struct {
		name      string
		partCount int
		grants    int
		granted   int
		complete  bool
	}{
		{"first window of a 33-part session", 33, 32, 32, false},
		{"whole 3-part session", 3, 3, 3, true},
		{"zero-byte session", 0, 0, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			u := fileops.Upload{ID: "u_window", PartSize: filesUploadPartSize, PartCount: test.partCount, ExpiresAt: expires, Operation: fileops.Operation{Intent: intent}}
			writer := &plannedTestWriter{plan: storage.PlannedUpload{PartSize: filesUploadPartSize, ExpiresAt: expires, Parts: window(test.grants)}}
			s := &Server{cfg: &config.Config{Storage: config.Storage{TransferClass: storage.TransferPresignedParts}}}
			transport, err := s.filesUploadTransport(context.Background(), writer, u)
			if err != nil {
				t.Fatal(err)
			}
			payload := transport.(map[string]any)
			granted, _ := payload["grantedParts"].(int)
			complete, _ := payload["partsComplete"].(bool)
			if granted != test.granted || complete != test.complete || len(payload["parts"].([]*filesPartGrant)) != test.grants {
				t.Fatalf("window = %+v", payload)
			}
		})
	}
}

// directGrantWriter models both direct shapes: a form upload and a conditional PUT.
type directGrantWriter struct {
	fileops.NativeActions
	form bool
}

func (w *directGrantWriter) SupportsDirectPost() bool { return w.form }

func (w *directGrantWriter) BeginDirectPost(_ context.Context, in storage.WriteIntent, _ storage.UploadMetadata, expires time.Time) (storage.SignedPartGrant, error) {
	return storage.SignedPartGrant{
		Number: 1, Size: in.Size, Method: "POST", URL: "https://bucket.example/",
		Form:      []storage.FormField{{Name: "key", Value: string(in.Target)}},
		FileField: "file", Headers: map[string]string{}, ExpiresAt: expires,
	}, nil
}

func (w *directGrantWriter) SupportsDirectPut(storage.Conditions) bool { return true }

func (w *directGrantWriter) BeginDirectPut(_ context.Context, in storage.WriteIntent, _ storage.UploadMetadata, expires time.Time) (storage.SignedPartGrant, error) {
	return storage.SignedPartGrant{Number: 1, Size: in.Size, Method: "PUT", URL: "https://bucket.example/" + string(in.Target), Headers: map[string]string{"If-None-Match": "*"}, ExpiresAt: expires}, nil
}

// A direct grant publishes the destination itself, so its transport is one complete
// grant: there is no next window to sign.
func TestDirectTransportIsOneCompleteGrant(t *testing.T) {
	stored, _ := json.Marshal(storage.WriteIntent{Target: "small.txt", Size: 4, Conditions: storage.Conditions{Absent: true}})
	u := fileops.Upload{ID: "u_direct", PartSize: filesUploadPartSize, PartCount: 1, ExpiresAt: time.Now().Add(time.Hour), Operation: fileops.Operation{Intent: stored}}
	s := &Server{cfg: &config.Config{Storage: config.Storage{TransferClass: storage.TransferPresignedParts}}}
	for _, test := range []struct {
		name string
		form bool
	}{
		{"form upload", true},
		{"conditional put", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport, err := s.filesDirectPutTransport(context.Background(), &directGrantWriter{form: test.form}, u)
			if err != nil {
				t.Fatal(err)
			}
			payload := transport.(map[string]any)
			granted, _ := payload["grantedParts"].(int)
			complete, _ := payload["partsComplete"].(bool)
			if granted != 1 || !complete || len(payload["parts"].([]any)) != 1 {
				t.Fatalf("direct transport = %+v", payload)
			}
		})
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
	plan   storage.PlannedUpload
	intent storage.WriteIntent
}

func (w *plannedTestWriter) BeginPlannedUpload(_ context.Context, in storage.WriteIntent, _ storage.UploadMetadata, _ time.Time) (storage.PlannedUpload, error) {
	w.intent = in
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
	part := payload["parts"].([]*filesPartGrant)[0]
	if !strings.HasPrefix(part.URL, filesTransferPrefix) {
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

// The class A driver keys its native multipart allocation and durable metadata by
// WriteIntent.ID, and rejects an intent without one. Losing the session identity
// between the journal and the driver fails session creation and every part call.
func TestClassAUploadTransportCarriesTheSessionIdentity(t *testing.T) {
	stored, _ := json.Marshal(storage.WriteIntent{Target: "a", Size: 4})
	u := fileops.Upload{ID: "u_identity", PartSize: filesUploadPartSize, ExpiresAt: time.Now().Add(24 * time.Hour), Operation: fileops.Operation{Intent: stored}}
	writer := &plannedTestWriter{plan: storage.PlannedUpload{PartSize: filesUploadPartSize, ExpiresAt: time.Now().Add(time.Hour)}}
	s := &Server{cfg: &config.Config{Storage: config.Storage{TransferClass: storage.TransferPresignedParts}}}
	if _, err := s.filesUploadTransport(context.Background(), writer, u); err != nil {
		t.Fatal(err)
	}
	if writer.intent.ID != u.ID {
		t.Fatalf("driver received intent ID %q, want the session id %q", writer.intent.ID, u.ID)
	}
	intent, err := filesUploadIntent(u)
	if err != nil || intent.ID != u.ID || intent.Target != "a" {
		t.Fatalf("decoded intent = %+v, %v", intent, err)
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
