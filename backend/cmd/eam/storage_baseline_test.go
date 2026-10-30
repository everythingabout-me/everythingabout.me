package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storagebaseline"
)

func TestBaselineOfflineDoesNotLoadConfigOrOverwrite(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	dir := t.TempDir()
	input := filepath.Join(dir, "input.json")
	output := filepath.Join(dir, "output.json")
	now := time.Now().UTC()
	a := baselineArtifact{Snapshot: storagebaseline.Snapshot{Version: 1, OwnerID: "owner", Backend: "cos", ContentRoot: "drive/owner/", StartedAt: now, FinishedAt: now, NativeComplete: true, IndexComplete: true, WritersQuiesced: true}}
	if err := writeBaselineArtifact(input, a); err != nil {
		t.Fatal(err)
	}
	if err := runStorageBaseline([]string{"--input", input, "--output", output}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var got baselineArtifact
	if err = json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Report.ReservationBaselineReady {
		t.Fatalf("unexpected %+v", got.Report)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatal("baseline artifact is not private")
	}
	if err = runStorageBaseline([]string{"--input", input, "--output", output}); err == nil {
		t.Fatal("existing artifact overwritten")
	}
	again, _ := os.ReadFile(output)
	if string(data) != string(again) {
		t.Fatal("existing evidence changed")
	}
	if err = runStorageBaseline([]string{"--input", input, "--output", filepath.Join(dir, "other.json"), "--writers-quiesced"}); err == nil {
		t.Fatal("offline replay upgraded quiescence evidence")
	}
}

func TestBaselineRejectsArgumentsBeforeConfig(t *testing.T) {
	cases := [][]string{{}, {"--owner-id", "../owner", "--output", "unused"}, {"--output", "unused", "--quota-bytes", "-1"}, {"--output", "unused", "--timeout", "0s"}, {"--output", "unused", "unexpected"}}
	for _, args := range cases {
		if err := runStorageBaseline(args); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}

func TestBaselineRejectsTrailingJSON(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.json")
	if err := os.WriteFile(input, []byte(`{} {}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runStorageBaseline([]string{"--input", input, "--output", filepath.Join(dir, "output.json")}); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("trailing JSON accepted: %v", err)
	}
}
