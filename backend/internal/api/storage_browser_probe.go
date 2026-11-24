package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/storagebackend"
)

// The browser half of the storage performance test.
//
// The server's own measurement (POST .../test) answers "what does a backup cost": it runs
// in this process, over this host's network, to the provider. A person uploading from the
// page does not use that path — on a class A connection the browser writes straight to the
// store — so the number they need is the one taken from where they are sitting. This
// endpoint signs exactly one conditional PUT and one read for one run, at the same length
// the server measures, and the page times that transfer itself. Nothing here is an
// application-side measurement, and the answer is never cached: it describes a client and a
// network, not the provider.

// browserProbeClaim is one run this process has signed. It lives in memory for the same
// reason the speed test's claim does: the grant it describes dies with the process, and the
// object it names is disposable.
type browserProbeClaim struct {
	ownerID      string
	connectionID string
	key          string
	at           time.Time
}

func (s *Server) claimBrowserProbe(claim browserProbeClaim) {
	s.browserProbeMu.Lock()
	defer s.browserProbeMu.Unlock()
	if s.browserProbes == nil {
		s.browserProbes = map[string]browserProbeClaim{}
	}
	s.browserProbes[claim.ownerID+":"+claim.connectionID+":"+claim.key] = claim
}

// takeBrowserProbe returns and forgets one run that this owner opened on this connection.
// The key is part of the identity so a caller cannot name another run's object.
func (s *Server) takeBrowserProbe(ownerID, connectionID, runID string) (browserProbeClaim, bool) {
	key, err := storagebackend.BrowserProbeKey(runID)
	if err != nil {
		return browserProbeClaim{}, false
	}
	id := ownerID + ":" + connectionID + ":" + key
	s.browserProbeMu.Lock()
	defer s.browserProbeMu.Unlock()
	claim, ok := s.browserProbes[id]
	if ok {
		delete(s.browserProbes, id)
	}
	return claim, ok
}

func newBrowserProbeRunID() (string, error) {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "bp_" + hex.EncodeToString(raw[:]), nil
}

func (s *Server) handleStorageBrowserProbeStart(w http.ResponseWriter, r *http.Request) error {
	if s.db == nil || s.conns == nil {
		return httpx.ServiceUnavailable("Storage settings are unavailable on this instance.")
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	target, ok, err := s.visibleConnection(r, user.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	if !ok {
		return httpx.NotFound("No such storage connection on this account.")
	}
	effective, err := target.ApplyEnvironment(*s.cfg)
	if err != nil {
		return httpx.BadRequest(err.Error())
	}
	runID, err := newBrowserProbeRunID()
	if err != nil {
		return err
	}
	backend, closer, err := storagebackend.OpenObjectBackend(&effective)
	if closer != nil {
		defer closer.Close()
	}
	if err != nil {
		return httpx.Conflict(fmt.Sprintf("This store cannot be written from a browser: %v", err))
	}
	probe, err := storagebackend.GrantBrowserProbe(r.Context(), target.Name, effective.Storage.TransferClass, backend, runID, time.Now().Add(storagebackend.BrowserProbeExpiry))
	switch {
	case err == nil:
	case storage.IsCode(err, "unsupported"):
		// Not a failure of the store: a relay connection has no provider URL to hand a
		// browser, and its bytes are exactly what the server-side measurement measures.
		return httpx.Conflict(err.Error())
	default:
		return err
	}
	s.claimBrowserProbe(browserProbeClaim{ownerID: user.ID, connectionID: target.ID, key: probe.Key, at: time.Now()})
	return s.ok(w, map[string]any{
		"runId": probe.RunID,
		"bytes": probe.Bytes,
		"upload": map[string]any{
			"url":       probe.Upload.URL,
			"method":    probe.Upload.Method,
			"headers":   probe.Upload.Headers,
			"expiresAt": probe.Upload.ExpiresAt.UTC().Format(time.RFC3339),
		},
		"download": map[string]any{
			"url":       probe.Download.URL,
			"headers":   probe.Download.Headers,
			"expiresAt": probe.Download.ExpiresAt.UTC().Format(time.RFC3339),
		},
	})
}

func (s *Server) handleStorageBrowserProbeDelete(w http.ResponseWriter, r *http.Request) error {
	if s.db == nil || s.conns == nil {
		return httpx.ServiceUnavailable("Storage settings are unavailable on this instance.")
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	target, ok, err := s.visibleConnection(r, user.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	if !ok {
		return httpx.NotFound("No such storage connection on this account.")
	}
	claim, ok := s.takeBrowserProbe(user.ID, target.ID, r.PathValue("runId"))
	if !ok {
		// The object is disposable and already swept, or the run was never this owner's.
		return s.ok(w, map[string]any{"removed": false})
	}
	effective, err := target.ApplyEnvironment(*s.cfg)
	if err != nil {
		return httpx.BadRequest(err.Error())
	}
	backend, closer, err := storagebackend.OpenObjectBackend(&effective)
	if closer != nil {
		defer closer.Close()
	}
	if err != nil {
		return err
	}
	// Cleanup uses a background context: the answer to the client must not depend on it,
	// and a client that disconnects must not leave the probe object behind.
	if err := storagebackend.DeleteBrowserProbe(context.Background(), target.Name, backend, claim.key); err != nil {
		return err
	}
	return s.ok(w, map[string]any{"removed": true})
}
