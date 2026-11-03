package api

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

// This fixture tests the shared HTTP auth/receipt boundary. COS signature and
// native state verification are exercised separately against the real COS SDK.
type signedAPIFactory struct {
	OwnerStorageFactory
	pending    bool
	grantCalls int
}
type signedAPIDriver struct {
	fileops.NativeActions
	factory *signedAPIFactory
}

func (f *signedAPIFactory) Open(ctx context.Context, owner string) (storage.Driver, error) {
	driver, e := f.OwnerStorageFactory.Open(ctx, owner)
	if e != nil {
		return nil, e
	}
	return &signedAPIDriver{NativeActions: driver.(fileops.NativeActions), factory: f}, nil
}
func (d *signedAPIDriver) SignedUploadSupported() bool { return true }
func (d *signedAPIDriver) AuthorizePart(_ context.Context, in storage.WriteIntent, p storage.SignedUploadPart, _ storage.UploadMetadata, expires time.Time) (storage.SignedPartGrant, error) {
	d.factory.grantCalls++
	return storage.SignedPartGrant{Method: "PUT", URL: "https://private-cos.example/drive/owner/%2B%252F%3F%23.bin?uploadId=private-upload&partNumber=" + strconv.Itoa(p.Number) + "&q-signature=restricted", Headers: map[string]string{"Content-MD5": p.MD5, "Content-Type": "application/octet-stream"}, ExpiresAt: expires}, nil
}
func (d *signedAPIDriver) ConfirmPart(ctx context.Context, in storage.WriteIntent, n int, _ storage.UploadMetadata, accept func(storage.UploadPart) (storage.UploadPart, error)) (storage.UploadPart, error) {
	if d.factory.pending {
		return storage.UploadPart{}, storage.NewError("part_pending", in.Target, nil)
	}
	sum := sha256.Sum256([]byte("data"))
	handle := `{"Signed":true,"MD5":"jXd/OF09/siBXSD3SWAm3A=="}`
	return accept(storage.UploadPart{Number: n, Size: 4, SHA256: hex.EncodeToString(sum[:]), Handle: handle})
}
func TestSignedUploadAPIPostgresControlAuthorizationAndReceipts(t *testing.T) {
	f := newFileWorkerFixture(t)
	h := newUploadsHTTPFixture(t, f)
	factory := &signedAPIFactory{OwnerStorageFactory: f.backend, pending: true}
	h.server.SetStorageFactory(factory)
	f.cfg.FileUploadIngress = "edgeone"
	id := h.newUpload(t, "/signed +%2F?#.bin", "signed-api", 4)
	status := uploadsHTTPJSON(t, h.request("GET", uploadRoute(id, ""), h.bearer, nil, nil), 200)
	if status["transport"] != "cos-signed" {
		t.Fatal("transport omitted", status)
	}
	sha := sha256.Sum256([]byte("data"))
	md := md5.Sum([]byte("data"))
	body, _ := json.Marshal(map[string]any{"size": 4, "sha256": hex.EncodeToString(sha[:]), "md5": base64.StdEncoding.EncodeToString(md[:])})
	authorize := uploadRoute(id, "/parts/1/authorize")
	// Cookie control operations still require the public Origin and CSRF token.
	uploadsHTTPProblem(t, h.request("POST", authorize, "cookie", body, http.Header{"Content-Type": {"application/json"}}), 403, "forbidden")
	grant := uploadsHTTPJSON(t, h.request("POST", authorize, h.bearer, body, http.Header{"Content-Type": {"application/json"}}), 200)
	target := grant["url"].(string)
	if !strings.HasPrefix(target, "/api/files/transfer/drive/owner/%2B%252F%3F%23.bin?") || strings.Contains(target, "private-cos.example") {
		t.Fatal("not same-origin exact escaped transfer", target)
	}
	if grant["method"] != "PUT" || grant["headers"].(map[string]any)["Content-MD5"] != base64.StdEncoding.EncodeToString(md[:]) {
		t.Fatal("invalid grant", grant)
	}
	// Upload response/ETag cannot acknowledge bytes: only server confirmation can.
	confirm := uploadRoute(id, "/parts/1/confirm")
	uploadsHTTPProblem(t, h.request("POST", confirm, h.bearer, nil, nil), 409, "part_pending")
	receipts := uploadsHTTPJSON(t, h.request("GET", uploadRoute(id, "/parts"), h.bearer, nil, nil), 200)
	if len(receipts["items"].([]any)) != 0 {
		t.Fatal("pending native bytes acknowledged")
	}
	factory.pending = false
	receipt := uploadsHTTPJSON(t, h.request("POST", confirm, h.bearer, nil, nil), 201)
	if receipt["checksumSource"] != "client-sha256-cos-md5" {
		t.Fatal("claimed server SHA256", receipt)
	}
	uploadsHTTPJSON(t, h.request("POST", confirm, h.bearer, nil, nil), 200)
	receipts = uploadsHTTPJSON(t, h.request("GET", uploadRoute(id, "/parts"), h.bearer, nil, nil), 200)
	if len(receipts["items"].([]any)) != 1 || receipts["items"].([]any)[0].(map[string]any)["checksumSource"] != "client-sha256-cos-md5" {
		t.Fatal(receipts)
	}
	uploadsHTTPProblem(t, h.request("POST", confirm, h.bearer, []byte(`{"etag":"client-authority"}`), http.Header{"Content-Type": {"application/json"}}), 400, "invalid_request")
	other, _ := f.token(t, h.root.OwnerID, "", true, true)
	uploadsHTTPProblem(t, h.request("POST", authorize, other, body, nil), 403, "forbidden")
	uploadsHTTPProblem(t, h.request("POST", uploadRoute(id, "/parts/2/authorize"), h.bearer, body, nil), 400, "invalid_request")
	h.request("DELETE", uploadRoute(id, ""), h.bearer, nil, nil)
	before := factory.grantCalls
	uploadsHTTPProblem(t, h.request("POST", authorize, h.bearer, body, nil), 410, "upload_aborted")
	if factory.grantCalls != before {
		t.Fatal("aborted session signed more authority")
	}
	// Embedded sessions do not expose the specialized control transport.
	f.cfg.FileUploadIngress = "embedded"
	embedded := h.newUpload(t, "/embedded", "embedded-api", 4)
	uploadsHTTPProblem(t, h.request("POST", uploadRoute(embedded, "/parts/1/authorize"), h.bearer, body, nil), 422, "unsupported_option")
}
