package objectmutation

import (
	"context"
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

func (b *signedTestBackend) SignPart(_ context.Context, key, id string, n int, size int64, expires time.Time) (storage.SignedPartGrant, error) {
	b.signCalls++
	if b.signFail {
		return storage.SignedPartGrant{}, storage.NewError("storage_failure", storage.Path(key), nil)
	}
	return storage.SignedPartGrant{Number: n, Size: size, Method: "PUT", URL: "https://bucket.example/" + key, Headers: map[string]string{"Content-Length": "4", "Content-Type": "application/octet-stream"}, ExpiresAt: expires}, nil
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
