package storagebackend

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
)

// probeFixtureBackend is an object store whose If-Match behaviour is a switch. Its default
// models the finding this probe exists for: the provider enforces the conditional header on
// reads and deletes but silently replaces the object on PUT.
type probeFixtureBackend struct {
	mu                 sync.Mutex
	objects            map[string]probeFixtureObject
	ignoreIfMatch      bool
	unsupportedIfMatch bool
	// refusalStatus is what a refused conditional PUT answers with. 412 is the contract;
	// another status is a refusal for another reason and must not count as evidence.
	refusalStatus int
	sequence      int
}

type probeFixtureObject struct {
	body, etag string
}

func newProbeFixtureBackend(ignoreIfMatch bool) *probeFixtureBackend {
	return &probeFixtureBackend{objects: map[string]probeFixtureObject{}, ignoreIfMatch: ignoreIfMatch, refusalStatus: http.StatusPreconditionFailed}
}

func (b *probeFixtureBackend) Head(_ context.Context, key string) (objectmutation.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	object, ok := b.objects[key]
	if !ok {
		return objectmutation.Object{}, storage.NewError("not_found", storage.Path(key), nil)
	}
	return probeFixtureObjectResult(key, object), nil
}

// Open enforces the condition even on the ignoring fixture: conditional GET and DELETE are
// the operations COS does enforce, so the probe's read-back must be a real check.
func (b *probeFixtureBackend) Open(_ context.Context, key, etag string) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	object, ok := b.objects[key]
	if !ok {
		return nil, storage.NewError("not_found", storage.Path(key), nil)
	}
	if etag == "" || object.etag != etag {
		return nil, storage.NewError("changed", storage.Path(key), nil)
	}
	return io.NopCloser(strings.NewReader(object.body)), nil
}

func (b *probeFixtureBackend) Put(_ context.Context, in objectmutation.Put) (objectmutation.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	object, exists := b.objects[in.Key]
	if in.Absent && exists {
		return objectmutation.Object{}, probeFixtureRefusal(in.Key, "ForbidOverwrite", http.StatusPreconditionFailed)
	}
	if in.ForbidOverwrite && exists {
		return objectmutation.Object{}, probeFixtureRefusal(in.Key, "ForbidOverwrite", http.StatusPreconditionFailed)
	}
	if in.ExpectedETag != "" {
		switch {
		case b.unsupportedIfMatch:
			return objectmutation.Object{}, storage.NewError("unsupported", storage.Path(in.Key), nil)
		case !b.ignoreIfMatch && (!exists || object.etag != in.ExpectedETag):
			return objectmutation.Object{}, probeFixtureRefusal(in.Key, "PreconditionFailed", b.refusalStatus)
		}
	}
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return objectmutation.Object{}, err
	}
	b.sequence++
	stored := probeFixtureObject{body: string(body), etag: `"fixture-` + itoaFixture(b.sequence) + `"`}
	b.objects[in.Key] = stored
	return probeFixtureObjectResult(in.Key, stored), nil
}

// probeFixtureRefusal carries the provider facts a real adapter attaches, so the probe's
// 412 check is exercised instead of the missing-status fallback.
func probeFixtureRefusal(key, code string, status int) error {
	return storage.NewProviderError("precondition_failed", storage.Path(key), status, code, "fixture-request", nil)
}

func (b *probeFixtureBackend) Delete(_ context.Context, key, etag string) (objectmutation.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	object, ok := b.objects[key]
	if !ok {
		return objectmutation.Object{}, storage.NewError("not_found", storage.Path(key), nil)
	}
	if etag == "" || object.etag != etag {
		return objectmutation.Object{}, storage.NewError("precondition_failed", storage.Path(key), nil)
	}
	delete(b.objects, key)
	return objectmutation.Object{Key: key}, nil
}

func probeFixtureObjectResult(key string, object probeFixtureObject) objectmutation.Object {
	return objectmutation.Object{Key: key, ETag: object.etag, Size: int64(len(object.body)), ModifiedAt: time.Now().UTC(), MediaType: "application/octet-stream"}
}

