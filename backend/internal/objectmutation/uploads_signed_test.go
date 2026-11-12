package objectmutation

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

type signedTestBackend struct {
	*nativeTestBackend
	signCalls, listCalls int
	signFail             bool
}

func (b *signedTestBackend) SignedPartsSupported() bool { return true }

func (b *signedTestBackend) DirectPostSupported() bool { return true }

func (b *signedTestBackend) SignPost(_ context.Context, key string, size int64, mediaType string, expires time.Time) (storage.SignedPartGrant, error) {
	b.signCalls++
	return storage.SignedPartGrant{
		Number: 1, Size: size, Method: "POST", URL: "https://bucket.example/",
		Form:      []storage.FormField{{Name: "key", Value: key}, {Name: "policy", Value: "policy"}, {Name: "q-signature", Value: "signature"}, {Name: "x-cos-forbid-overwrite", Value: "true"}, {Name: "Content-Type", Value: mediaType}},
		FileField: "file", Headers: map[string]string{}, ExpiresAt: expires,
	}, nil
}

func (b *signedTestBackend) SignPart(_ context.Context, key, id string, n int, size int64, expires time.Time) (storage.SignedPartGrant, error) {
	b.signCalls++
	if b.signFail {
		return storage.SignedPartGrant{}, storage.NewError("storage_failure", storage.Path(key), nil)
	}
	return storage.SignedPartGrant{Number: n, Size: size, Method: "PUT", URL: "https://bucket.example/" + key, Headers: map[string]string{"Content-Length": "4", "Content-Type": "application/octet-stream"}, ExpiresAt: expires}, nil
}

func (b *signedTestBackend) SignPut(_ context.Context, key string, size int64, mediaType string, cond storage.Conditions, expires time.Time) (storage.SignedPartGrant, error) {
	b.signCalls++
	if b.signFail {
		return storage.SignedPartGrant{}, storage.NewError("storage_failure", storage.Path(key), nil)
	}
	headers := map[string]string{"Content-Length": itoa(int(size)), "Content-Type": mediaType}
	if cond.Absent {
		headers["If-None-Match"] = "*"
	} else {
		headers["If-Match"] = string(cond.ExpectedRevision)
	}
	return storage.SignedPartGrant{Number: 1, Size: size, Method: "PUT", URL: "https://bucket.example/" + key, Headers: headers, ExpiresAt: expires}, nil
}

