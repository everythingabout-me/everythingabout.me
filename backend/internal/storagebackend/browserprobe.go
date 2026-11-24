package storagebackend

import (
	"context"
	"fmt"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
)

// BrowserProbe is one measurement the browser runs itself: the same object, the same
// length and the same provider as a real class A upload, signed by this service for one
// run.
//
// It exists because a server-side measurement answers a different question than the one a
// person asks. The backup and the index work from the server, but an upload from the page
// goes straight from the browser to the store, over the browser's network — and that is the
// path whose speed the person actually feels. Measuring it from the server would silently
// answer for the wrong link.
type BrowserProbe struct {
	// RunID names this measurement. The client returns it when it is done, and only the
	// server may delete the object afterwards: the grant covers a PUT on one key, nothing
	// else.
	RunID string
	// Key is the probe object. It is never returned to the client.
	Key string
	// Bytes is what the client must send, exactly: the server's own measurement sends the
	// same amount, so the two numbers are comparable rather than merely both "fast".
	Bytes int64
	// Upload is a conditional create for exactly Bytes at Key. The condition travels in the
	// signed headers — the client cannot drop it.
	Upload storage.SignedPartGrant
	// Download reads the same object back. The client times the first byte (the round trip)
	// and then the body (the throughput).
	Download storage.ReadGrant
}

// BrowserProbeExpiry bounds one run. A 100 MB upload from a home uplink can take minutes,
// and an expired URL mid-run would report a failure that is really a clock.
const BrowserProbeExpiry = 30 * time.Minute

// BrowserProbeKey is the only key a browser probe may touch.
func BrowserProbeKey(runID string) (string, error) {
	if runID == "" || len(runID) > 64 {
		return "", fmt.Errorf("browser probe: a run id is required to name its object")
	}
	for _, r := range runID {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return "", fmt.Errorf("browser probe: run id %q has a character this service never issues", runID)
		}
	}
	return probeRoot + "browser-" + runID + "/measure", nil
}

// GrantBrowserProbe signs one run against a connection. It refuses a connection whose class
// is not A, because there is no provider URL a browser could be given on a relay: those
// bytes go through this service, and the server-side measurement is already that path.
//
// It takes the backend rather than building one so a test can sign against a fixture; the
// handler passes the real provider adapter.
func GrantBrowserProbe(ctx context.Context, name string, class string, backend objectmutation.Backend, runID string, expires time.Time) (BrowserProbe, error) {
	var probe BrowserProbe
	if backend == nil {
		return probe, fmt.Errorf("storage connection %q: no object backend to sign a browser probe: %w", name, storage.NewError("invalid_config", "", nil))
	}
	if class != storage.TransferPresignedParts {
		return probe, fmt.Errorf("storage connection %q serves class %q, so a browser upload is relayed through this service and the server-side measurement already describes that path: %w", name, class, storage.NewError("unsupported", "", nil))
	}
	signer, ok := backend.(objectmutation.DirectPutBackend)
	if !ok {
		return probe, fmt.Errorf("storage connection %q: this product's driver cannot sign a direct PUT, so the browser cannot write to the store itself: %w", name, storage.NewError("unsupported", "", nil))
	}
	readSigner, ok := backend.(objectmutation.ReadSignerBackend)
	if !ok || !readSigner.ReadSigningSupported() {
		return probe, fmt.Errorf("storage connection %q: this product's driver cannot sign a direct read: %w", name, storage.NewError("unsupported", "", nil))
	}
	key, err := BrowserProbeKey(runID)
	if err != nil {
		return probe, err
	}
	upload, err := signer.SignPut(ctx, key, measureBytes, "application/octet-stream", storage.Conditions{Absent: true}, expires)
	if err != nil {
		return probe, fmt.Errorf("storage connection %q: sign the browser probe upload: %w", name, err)
	}
	download, err := readSigner.PresignRead(ctx, key, storage.ReadGrantOptions{
		MediaType:   "application/octet-stream",
		Disposition: "attachment",
		ExpiresAt:   expires,
	})
	if err != nil {
		return probe, fmt.Errorf("storage connection %q: sign the browser probe download: %w", name, err)
	}
	return BrowserProbe{RunID: runID, Key: key, Bytes: measureBytes, Upload: upload, Download: download}, nil
}

// DeleteBrowserProbe removes one run's object. The browser never holds a delete grant: a
// grant that could remove an object is a grant that could remove the wrong one, and the
// object lives in a namespace only this service should be able to touch.
func DeleteBrowserProbe(ctx context.Context, name string, backend objectmutation.Backend, key string) error {
	if backend == nil {
		return fmt.Errorf("storage connection %q: no object backend to clean up a browser probe: %w", name, storage.NewError("invalid_config", "", nil))
	}
	object, err := backend.Head(ctx, key)
	if err != nil {
		if storage.IsCode(err, "not_found") {
			return nil
		}
		return fmt.Errorf("storage connection %q: locate the browser probe object: %w", name, err)
	}
	if _, err := backend.Delete(ctx, key, object.ETag); err != nil {
		return fmt.Errorf("storage connection %q: delete the browser probe object: %w", name, err)
	}
	return nil
}
