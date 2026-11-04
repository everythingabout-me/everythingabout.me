package s3storage

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
)

// Once the first transport buffer is full, more bytes require the provider to
// have received data. A signing pre-read or whole-part buffer cannot finish.
type gatedUploadReader struct {
	ctx       context.Context
	started   <-chan struct{}
	read      int64
	remaining int64
}

func (r *gatedUploadReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if r.read >= 32<<10 {
		select {
		case <-r.started:
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
	n := min(int64(len(p)), r.remaining, 32<<10)
	clear(p[:n])
	r.remaining -= n
	r.read += n
	return int(n), nil
}

func TestMutationUploadPartStreamsNonSeekableBody(t *testing.T) {
	const size int64 = 64 << 20
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	var calls atomic.Int32
	var received atomic.Int64
	_, backend, _ := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "PUT" || r.URL.Path != "/test-bucket/drive/alice/stream.bin" || r.URL.Query().Get("uploadId") != "native-upload" || r.URL.Query().Get("partNumber") != "2" {
			t.Errorf("unexpected upload request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("X-Amz-Content-Sha256") != "UNSIGNED-PAYLOAD" || r.Header.Get("Authorization") == "" || r.Header.Get("X-Amz-Security-Token") != "fixture-token" {
			t.Errorf("streaming request lost signing: %v", r.Header)
		}
		if r.ContentLength != size || len(r.TransferEncoding) != 0 {
			t.Errorf("streaming request lost exact length: %d %v", r.ContentLength, r.TransferEncoding)
		}
		digest := sha256.New()
		if _, err := io.CopyN(digest, r.Body, 1); err != nil {
			t.Error(err)
			return
		}
		close(started)
		n, err := io.Copy(digest, r.Body)
		received.Store(n + 1)
		if err != nil || n+1 != size {
			t.Errorf("stream truncated: bytes=%d err=%v", n+1, err)
		}
		expected := sha256.New()
		zeros := &gatedUploadReader{ctx: ctx, started: started, remaining: size}
		if _, err := io.Copy(expected, zeros); err != nil {
			t.Error(err)
		}
		if string(digest.Sum(nil)) != string(expected.Sum(nil)) {
			t.Error("streamed bytes changed")
		}
		w.Header().Set("ETag", `"native-part-tag"`)
	}))
	body := &gatedUploadReader{ctx: ctx, started: started, remaining: size}
	tag, err := backend.(objectmutation.MultipartBackend).PutPart(ctx, "drive/alice/stream.bin", "native-upload", 2, body, size)
	if err != nil {
		t.Fatalf("non-seekable UploadPart failed: %v", err)
	}
	if tag != `"native-part-tag"` || calls.Load() != 1 || received.Load() != size || body.read != size || body.remaining != 0 {
		t.Fatalf("incorrect single-pass upload: tag=%q calls=%d received=%d read=%d remaining=%d", tag, calls.Load(), received.Load(), body.read, body.remaining)
	}
}
