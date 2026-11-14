package storagebackend

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
)

// measureBytes is the payload a speed test uploads and reads back. It is 8 MiB:
// large enough that the number is dominated by transfer rather than by connection
// setup, and small enough that a person testing a home NAS over a slow uplink
// waits seconds rather than minutes. The test is bounded by this byte count, not
// by a clock, so a slow link can complete it instead of being cut off.
const measureBytes = 8 << 20

// Measurement is what one reachability/latency/throughput test observed. Rates are
// bytes per second over the transfer phase only; latency is one metadata round
// trip, which is the number a person feels as "is it there?".
type Measurement struct {
	Latency      time.Duration
	Uploaded     int64
	Downloaded   int64
	UploadRate   int64
	DownloadRate int64
}

// Measure tests one object connection by writing and reading a known payload under
// the reserved probe prefix. It answers the three questions a person asks about a
// store they just attached — is it reachable, how far away is it, how fast is it —
// using the same native write and read path a backup uses, so a passing result is
// evidence about the path that matters rather than about a separate API.
//
// It deliberately does not consult admission evidence: measuring must work on a
// connection whose publishability is exactly what is still unproven. The caller
// runs ProbeConnection afterwards to establish that.
func Measure(ctx context.Context, cfg *config.Config) (Measurement, error) {
	var m Measurement
	if cfg == nil {
		return m, fmt.Errorf("storage configuration is required to measure a connection")
	}
	switch cfg.BackendName() {
	case "cos", "s3":
	default:
		return m, fmt.Errorf("storage connection %q uses backend %q: a speed test measures the object path, which only cos and s3 provide",
			cfg.Storage.ConnectionID, cfg.BackendName())
	}
	backend, closer, err := objectMutationBackend(cfg)
	if err != nil {
		return m, fmt.Errorf("storage connection %q: open the provider for measuring: %w", cfg.Storage.ConnectionID, err)
	}
	if closer != nil {
		defer closer.Close()
	}
	endpoint, bucket, _ := connectionIdentity(cfg)
	return measureBackend(ctx, cfg.Storage.ConnectionID, endpoint, bucket, backend)
}

// measureBackend is the half of Measure that touches a provider, split out so a test
// can time a fixture instead of a network.
func measureBackend(ctx context.Context, name, endpoint, bucket string, backend objectmutation.Backend) (Measurement, error) {
	var m Measurement
	suffix, err := randomProbeSuffix()
	if err != nil {
		return m, err
	}
	key := probeRoot + suffix + "/measure"

	// A metadata request for a key that does not exist costs one round trip and
	// proves the endpoint answers. It is the latency number, not an error.
	start := time.Now()
	if _, err := backend.Head(ctx, key); err != nil && !storage.IsCode(err, "not_found") {
		return m, fmt.Errorf("storage connection %q: reach %s bucket %q: %w", name, endpoint, bucket, err)
	}
	m.Latency = time.Since(start)

	payload := make([]byte, measureBytes)
	if _, err := rand.Read(payload); err != nil {
		return m, fmt.Errorf("storage connection %q: prepare the test payload: %w", name, err)
	}
	start = time.Now()
	object, err := backend.Put(ctx, objectmutation.Put{
		Key: key, Body: bytes.NewReader(payload), Size: int64(len(payload)),
		MediaType: "application/octet-stream", Absent: true,
	})
	if err != nil {
		return m, fmt.Errorf("storage connection %q: upload the test object %s: %w", name, key, err)
	}
	m.Uploaded = int64(len(payload))
	m.UploadRate = rate(m.Uploaded, time.Since(start))
	defer func() {
		if _, err := backend.Delete(context.Background(), key, object.ETag); err != nil {
			// The test itself passed; a leftover probe object is operator housekeeping,
			// and naming it is the only way anyone finds it again.
			slog.Warn("storage probe object was not cleaned up", "connection", name, "key", key, "error", err)
		}
	}()

	start = time.Now()
	body, err := backend.Open(ctx, key, object.ETag)
	if err != nil {
		return m, fmt.Errorf("storage connection %q: download the test object %s: %w", name, key, err)
	}
	read, err := io.Copy(io.Discard, body)
	closeErr := body.Close()
	if err != nil {
		return m, fmt.Errorf("storage connection %q: read the test object %s back: %w", name, key, err)
	}
	if closeErr != nil {
		return m, fmt.Errorf("storage connection %q: close the test object %s: %w", name, key, closeErr)
	}
	m.Downloaded = read
	m.DownloadRate = rate(read, time.Since(start))
	return m, nil
}

func rate(bytes int64, elapsed time.Duration) int64 {
	if elapsed <= 0 {
		return 0
	}
	return bytes * int64(time.Second) / int64(elapsed)
}
