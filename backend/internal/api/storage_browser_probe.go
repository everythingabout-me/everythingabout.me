package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"everythingabout.me/backend/internal/connections"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storagebackend"
)

// The storage speed test. There is one of them, and it runs in the browser.
//
// A measurement taken in this process measures this host: the link between the server and
// the provider, which is the path a backup takes and not the path a person uploading from
// the page takes. On a product that can be written to directly the browser writes straight
// to the store; on the others it writes to this service, which relays. Both legs start at
// the browser, so the number the page prints has to be taken there, on the connection that
// is actually between the person and their store.
//
// The service still decides where those bytes go, because that is the connection's class:
// a class A connection gets a signed PUT and read (Direct), and everything else gets the
// two endpoints below, which relay the bytes through the same provider calls a real upload
// makes. The person sees one test either way.

// browserProbeClaim is one run this process has signed. It lives in memory for the same
// reason the signed URL does: the run dies with the process, and the object it names is
// disposable.
type browserProbeClaim struct {
	ownerID      string
	connectionID string
	key          string
	at           time.Time
}

func browserProbeClaimID(ownerID, connectionID, key string) string {
	return ownerID + ":" + connectionID + ":" + key
}

func (s *Server) claimBrowserProbe(claim browserProbeClaim) {
	s.browserProbeMu.Lock()
	defer s.browserProbeMu.Unlock()
	if s.browserProbes == nil {
		s.browserProbes = map[string]browserProbeClaim{}
	}
	// Expired runs are dropped here rather than swept on a timer: a claim is small, tests are
	// rare, and the only thing that must be true is that the map cannot grow without bound.
	for id, running := range s.browserProbes {
		if time.Since(running.at) > storagebackend.BrowserProbeExpiry {
			delete(s.browserProbes, id)
		}
	}
	s.browserProbes[browserProbeClaimID(claim.ownerID, claim.connectionID, claim.key)] = claim
}

// browserProbe finds one run this owner opened on this connection, without consuming it.
// The relayed legs are two requests (a PUT and a GET) over one run, so the claim has to
// survive both; DELETE is what ends it.
func (s *Server) browserProbe(ownerID, connectionID, runID string) (browserProbeClaim, bool) {
	key, err := storagebackend.BrowserProbeKey(runID)
	if err != nil {
		return browserProbeClaim{}, false
	}
	s.browserProbeMu.Lock()
	defer s.browserProbeMu.Unlock()
	claim, ok := s.browserProbes[browserProbeClaimID(ownerID, connectionID, key)]
	if !ok || time.Since(claim.at) > storagebackend.BrowserProbeExpiry {
		return browserProbeClaim{}, false
	}
	return claim, true
}

// takeBrowserProbe returns and forgets one run that this owner opened on this connection.
// The key is part of the identity so a caller cannot name another run's object.
func (s *Server) takeBrowserProbe(ownerID, connectionID, runID string) (browserProbeClaim, bool) {
	claim, ok := s.browserProbe(ownerID, connectionID, runID)
	if !ok {
		return browserProbeClaim{}, false
	}
	s.browserProbeMu.Lock()
	delete(s.browserProbes, browserProbeClaimID(claim.ownerID, claim.connectionID, claim.key))
	s.browserProbeMu.Unlock()
	return claim, true
}

func newBrowserProbeRunID() (string, error) {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "bp_" + hex.EncodeToString(raw[:]), nil
}

// browserProbeRelayPath is where a relayed run's object lives: on this service, behind the
// session that started the run.
func browserProbeRelayPath(connectionID, runID string) string {
	return "/api/storage/connections/" + connectionID + "/browser-probe/" + runID + "/object"
}

