package storagebackend

import (
	"context"
	"fmt"
	"io"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
)

// BrowserProbe is one measurement, taken where the person sits: this browser moves the
// bytes, then reads the same object back, along the path an upload of that size takes on
// this connection.
//
// It is the only speed test the service offers, and that is deliberate. A measurement run
// in this process answers "what does a backup cost", which is not a question about a
// person's connection: an upload from the page starts at the browser, and on a product
// that can be written to directly it never passes through this service at all. A number
// taken on the server would silently describe the wrong link — the one between the host and
// the provider — and the page would print it where a person expects their own speed.
//
// The transport follows the connection's class, which is the same rule a real upload
// follows: class A hands the browser a signed PUT to the provider, and everything else is
// relayed through this service (see Relay endpoint methods below). One test, two legs,
// because those are the two legs uploads actually have.
type BrowserProbe struct {
	// RunID names this measurement. The client returns it when it is done, and only the
	// server may delete the object afterwards.
	RunID string
	// Key is the probe object. It is never returned to the client.
	Key string
	// Bytes is what the client must send, exactly, so two runs are comparable rather than
	// merely both "fast".
	Bytes int64
	// Direct reports whether the client's bytes go to the provider itself (class A) or
	// through this service (class C). The two need different grants: a direct run needs
	// signed provider URLs and must not send cookies; a relayed run needs this service's
	// own URL and its session cookie.
	Direct bool
	// Upload is a conditional create for exactly Bytes at Key, present only for a direct
	// run. The condition travels in the signed headers — the client cannot drop it.
	Upload storage.SignedPartGrant
	// Download reads the same object back, present only for a direct run.
	Download storage.ReadGrant
}

// BrowserProbeExpiry bounds one run. A 100 MB upload from a home uplink can take minutes,
// and an expired URL mid-run would report a failure that is really a clock.
const BrowserProbeExpiry = 30 * time.Minute

// BrowserProbeBytes is the payload one run moves: 100 MB decimal, because that is the number a
// person reads next to the rate, and one conditional PUT is what a file of that size costs
// on a product that promises conditional writes. Reading it back is the download half.
const BrowserProbeBytes = 100_000_000

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

// GrantBrowserProbe decides the transport for one run and, where the product allows it,
// signs the provider URLs the browser will use.
//
// It takes the backend rather than building one so a test can sign against a fixture; the
// handler passes the real provider adapter. A product that cannot sign is not an error:
// every connection has a browser path, and on a relay that path runs through this service.
func GrantBrowserProbe(ctx context.Context, name string, class string, backend objectmutation.Backend, runID string, expires time.Time) (BrowserProbe, error) {
	key, err := BrowserProbeKey(runID)
	if err != nil {
		return BrowserProbe{}, err
	}
	probe := BrowserProbe{RunID: runID, Key: key, Bytes: BrowserProbeBytes}
	if class != storage.TransferPresignedParts {
		// Class C: no provider URL exists to hand a browser, so the browser talks to this
		// service and the service talks to the store — the same two legs a real upload
		// takes on this product.
		return probe, nil
	}
	if backend == nil {
		return probe, fmt.Errorf("storage connection %q is class A but has no object backend to sign against: %w", name, storage.NewError("invalid_config", "", nil))
	}
	signer, ok := backend.(objectmutation.DirectPutBackend)
	if !ok {
		// A class A product whose driver cannot sign is a contradiction in the product
		// table, not a property of this connection: fall back to the relay leg rather than
		// fail the test for a reason the person cannot act on.
		return probe, nil
	}
	readSigner, ok := backend.(objectmutation.ReadSignerBackend)
	if !ok || !readSigner.ReadSigningSupported() {
		return probe, nil
	}
	upload, err := signer.SignPut(ctx, key, BrowserProbeBytes, "application/octet-stream", storage.Conditions{Absent: true}, expires)
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
	probe.Direct = true
	probe.Upload, probe.Download = upload, download
	return probe, nil
}

// WriteRelayedBrowserProbe streams one relayed run's bytes into the store, exactly where a
// relayed upload would put them: this service receives the body and writes it to the
// provider. It refuses a body that is not exactly size bytes, because a short or long
// upload would publish a rate for work that never happened.
func WriteRelayedBrowserProbe(ctx context.Context, name string, backend objectmutation.Backend, key string, body io.Reader, size int64) error {
	if backend == nil {
		return fmt.Errorf("storage connection %q has no object backend to relay a browser probe into: %w", name, storage.NewError("invalid_config", "", nil))
	}
	if _, err := backend.Put(ctx, objectmutation.Put{
		Key: key, Body: body, Size: size, MediaType: "application/octet-stream", Absent: true,
	}); err != nil {
		return fmt.Errorf("storage connection %q: store the browser probe upload: %w", name, err)
	}
	return nil
}

// ReadRelayedBrowserProbe opens the object the relayed write created. The read is bound to
// the current revision, like every other read of this tree, so the bytes returned are the
// ones this run wrote.
func ReadRelayedBrowserProbe(ctx context.Context, name string, backend objectmutation.Backend, key string) (io.ReadCloser, int64, error) {
	if backend == nil {
		return nil, 0, fmt.Errorf("storage connection %q has no object backend to relay a browser probe out of: %w", name, storage.NewError("invalid_config", "", nil))
	}
	object, err := backend.Head(ctx, key)
	if err != nil {
		return nil, 0, fmt.Errorf("storage connection %q: locate the browser probe object: %w", name, err)
	}
	reader, err := backend.Open(ctx, key, object.ETag)
	if err != nil {
		return nil, 0, fmt.Errorf("storage connection %q: open the browser probe object: %w", name, err)
	}
	return reader, object.Size, nil
}

// DeleteBrowserProbe removes one run's object. The browser never holds a delete grant: a
// grant that could remove an object is a grant that could remove the wrong one, and the
// object lives in a namespace only this service should touch.
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
