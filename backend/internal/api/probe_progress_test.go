package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storagebackend"
)

// The page prints one row per phase, so the running record must expose exactly the phases
// that finished, in the order they finished, carrying the numbers their finished event
// carried. A record that lagged, repeated a phase, or kept the numbers of a phase that was
// still measuring would put a spinner under a value the page already printed.
func TestProbeProgressTracksFinishedPhases(t *testing.T) {
	server := &Server{}
	if !server.claimProbe("nas", "run-1", "2026-01-01T00:00:00Z") {
		t.Fatal("the first claim must start a run")
	}
	if server.claimProbe("nas", "run-2", "2026-01-01T00:00:01Z") {
		t.Fatal("a second claim on the same provider must be refused while one runs")
	}
	if !server.probeRunning("nas") {
		t.Fatal("a claimed provider must report a run")
	}

	// A starting phase names the row to spin but contributes no number: a row must not print
	// a value before the phase that produces it has finished.
	server.recordProbePhase("nas", storagebackend.Progress{Phase: storagebackend.PhaseLatency})
	started := server.probeProgress("nas")
	if started == nil {
		t.Fatal("a running provider must expose its progress")
	}
	if started.RunID != "run-1" || started.TestedAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("progress identity = %q/%q, want the claimed run's id and start time", started.RunID, started.TestedAt)
	}
	if started.Phase != string(storagebackend.PhaseLatency) {
		t.Fatalf("phase = %q, want %q", started.Phase, storagebackend.PhaseLatency)
	}
	if len(started.Done) != 0 {
		t.Fatalf("done = %v before the first phase finished, want none", started.Done)
	}

	server.recordProbePhase("nas", storagebackend.Progress{
		Phase: storagebackend.PhaseLatency, Done: true,
		Measurement: storagebackend.Measurement{Latency: 38 * time.Millisecond},
	})
	finished := server.probeProgress("nas")
	if finished.Phase != "" {
		t.Fatalf("phase = %q after the phase finished, want no row spinning", finished.Phase)
	}
	if finished.LatencyMS != 38 {
		t.Fatalf("latencyMs = %d, want the 38 the finished event carried", finished.LatencyMS)
	}
	if len(finished.Done) != 1 || finished.Done[0] != string(storagebackend.PhaseLatency) {
		t.Fatalf("done = %v, want [%s]", finished.Done, storagebackend.PhaseLatency)
	}

	// The page reads the running record as "the cached result, plus phase and done", so the
	// encoded shape must promote the result's fields instead of nesting them under a name.
	encoded, err := json.Marshal(finished)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"runId":"run-1"`, `"testedAt":"2026-01-01T00:00:00Z"`, `"latencyMs":38`, `"done":["latency"]`, `"phase":""`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("encoded progress %s does not contain %s", encoded, want)
		}
	}

	// A progress event for a provider with no run is dropped: the record is keyed by the
	// claim, not by whatever a late goroutine happens to report.
	server.recordProbePhase("elsewhere", storagebackend.Progress{Phase: storagebackend.PhaseUpload})
	if server.probeProgress("elsewhere") != nil {
		t.Fatal("an unclaimed provider must not gain a running record")
	}

	server.releaseProbe("nas")
	if server.probeRunning("nas") {
		t.Fatal("a released provider must not report a run")
	}
	if server.probeProgress("nas") != nil {
		t.Fatal("a released provider must not keep a progress record")
	}
}
