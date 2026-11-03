package objectmutation

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

type signedTestBackend struct {
	*nativeTestBackend
	nativeInfo            NativePartInfo
	exists                bool
	signCalls, queryCalls int
}

func (b *signedTestBackend) SignedPartsSupported() bool { return true }

func (b *signedTestBackend) SignPart(_ context.Context, key, id string, n int, size int64, md5 string, expires time.Time) (storage.SignedPartGrant, error) {
	b.signCalls++
	return storage.SignedPartGrant{Method: "PUT", URL: "https://bucket.example/" + key + "?partNumber=1&uploadId=" + id, Headers: map[string]string{"Content-MD5": md5, "Content-Type": "application/octet-stream"}, ExpiresAt: expires}, nil
}
func (b *signedTestBackend) QueryPart(context.Context, string, string, int) (NativePartInfo, bool, error) {
	b.queryCalls++
	return b.nativeInfo, b.exists, nil
}
func signedFixture(t *testing.T) (*Driver, *signedTestBackend, *memoryUploadMetadata) {
	_, base, binding, options := nativeFixture(t)
	options.Admission.SignedMultipartParts = true
	backend := &signedTestBackend{nativeTestBackend: base}
	driver, err := New(&fakeTree{backend: base.fakeBackend, root: binding.ContentRoot}, binding, backend, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { driver.Close() })
	return driver, backend, &memoryUploadMetadata{}
}
func signedInput(data []byte) storage.SignedUploadPart {
	sha := sha256.Sum256(data)
	m := md5.Sum(data)
	return storage.SignedUploadPart{Number: 1, Size: int64(len(data)), SHA256: hex.EncodeToString(sha[:]), MD5: base64.StdEncoding.EncodeToString(m[:])}
}
func TestSignedNativeGrantConfirmAndStableReplay(t *testing.T) {
	d, b, metadata := signedFixture(t)
	ctx := context.Background()
	data := []byte("data")
	in := nativeIntent(t, d, "signed", 4)
	p := signedInput(data)
	grant, err := d.AuthorizePart(ctx, in, p, metadata, time.Now().Add(5*time.Minute))
	if err != nil || grant.Method != "PUT" {
		t.Fatal(grant, err)
	}
	if b.partCalls != 0 {
		t.Fatal("authorization sent bytes")
	}
	_, err = d.ConfirmPart(ctx, in, 1, metadata, keepPart)
	requireCode(t, err, "part_pending")
	// Client claims are insufficient: ListParts must return the bound exact MD5 and size.
	b.exists = true
	b.nativeInfo = NativePartInfo{Size: 4, ETag: `"wrong"`}
	_, err = d.ConfirmPart(ctx, in, 1, metadata, keepPart)
	requireCode(t, err, "digest_mismatch")
	m := md5.Sum(data)
	b.nativeInfo.ETag = `"` + hex.EncodeToString(m[:]) + `"`
	part, err := d.ConfirmPart(ctx, in, 1, metadata, keepPart)
	if err != nil {
		t.Fatal(err)
	}
	oldHandle := part.Handle
	if _, err = d.AuthorizePart(ctx, in, p, metadata, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	replay, err := d.ConfirmPart(ctx, in, 1, metadata, keepPart)
	if err != nil || replay.Handle != oldHandle {
		t.Fatal("renewal changed immutable receipt", err)
	}
	changed := signedInput([]byte("evil"))
	_, err = d.AuthorizePart(ctx, in, changed, metadata, time.Now().Add(5*time.Minute))
	requireCode(t, err, "part_conflict")
	_, err = d.ReceivePart(ctx, in, storage.UploadPart{Number: 1, Size: 4}, bytes.NewReader(data), metadata, keepPart)
	requireCode(t, err, "part_conflict")
	// Pretend COS stored the bytes: commit must seal receipts without receiving them.
	b.parts = map[int][]byte{1: data}
	stage, err := d.SealStage(ctx, in, []storage.UploadPart{part}, metadata, cpOK)
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
	if err = d.Discard(ctx, in, result.Evidence); err != nil {
		t.Fatal(err)
	}
	if err = d.DiscardParts(ctx, in, metadata); err != nil {
		t.Fatal(err)
	}
}
func TestSignedNativeAbortRetainsIssuedAuthorityLiability(t *testing.T) {
	d, b, metadata := signedFixture(t)
	ctx := context.Background()
	in := nativeIntent(t, d, "aborted-signed", 4)
	if _, err := d.AuthorizePart(ctx, in, signedInput([]byte("data")), metadata, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := d.Discard(ctx, in, nil); err != nil {
		t.Fatal(err)
	}
	requireCode(t, d.DiscardParts(ctx, in, metadata), "recovery_required")
	_, err := d.AuthorizePart(ctx, in, signedInput([]byte("data")), metadata, time.Now().Add(5*time.Minute))
	requireCode(t, err, "upload_aborted")
	_, err = d.ConfirmPart(ctx, in, 1, metadata, keepPart)
	requireCode(t, err, "upload_aborted")
	if b.completeCalls != 0 || b.abortCalls != 1 {
		t.Fatal("issued authority was incorrectly published")
	}
}
func TestSignedNativePartsRequireSeparateAdmission(t *testing.T) {
	d, _, _ := signedFixture(t)
	d.options.Admission.SignedMultipartParts = false
	if d.SignedUploadSupported() {
		t.Fatal("missing proof enabled signing")
	}
	if d.UploadMaxFileBytes() == 0 {
		t.Fatal("signed proof disabled embedded native multipart")
	}
	_, err := d.AuthorizePart(context.Background(), nativeIntent(t, d, "unproved", 4), signedInput([]byte("data")), &memoryUploadMetadata{}, time.Now().Add(5*time.Minute))
	requireCode(t, err, "unsupported")
}

func TestSignedNativePartGeometryAndChecksumValidation(t *testing.T) {
	d, b, metadata := signedFixture(t)
	ctx := context.Background()
	in := nativeIntent(t, d, "invalid-signed", 4)
	for _, change := range []func(*storage.SignedUploadPart){
		func(p *storage.SignedUploadPart) { p.Size = 3 }, func(p *storage.SignedUploadPart) { p.Number = 2 },
		func(p *storage.SignedUploadPart) { p.MD5 = "invalid" }, func(p *storage.SignedUploadPart) { p.SHA256 = "AA" },
	} {
		p := signedInput([]byte("data"))
		change(&p)
		_, err := d.AuthorizePart(ctx, in, p, metadata, time.Now().Add(5*time.Minute))
		requireCode(t, err, "invalid_request")
	}
	if b.beginCalls != 0 || b.signCalls != 0 {
		t.Fatal("invalid digest issued native authority")
	}
}