// browserProbeLeg is one half of a run as the page sees it: where to send or fetch the
// bytes, how, and whether that URL belongs to this service (and therefore needs the
// session cookie) or to the provider (and therefore must not carry one).
type browserProbeLeg struct {
	URL         string            `json:"url"`
	Method      string            `json:"method,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Credentials string            `json:"credentials"`
	ExpiresAt   string            `json:"expiresAt"`
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
		// A local root has no object backend and no provider hop: its bytes are written by
		// this service, so there is no store-side transfer to time.
		return httpx.Conflict(fmt.Sprintf("This store cannot be reached as an object store, so it has no speed to measure: %v", err))
	}
	expires := time.Now().Add(storagebackend.BrowserProbeExpiry)
	probe, err := storagebackend.GrantBrowserProbe(r.Context(), target.Name, effective.Storage.TransferClass, backend, runID, expires)
	if err != nil {
		return err
	}
	s.claimBrowserProbe(browserProbeClaim{ownerID: user.ID, connectionID: target.ID, key: probe.Key, at: time.Now()})
	expiresAt := expires.UTC().Format(time.RFC3339)
	var upload, download browserProbeLeg
	if probe.Direct {
		// The browser talks to the provider itself, so the signed terms travel as headers and
		// the request carries no cookie: a cross-origin fetch with credentials would be
		// rejected by the provider's CORS policy, and this service's session is not the
		// provider's business.
		upload = browserProbeLeg{URL: probe.Upload.URL, Method: probe.Upload.Method, Headers: probe.Upload.Headers, Credentials: "omit", ExpiresAt: expiresAt}
		download = browserProbeLeg{URL: probe.Download.URL, Headers: probe.Download.Headers, Credentials: "omit", ExpiresAt: expiresAt}
	} else {
		// Relayed: the browser sends its bytes to this service, which writes them to the
		// store — the same leg order a real upload takes on this product.
		path := browserProbeRelayPath(target.ID, probe.RunID)
		upload = browserProbeLeg{URL: path, Method: http.MethodPut, Headers: map[string]string{"Content-Type": "application/octet-stream"}, Credentials: "include", ExpiresAt: expiresAt}
		download = browserProbeLeg{URL: path, Credentials: "include", ExpiresAt: expiresAt}
	}
	return s.ok(w, map[string]any{
		"runId":    probe.RunID,
		"bytes":    probe.Bytes,
		"direct":   probe.Direct,
		"upload":   upload,
		"download": download,
	})
}

// handleStorageBrowserProbePut receives the upload half of a relayed run and writes it to
// the store, exactly as a relayed upload would.
func (s *Server) handleStorageBrowserProbePut(w http.ResponseWriter, r *http.Request) error {
	target, claim, backend, closer, err := s.browserProbeTarget(r)
	if closer != nil {
		defer closer.Close()
	}
	if err != nil {
		return err
	}
	if r.ContentLength >= 0 && r.ContentLength != storagebackend.BrowserProbeBytes {
		return httpx.BadRequest(fmt.Sprintf("The speed test sends exactly %d bytes; this request announced %d.", storagebackend.BrowserProbeBytes, r.ContentLength))
	}
	body := &countingReader{reader: http.MaxBytesReader(w, r.Body, storagebackend.BrowserProbeBytes)}
	if err := storagebackend.WriteRelayedBrowserProbe(r.Context(), target.Name, backend, claim.key, body, storagebackend.BrowserProbeBytes); err != nil {
		return err
	}
	if body.read != storagebackend.BrowserProbeBytes {
		return httpx.BadRequest(fmt.Sprintf("The speed test sends exactly %d bytes; this request delivered %d.", storagebackend.BrowserProbeBytes, body.read))
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// handleStorageBrowserProbeGet returns the object the PUT half wrote, so the browser times
// the read-back along its own link to the store.
func (s *Server) handleStorageBrowserProbeGet(w http.ResponseWriter, r *http.Request) error {
	target, claim, backend, closer, err := s.browserProbeTarget(r)
	if closer != nil {
		defer closer.Close()
	}
	if err != nil {
		return err
	}
	reader, size, err := storagebackend.ReadRelayedBrowserProbe(r.Context(), target.Name, backend, claim.key)
	if err != nil {
		return err
	}
	defer reader.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, reader)
	return nil
}

// browserProbeTarget resolves the connection and the live claim behind one relayed leg.
// Both legs are cookie-authenticated like every other settings request, and both refuse a
// run this owner did not open.
func (s *Server) browserProbeTarget(r *http.Request) (target connections.Connection, claim browserProbeClaim, backend objectmutation.Backend, closer io.Closer, err error) {
	if s.db == nil || s.conns == nil {
		return target, claim, nil, nil, httpx.ServiceUnavailable("Storage settings are unavailable on this instance.")
	}
	user, err := s.requireUser(r)
	if err != nil {
		return target, claim, nil, nil, err
	}
	if err = httpx.AssertSameOrigin(r); err != nil {
		return target, claim, nil, nil, err
	}
	conn, ok, err := s.visibleConnection(r, user.ID, r.PathValue("id"))
	if err != nil {
		return target, claim, nil, nil, err
	}
	if !ok {
		return target, claim, nil, nil, httpx.NotFound("No such storage connection on this account.")
	}
	claim, ok = s.browserProbe(user.ID, conn.ID, r.PathValue("runId"))
	if !ok {
		return target, claim, nil, nil, httpx.NotFound("This speed test run has expired or was never started here; start the test again.")
	}
	effective, applyErr := conn.ApplyEnvironment(*s.cfg)
	if applyErr != nil {
		return target, claim, nil, nil, httpx.BadRequest(applyErr.Error())
	}
	opened, openedCloser, openErr := storagebackend.OpenObjectBackend(&effective)
	if openErr != nil {
		return target, claim, nil, nil, openErr
	}
	return conn, claim, opened, openedCloser, nil
}

// countingReader counts what the store actually consumed, so a short body cannot report a
// rate for bytes nobody sent.
type countingReader struct {
	reader io.Reader
	read   int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += int64(n)
	return n, err
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