func (b *signedTestBackend) ListParts(_ context.Context, key, _ string, after, limit int) ([]storage.NativeUploadPart, bool, error) {
	b.listCalls++
	parts := make([]storage.NativeUploadPart, 0, len(b.parts))
	for n := after + 1; n <= len(b.parts); n++ {
		data, ok := b.parts[n]
		if !ok {
			return nil, false, nil
		}
		parts = append(parts, storage.NativeUploadPart{Number: n, Size: int64(len(data)), ETag: "tag-" + itoa(n)})
		if len(parts) >= limit {
			return parts, n < len(b.parts), nil
		}
	}
	return parts, false, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func signedFixture(t *testing.T) (*Driver, *signedTestBackend, *memoryUploadMetadata) {
	_, base, binding, options := nativeFixture(t)
	backend := &signedTestBackend{nativeTestBackend: base}
	driver, err := New(&fakeTree{backend: base.fakeBackend, root: binding.ContentRoot}, binding, backend, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { driver.Close() })
	return driver, backend, &memoryUploadMetadata{}
}

func TestPlannedUploadSignsWithoutReceivingOrDigesting(t *testing.T) {
	d, b, metadata := signedFixture(t)
	ctx := context.Background()
	in := nativeIntent(t, d, "planned", 4)
	expires := time.Now().Add(10 * time.Minute)
	plan, err := d.BeginPlannedUpload(ctx, in, metadata, expires)
	if err != nil || len(plan.Parts) != 1 || plan.Parts[0].Size != 4 || len(plan.Parts[0].Headers) != 2 {
		t.Fatal(plan, err)
	}
	if b.partCalls != 0 || b.puts != 0 {
		t.Fatal("class A plan moved bytes")
	}
	// Pretend the provider holds the part; commit seals the provider listing.
	b.parts = map[int][]byte{1: []byte("data")}
	stage, err := d.SealPlannedUpload(ctx, in, metadata, cpOK)
	if err != nil {
		t.Fatal(err)
	}
	result, err := d.Publish(ctx, stage, cpOK)
	if err != nil || result.Entry.Size != 4 {
		t.Fatal(result, err)
	}
	if b.partCalls != 0 || b.puts != 0 || b.completeCalls != 1 {
		t.Fatal("application relayed bytes", b.partCalls, b.puts, b.completeCalls)
	}
}

func TestPlannedUploadRejectsIncompleteProviderListing(t *testing.T) {
	d, b, metadata := signedFixture(t)
	ctx := context.Background()
	in := nativeIntent(t, d, "incomplete", 4)
	if _, err := d.BeginPlannedUpload(ctx, in, metadata, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	b.parts = map[int][]byte{1: []byte("dat")}
	_, err := d.SealPlannedUpload(ctx, in, metadata, cpOK)
	requireCode(t, err, "upload_incomplete")
	if b.completeCalls != 0 {
		t.Fatal("incomplete listing published")
	}
}

func TestPlannedUploadRenewalKeepsBoundLengths(t *testing.T) {
	d, b, metadata := signedFixture(t)
	ctx := context.Background()
	in := nativeIntent(t, d, "renew", nativePartSize+4)
	if _, err := d.BeginPlannedUpload(ctx, in, metadata, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	grants, err := d.SignUploadParts(ctx, in, []int{1, 2}, metadata, time.Now().Add(10*time.Minute))
	if err != nil || len(grants) != 2 || grants[0].Size != nativePartSize || grants[1].Size != 4 {
		t.Fatal(grants, err)
	}
	_, err = d.SignUploadParts(ctx, in, []int{3}, metadata, time.Now().Add(10*time.Minute))
	requireCode(t, err, "invalid_request")
	_, err = d.SignUploadParts(ctx, in, []int{1, 1}, metadata, time.Now().Add(10*time.Minute))
	requireCode(t, err, "invalid_request")
	if b.signCalls < 3 {
		t.Fatal("renewal did not sign the same bound lengths")
	}
}

func TestPlannedUploadRequiresProviderEvidence(t *testing.T) {
	d, _, _ := signedFixture(t)
	d.options.Admission.MultipartUploads = false
	if d.SignedUploadSupported() {
		t.Fatal("missing native multipart evidence enabled class A")
	}
	_, err := d.BeginPlannedUpload(context.Background(), nativeIntent(t, d, "unproved", 4), &memoryUploadMetadata{}, time.Now().Add(time.Minute))
	requireCode(t, err, "unsupported")
}

// A direct session signs the destination itself: one conditional PUT, no multipart
// allocation, and the condition travels in the signed headers so a client cannot drop it.
func TestBeginDirectPutSignsOneConditionalDestinationPut(t *testing.T) {
	driver, backend, metadata := signedFixture(t)
	defer driver.Close()
	intent := storage.WriteIntent{ID: "own-1", Target: "folder/file.txt", Size: 4, MediaType: "text/plain", Conditions: storage.Conditions{Absent: true}}
	grant, err := driver.BeginDirectPut(context.Background(), intent, metadata, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if grant.Number != 1 || grant.Size != 4 || grant.URL == "" {
		t.Fatalf("unexpected grant: %+v", grant)
	}
	if grant.Headers["If-None-Match"] != "*" {
		t.Fatalf("create condition missing from the signed headers: %v", grant.Headers)
	}
	if backend.signCalls != 1 || backend.beginCalls != 0 {
		t.Fatalf("direct put touched the provider: signed=%d begins=%d", backend.signCalls, backend.beginCalls)
	}
}

// A form upload is the create path when the connection signs one: it has no conditional
// header, so the condition lives in the signed policy, and nothing is staged.
func TestBeginDirectPostSignsOneConditionalFormUpload(t *testing.T) {
	driver, backend, metadata := signedFixture(t)
	defer driver.Close()
	if !driver.SupportsDirectPost() {
		t.Fatal("a backend that signs form posts was not offered")
	}
	intent := storage.WriteIntent{ID: "own-1", Target: "folder/file.txt", Size: 4, MediaType: "text/plain", Conditions: storage.Conditions{Absent: true}}
	grant, err := driver.BeginDirectPost(context.Background(), intent, metadata, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if grant.Method != "POST" || grant.FileField != "file" || len(grant.Form) == 0 || len(grant.Headers) != 0 {
		t.Fatalf("unexpected form grant: %+v", grant)
	}
	if backend.signCalls != 1 || backend.beginCalls != 0 {
		t.Fatalf("form post touched the provider: signed=%d begins=%d", backend.signCalls, backend.beginCalls)
	}
	// A replacement has no form-post expression: it keeps the conditional PUT.
	replace := intent
	replace.Conditions = storage.Conditions{ExpectedRevision: []byte("native-etag:\"abc\"")}
	if _, err = driver.BeginDirectPost(context.Background(), replace, metadata, time.Now().Add(time.Minute)); err == nil {
		t.Fatal("a form upload cannot express a replacement")
	}
}

func TestBeginDirectPutRefusesWhatOnePutCannotPublish(t *testing.T) {
	driver, _, metadata := signedFixture(t)
	defer driver.Close()
	expires := time.Now().Add(time.Minute)
	for name, intent := range map[string]storage.WriteIntent{
		"multi part":                 {ID: "own-1", Target: "big", Size: 2*nativePartSize + 1, Conditions: storage.Conditions{Absent: true}},
		"no condition":               {ID: "own-1", Target: "file", Size: 4},
		"replace without a revision": {ID: "own-1", Target: "file", Size: 4, Conditions: storage.Conditions{}},
	} {
		if _, err := driver.BeginDirectPut(context.Background(), intent, metadata, expires); err == nil {
			t.Fatalf("%s: expected the direct path to refuse", name)
		}
	}
}

// A replacement's signed If-Match is the grant's only guard: nothing else compares a native
// revision before the client's write, and the grant outlives the session that signed it (up
// to an hour), so a superseded session can replace a newer file with old bytes. That makes
// it a measured capability rather than the object-driver default: a create is covered by
// the connection's conditional-create evidence (the overwrite switch refuses an occupied
// key), a replacement needs the connection to have refused a stale If-Match itself.
func TestDirectReplacementRequiresMeasuredConditionalPut(t *testing.T) {
	driver, _, metadata := signedFixture(t)
	defer driver.Close()
	ctx := context.Background()
	replacement := storage.WriteIntent{
		ID: "replace-1", Target: "folder/file.txt", Size: 4, MediaType: "text/plain",
		Conditions: storage.Conditions{ExpectedRevision: []byte(`native-etag:"abc"`)},
	}
	if driver.SupportsDirectPut(replacement.Conditions) {
		t.Fatal("a replacement was offered without conditional-PUT evidence")
	}
	if _, err := driver.BeginDirectPut(ctx, replacement, metadata, time.Now().Add(time.Minute)); !storage.IsCode(err, "unsupported") {
		t.Fatalf("signing a replacement without measured evidence = %v, want unsupported", err)
	}
	// The create condition is another measurement and another promise; it keeps direct
	// publication, because its request also carries the provider's overwrite switch.
	if !driver.SupportsDirectPut(storage.Conditions{Absent: true}) {
		t.Fatal("a create lost direct publication because replacement evidence is missing")
	}
	// The alternative route has to exist for the driver to choose it: the same replacement
	// plans and seals as an ordinary multipart session.
	if _, err := driver.BeginPlannedUpload(ctx, replacement, metadata, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatalf("class A cannot express the replacement as a multipart session: %v", err)
	}
	driver.options.Admission.ConditionalPut = true
	if !driver.SupportsDirectPut(replacement.Conditions) {
		t.Fatal("measured conditional PUT did not admit direct replacement")
	}
	if _, err := driver.BeginDirectPut(ctx, replacement, metadata, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("measured conditional PUT did not sign the replacement: %v", err)
	}
}

// Routing a replacement through multipart is what buys the safety: the parts stay
// unpublished, and the commit re-observes the destination and refuses a revision that moved
// before sending the one request that publishes. A provider that ignores PUT If-Match can
// therefore still not be published over a newer file by an older session.
func TestMultipartReplacementRefusesAChangedDestinationBeforePublishing(t *testing.T) {
	driver, backend, metadata := signedFixture(t)
	defer driver.Close()
	ctx := context.Background()
	publish := func(id, data string, cond storage.Conditions) storage.Entry {
		t.Helper()
		intent := storage.WriteIntent{ID: id, Target: "target", Size: int64(len(data)), MediaType: "application/octet-stream", Conditions: cond}
		stage, err := driver.Stage(ctx, intent, strings.NewReader(data), cpOK)
		if err != nil {
			t.Fatal(err)
		}
		result, err := driver.Publish(ctx, stage, cpOK)
		if err != nil {
			t.Fatal(err)
		}
		return *result.Entry
	}
	original := publish("original", "old!", storage.Conditions{Absent: true})
	replacement := storage.WriteIntent{ID: "replacement", Target: "target", Size: int64(len("newer")), MediaType: "application/octet-stream", Conditions: storage.Conditions{ExpectedRevision: original.Revision}}
	if _, err := driver.BeginPlannedUpload(ctx, replacement, metadata, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	backend.parts = map[int][]byte{1: []byte("newer")}
	sealed, err := driver.SealPlannedUpload(ctx, replacement, metadata, cpOK)
	if err != nil {
		t.Fatal(err)
	}
	// Somebody else replaces the destination after this session bound its revision: the
	// bytes a client would have uploaded are already at the provider, still unpublished.
	publish("interloper", "hijack", storage.Conditions{ExpectedRevision: original.Revision})
	completes := backend.completeCalls
	if _, err = driver.Publish(ctx, sealed, cpOK); !storage.IsCode(err, "changed") {
		t.Fatalf("stale multipart replacement = %v, want changed", err)
	}
	if backend.completeCalls != completes {
		t.Fatal("a stale multipart replacement sent its publishing request")
	}
	if data := mustOpen(t, driver, "target"); string(data) != "hijack" {
		t.Fatalf("newer destination bytes were overwritten: %q", data)
	}
}

func mustOpen(t *testing.T, driver *Driver, path storage.Path) []byte {
	t.Helper()
	read, err := driver.Open(context.Background(), path, storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer read.Body.Close()
	data, err := io.ReadAll(read.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
