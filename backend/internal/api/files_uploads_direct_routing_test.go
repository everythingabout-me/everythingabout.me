package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

// directRoutingDriver exposes a DirectPutWriter whose replacement support is the case under
// test. The real object driver answers SupportsDirectPut from its probe evidence, but the S3
// mutation backend used by the fixture does not sign destination PUTs at all, so the two
// branches would be unreachable without this wrapper. Everything else is the real driver.
type directRoutingDriver struct {
	fileops.NativeActions
	storage.PlannedUploadWriter
	factory     *directRoutingFactory
	beginDirect int
	planned     int
}

func (d *directRoutingDriver) SupportsDirectPut(conditions storage.Conditions) bool {
	if conditions.Absent {
		return true
	}
	return len(conditions.ExpectedRevision) > 0 && d.factory.conditionalPut
}

// BeginPlannedUpload returns a synthetic first window: this fixture's mutation backend does
// not sign native parts, and what this test measures is the route the service chose, not the
// provider call. The real multipart commit has its own coverage.
func (d *directRoutingDriver) BeginPlannedUpload(_ context.Context, in storage.WriteIntent, _ storage.UploadMetadata, expires time.Time) (storage.PlannedUpload, error) {
	d.factory.mu.Lock()
	d.planned++
	d.factory.mu.Unlock()
	return storage.PlannedUpload{
		PartSize:  filesUploadPartSize,
		ExpiresAt: expires,
		Parts: []storage.SignedPartGrant{{
			Number: 1, Size: in.Size, Method: http.MethodPut,
			URL:       "https://bucket.example/" + string(in.Target) + "?partNumber=1&uploadId=fixture",
			Headers:   map[string]string{"Content-Type": "application/octet-stream"},
			ExpiresAt: expires,
		}},
	}, nil
}

func (d *directRoutingDriver) BeginDirectPut(_ context.Context, in storage.WriteIntent, _ storage.UploadMetadata, expires time.Time) (storage.SignedPartGrant, error) {
	d.factory.mu.Lock()
	d.beginDirect++
	d.factory.mu.Unlock()
	return storage.SignedPartGrant{
		Number: 1, Size: in.Size, Method: http.MethodPut,
		URL:       "https://bucket.example/" + string(in.Target),
		Headers:   map[string]string{"If-Match": string(in.Conditions.ExpectedRevision)},
		ExpiresAt: expires,
	}, nil
}

type directRoutingFactory struct {
	mu             sync.Mutex
	inner          OwnerStorageFactory
	conditionalPut bool
	drivers        []*directRoutingDriver
}

func (f *directRoutingFactory) Open(ctx context.Context, owner string) (storage.Driver, error) {
	driver, err := f.inner.Open(ctx, owner)
	if err != nil {
		return nil, err
	}
	native, ok := driver.(fileops.NativeActions)
	if !ok {
		driver.Close()
		return nil, errors.New("fixture driver does not expose native actions")
	}
	planned, ok := driver.(storage.PlannedUploadWriter)
	if !ok {
		driver.Close()
		return nil, errors.New("fixture driver does not expose planned uploads")
	}
	f.mu.Lock()
	wrapped := &directRoutingDriver{NativeActions: native, PlannedUploadWriter: planned, factory: f}
	f.drivers = append(f.drivers, wrapped)
	f.mu.Unlock()
	return wrapped, nil
}

// Binding must delegate as well: the request's content namespace is derived from the
// storage binding, and a factory that only forwards Open would silently index this write
// under a different namespace than the one that seeded the destination.
func (f *directRoutingFactory) Binding(owner string) (storage.Config, error) {
	provider, ok := f.inner.(interface {
		Binding(string) (storage.Config, error)
	})
	if !ok {
		return storage.Config{}, errors.New("fixture storage factory cannot report a binding")
	}
	return provider.Binding(owner)
}

func (f *directRoutingFactory) signed() (direct, planned int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, driver := range f.drivers {
		direct += driver.beginDirect
		planned += driver.planned
	}
	return direct, planned
}

