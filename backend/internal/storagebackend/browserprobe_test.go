package storagebackend

import (
	"context"
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

func TestBrowserProbeSignsTheSameWorkTheServerMeasures(t *testing.T) {
	fixture := &signingFixture{}
	probe, err := GrantBrowserProbe(context.Background(), "cos", storage.TransferPresignedParts, fixture, "bp_abc", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if probe.Bytes != measureBytes {
		t.Fatalf("browser probe sends %d bytes, the server measures %d: the two numbers would not be comparable", probe.Bytes, measureBytes)
	}
	if !strings.HasPrefix(probe.Key, probeRoot) || strings.Contains(probe.Key, "..") {
		t.Fatalf("probe key %q is outside the reserved probe prefix", probe.Key)
	}
	if len(fixture.puts) != 1 || fixture.puts[0].Size != measureBytes {
		t.Fatalf("signed put is not a create of the measured size: %+v", fixture.puts)
	}
	if len(fixture.absent) != 1 || !fixture.absent[0] {
		t.Fatal("the probe upload is not a conditional create, so a stale grant could overwrite")
	}
	if len(fixture.readKeys) != 1 || fixture.readKeys[0] != probe.Key {
		t.Fatalf("read grant does not name the probe object: %+v", fixture.readKeys)
	}
}

// A relay connection has no provider URL to hand a browser, and its bytes are what the
// server measurement already measures. Refusing here is what keeps the panel honest.
func TestBrowserProbeRefusesARelayConnection(t *testing.T) {
	_, err := GrantBrowserProbe(context.Background(), "local", storage.TransferRelay, &signingFixture{}, "bp_abc", time.Now().Add(time.Minute))
	if err == nil || !storage.IsCode(err, "unsupported") {
		t.Fatalf("a relay connection was offered a browser probe: %v", err)
	}
}

// The S3-family driver cannot sign in this build, so a product served by it must refuse
// rather than hand the browser a URL that does not exist.
func TestBrowserProbeRefusesADriverThatCannotSign(t *testing.T) {
	_, err := GrantBrowserProbe(context.Background(), "nas", storage.TransferPresignedParts, &opaqueFixture{}, "bp_abc", time.Now().Add(time.Minute))
	if err == nil || !storage.IsCode(err, "unsupported") {
		t.Fatalf("a driver without a signer was accepted: %v", err)
	}
}

func TestBrowserProbeKeyRejectsAForeignRunID(t *testing.T) {
	for _, run := range []string{"", "bp/../x", "bp_ABC", "bp_abc def"} {
		if _, err := BrowserProbeKey(run); err == nil {
			t.Errorf("run id %q was accepted as a probe object name", run)
		}
	}
}