func itoaFixture(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func probeFixtureAdmission(t *testing.T, backend objectmutation.Backend) (objectmutation.Admission, []probeObservation) {
	t.Helper()
	admission, observations, err := ProbeAdmission(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	// The identity fields are filled by the caller that knows the connection, and
	// singleGateway is a deployment assertion no request can measure; neither is what this
	// probe measures.
	admission.ConnectionID, admission.Endpoint, admission.Bucket, admission.Region = "fixture", "https://fixture.invalid", "bucket", "region"
	admission.ProbeReference, admission.SingleGateway = "isolated probe fixture", true
	return admission, observations
}

func probeObservationResult(observations []probeObservation, property string) (string, bool) {
	for _, observation := range observations {
		if observation.Property == property {
			return observation.Result, true
		}
	}
	return "", false
}

// A provider that refuses a stale If-Match is admitted for direct class A replacement: the
// signed header really is the guard the grant claims to carry.
func TestProbeAdmissionMeasuresAnEnforcedConditionalPut(t *testing.T) {
	admission, observations := probeFixtureAdmission(t, newProbeFixtureBackend(false))
	if !admission.ConditionalPut {
		t.Fatalf("an enforced stale If-Match was not recorded: %+v", observations)
	}
	if result, ok := probeObservationResult(observations, "conditionalPut"); !ok || result != "rejected (412)" {
		t.Fatalf("transcript does not name the refused request: %+v", observations)
	}
	if err := objectmutation.ValidateAdmission(admission); err != nil {
		t.Fatal(err)
	}
}

// The finding: COS ignores If-Match on PUT. The probe must see the replacement happen, and
// the connection must still be admissible — it publishes through multipart instead.
func TestProbeAdmissionRecordsAnIgnoredConditionalPut(t *testing.T) {
	admission, observations := probeFixtureAdmission(t, newProbeFixtureBackend(true))
	if admission.ConditionalPut {
		t.Fatal("an ignored stale If-Match was recorded as enforced")
	}
	if result, ok := probeObservationResult(observations, "conditionalPut"); !ok || !strings.Contains(result, "accepted") {
		t.Fatalf("transcript hides the replacement: %+v", observations)
	}
	if err := objectmutation.ValidateAdmission(admission); err != nil {
		t.Fatalf("a connection without conditional PUT was refused admission: %v", err)
	}
}

// A provider that does not implement the header at all is not evidence either: the flag
// stays false and the transcript distinguishes "unsupported" from "accepted".
func TestProbeAdmissionTreatsAnUnsupportedConditionalPutAsNoEvidence(t *testing.T) {
	backend := newProbeFixtureBackend(false)
	backend.unsupportedIfMatch = true
	admission, observations := probeFixtureAdmission(t, backend)
	if admission.ConditionalPut {
		t.Fatal("an unsupported conditional PUT was recorded as enforced")
	}
	if result, ok := probeObservationResult(observations, "conditionalPut"); !ok || !strings.Contains(result, "unsupported") {
		t.Fatalf("transcript does not distinguish unsupported from ignored: %+v", observations)
	}
}

// A refusal for a different reason is not evidence for this capability. The overwrite switch
// also answers with a condition failure, so a 409 here must not admit a direct replacement
// whose only guard is PUT If-Match.
func TestProbeAdmissionRejectsARefusalForAnotherReason(t *testing.T) {
	backend := newProbeFixtureBackend(false)
	backend.refusalStatus = http.StatusConflict
	admission, observations := probeFixtureAdmission(t, backend)
	if admission.ConditionalPut {
		t.Fatal("a 409 was recorded as evidence that PUT If-Match is enforced")
	}
	if result, ok := probeObservationResult(observations, "conditionalPut"); !ok || !strings.Contains(result, "HTTP 409") {
		t.Fatalf("transcript does not say why the refusal was not evidence: %+v", observations)
	}
}