// A connection that did not measure a refused stale If-Match must not hand the client a
// grant whose only guard is that header: the write is signed into an unpublished multipart
// session instead, and only the server's commit publishes. The fixture provider is the
// finding — its PUT ignores If-Match and its Complete enforces it.
func TestFilesUploadsReplacementRoutesThroughMultipartWithoutConditionalPutEvidence(t *testing.T) {
	f := newObjectsAPIFixture(t)
	// Seed through the relay class first: with class A configured, an ordinary content PUT
	// is refused by design, and this test is about the upload session that replaces it.
	created := objectsAPIJSON(t, f.request(t, "PUT", "/content", "/routed", []byte("old"), nil), 201)
	f.cfg.Storage.TransferClass = storage.TransferPresignedParts
	factory := &directRoutingFactory{inner: f.backend}
	f.server.SetStorageFactory(factory)

	key := "drive/" + f.owners[0] + "/routed"
	f.provider.mu.Lock()
	putsBefore := f.provider.puts[key]
	f.provider.mu.Unlock()

	body, _ := json.Marshal(map[string]any{
		"path": "/routed", "size": 3, "mediaType": "text/plain",
		"condition": map[string]any{"mode": "replace", "contentRevision": created["contentRevision"]},
	})
	resource := uploadsHTTPJSON(t, f.request(t, "POST", "/uploads", "", body, http.Header{"Idempotency-Key": {"routed-replacement"}}), 201)
	if resource["class"] != "A" {
		t.Fatalf("replacement session class = %v", resource["class"])
	}
	transport, ok := resource["transport"].(map[string]any)
	if !ok || transport["kind"] != "presigned-parts" {
		t.Fatalf("replacement session transport = %v", resource["transport"])
	}
	parts, ok := transport["parts"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("replacement session parts = %v", transport["parts"])
	}
	part := parts[0].(map[string]any)
	partURL := part["url"].(string)
	if !strings.Contains(partURL, "uploadId=") || !strings.Contains(partURL, "partNumber=1") {
		t.Fatalf("replacement was signed as a destination PUT instead of a multipart part: %s", partURL)
	}
	if headers, _ := part["headers"].(map[string]any); headers["If-Match"] != nil {
		t.Fatalf("multipart replacement bound the destination's If-Match: %v", headers)
	}
	direct, planned := factory.signed()
	if direct != 0 {
		t.Fatal("a direct replacement was signed without conditional-PUT evidence")
	}
	if planned != 1 {
		t.Fatalf("replacement did not plan a multipart session: %d planned", planned)
	}
	f.provider.mu.Lock()
	putsAfter := f.provider.puts[key]
	f.provider.mu.Unlock()
	if putsAfter != putsBefore {
		t.Fatalf("routing a replacement through multipart wrote the destination (%d -> %d native PUTs)", putsBefore, putsAfter)
	}
}

// The runtime guard: a session admitted in direct mode must not be re-signed after the
// connection stops admitting its condition. Re-signing it would put the unguarded
// conditional PUT back in a client's hands for the rest of the grant's hour.
func TestFilesUploadsDirectReplacementStopsWhenConditionalPutIsNoLongerAdmitted(t *testing.T) {
	f := newObjectsAPIFixture(t)
	created := objectsAPIJSON(t, f.request(t, "PUT", "/content", "/direct", []byte("old"), nil), 201)
	f.cfg.Storage.TransferClass = storage.TransferPresignedParts
	factory := &directRoutingFactory{inner: f.backend, conditionalPut: true}
	f.server.SetStorageFactory(factory)

	body, _ := json.Marshal(map[string]any{
		"path": "/direct", "size": 3, "mediaType": "text/plain",
		"condition": map[string]any{"mode": "replace", "contentRevision": created["contentRevision"]},
	})
	headers := http.Header{"Idempotency-Key": {"direct-replacement"}}
	resource := uploadsHTTPJSON(t, f.request(t, "POST", "/uploads", "", body, headers), 201)
	transport, ok := resource["transport"].(map[string]any)
	if !ok || transport["kind"] != "presigned-parts" {
		t.Fatalf("direct replacement transport = %v", resource["transport"])
	}
	part := transport["parts"].([]any)[0].(map[string]any)
	if part["method"] != "PUT" || !strings.HasSuffix(part["url"].(string), "/direct") {
		t.Fatalf("direct replacement grant = %v", part)
	}
	if headers, _ := part["headers"].(map[string]any); headers["If-Match"] == nil {
		t.Fatalf("direct replacement grant dropped its condition: %v", part)
	}
	if direct, _ := factory.signed(); direct != 1 {
		t.Fatalf("signed direct puts = %d, want 1", direct)
	}

	// The connection was re-measured and no longer admits the condition. Replaying the same
	// create must not answer with a fresh grant for the destination.
	factory.conditionalPut = false
	w := f.request(t, "POST", "/uploads", "", body, headers)
	uploadsHTTPProblem(t, w, 409, "direct_put_unavailable")
	if !strings.Contains(w.Body.String(), "multipart") {
		t.Fatalf("refusal does not name the route that would work: %s", w.Body.String())
	}
	if direct, _ := factory.signed(); direct != 1 {
		t.Fatal("a replacement grant was signed after its condition stopped being admitted")
	}
}
