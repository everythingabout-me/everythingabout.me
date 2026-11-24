package storagebackend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
)

// signingFixture is a provider adapter that can sign a direct PUT and a direct read, which
// is exactly what a class A product must be able to do.
type signingFixture struct {
	objectmutation.Backend
	puts     []storage.SignedPartGrant
	absent   []bool
	readKeys []string
}

func (f *signingFixture) SignPut(_ context.Context, key string, size int64, mediaType string, cond storage.Conditions, expires time.Time) (storage.SignedPartGrant, error) {
	f.puts = append(f.puts, storage.SignedPartGrant{Number: 1, Size: size, Method: "PUT", URL: "https://provider.test/" + key, Headers: map[string]string{"Content-Type": mediaType}, ExpiresAt: expires})
	f.absent = append(f.absent, cond.Absent)
	return f.puts[len(f.puts)-1], nil
}

func (f *signingFixture) ReadSigningSupported() bool { return true }

func (f *signingFixture) PresignRead(_ context.Context, key string, options storage.ReadGrantOptions) (storage.ReadGrant, error) {
	f.readKeys = append(f.readKeys, key)
	return storage.ReadGrant{Kind: "url", URL: "https://provider.test/" + key, ExpiresAt: options.ExpiresAt}, nil
}

// A plain adapter, like the S3 driver today: it can move bytes but cannot sign a URL.
type opaqueFixture struct{ objectmutation.Backend }

func TestBrowserProbeSignsTheSameWorkARealUploadSends(t *testing.T) {
	fixture := &signingFixture{}
	probe, err := GrantBrowserProbe(context.Background(), "cos", storage.TransferPresignedParts, fixture, "bp_abc", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if probe.Bytes != BrowserProbeBytes {
		t.Fatalf("browser probe sends %d bytes, a real upload of that size sends %d: the two numbers must be comparable", probe.Bytes, BrowserProbeBytes)
	}
	if !strings.HasPrefix(probe.Key, probeRoot) || strings.Contains(probe.Key, "..") {
		t.Fatalf("probe key %q is outside the reserved probe prefix", probe.Key)
	}
	if len(fixture.puts) != 1 || fixture.puts[0].Size != BrowserProbeBytes {
		t.Fatalf("signed put is not a create of the measured size: %+v", fixture.puts)
	}
	if len(fixture.absent) != 1 || !fixture.absent[0] {
		t.Fatal("the probe upload is not a conditional create, so a stale grant could overwrite")
	}
	if len(fixture.readKeys) != 1 || fixture.readKeys[0] != probe.Key {
		t.Fatalf("read grant does not name the probe object: %+v", fixture.readKeys)
	}
}

// A relayed connection has no provider URL to hand a browser, so the run is still one
// browser measurement, taken through this service instead. Refusing would leave the
// person with no test at all on exactly the connections whose speed they can change.
func TestBrowserProbeRelaysARelayConnection(t *testing.T) {
	probe, err := GrantBrowserProbe(context.Background(), "local", storage.TransferRelay, &signingFixture{}, "bp_abc", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if probe.Direct {
		t.Fatal("a relay connection was handed a direct provider transfer")
	}
	if probe.Key == "" || probe.Bytes != BrowserProbeBytes {
		t.Fatalf("relayed probe does not describe the same object and size: %+v", probe)
	}
}

// The S3-family driver cannot sign in this build, so a class A product served by it takes
// the relayed leg rather than being handed a URL that does not exist.
func TestBrowserProbeRelaysADriverThatCannotSign(t *testing.T) {
	probe, err := GrantBrowserProbe(context.Background(), "nas", storage.TransferPresignedParts, &opaqueFixture{}, "bp_abc", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if probe.Direct || probe.Upload.URL != "" {
		t.Fatalf("a driver without a signer was handed a direct transfer: %+v", probe)
	}
}

func TestBrowserProbeKeyRejectsAForeignRunID(t *testing.T) {
	for _, run := range []string{"", "bp/../x", "bp_ABC", "bp_abc def"} {
		if _, err := BrowserProbeKey(run); err == nil {
			t.Errorf("run id %q was accepted as a probe object name", run)
		}
	}
}

// storeFixture is an object backend that holds one object, so the relayed legs can be
// checked without a provider: what matters here is the size and condition this service
// sends, and the revision it binds the read to.
type storeFixture struct {
	objectmutation.Backend
	body   []byte
	etag   string
	put    *objectmutation.Put
	opened string
}

func (f *storeFixture) Put(_ context.Context, in objectmutation.Put) (objectmutation.Object, error) {
	f.put = &in
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return objectmutation.Object{}, err
	}
	if int64(len(body)) != in.Size {
		return objectmutation.Object{}, fmt.Errorf("body is %d bytes, the put declared %d", len(body), in.Size)
	}
	f.body, f.etag = body, "etag-1"
	return objectmutation.Object{Key: in.Key, Size: in.Size, ETag: f.etag}, nil
}

func (f *storeFixture) Head(_ context.Context, key string) (objectmutation.Object, error) {
	if f.body == nil {
		return objectmutation.Object{}, storage.NewError("not_found", storage.Path(key), nil)
	}
	return objectmutation.Object{Key: key, Size: int64(len(f.body)), ETag: f.etag}, nil
}

func (f *storeFixture) Open(_ context.Context, key, etag string) (io.ReadCloser, error) {
	if etag != f.etag {
		return nil, storage.NewError("changed", storage.Path(key), nil)
	}
	f.opened = etag
	return io.NopCloser(bytes.NewReader(f.body)), nil
}

func (f *storeFixture) Delete(_ context.Context, key, etag string) (objectmutation.Object, error) {
	if etag != f.etag {
		return objectmutation.Object{}, storage.NewError("changed", storage.Path(key), nil)
	}
	f.body = nil
	return objectmutation.Object{Key: key}, nil
}

// A relayed run moves the measured bytes through this service in both directions: the write
// declares the exact length as a create, and the read is bound to the revision the write
// produced.
func TestRelayedBrowserProbeMovesTheMeasuredBytes(t *testing.T) {
	fixture := &storeFixture{}
	key, err := BrowserProbeKey("bp_relay")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRelayedBrowserProbe(context.Background(), "nas", fixture, key, bytes.NewReader(make([]byte, BrowserProbeBytes)), BrowserProbeBytes); err != nil {
		t.Fatal(err)
	}
	if fixture.put == nil || fixture.put.Size != BrowserProbeBytes || !fixture.put.Absent {
		t.Fatalf("the relayed write is not a create of the measured size: %+v", fixture.put)
	}
	reader, size, err := ReadRelayedBrowserProbe(context.Background(), "nas", fixture, key)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if size != BrowserProbeBytes || fixture.opened != "etag-1" {
		t.Fatalf("the relayed read returned %d bytes bound to %q", size, fixture.opened)
	}
	if err := DeleteBrowserProbe(context.Background(), "nas", fixture, key); err != nil {
		t.Fatal(err)
	}
	if fixture.body != nil {
		t.Fatal("the probe object survived its cleanup")
	}
}
