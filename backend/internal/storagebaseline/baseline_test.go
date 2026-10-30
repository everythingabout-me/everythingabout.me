package storagebaseline

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func cleanSnapshot() Snapshot {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	return Snapshot{Version: 1, OwnerID: "owner", Backend: "cos", ContentRoot: "drive/owner/", StartedAt: now, FinishedAt: now, NativeComplete: true, IndexComplete: true, WritersQuiesced: true,
		Native: []NativeEntry{{Key: "drive/owner/Notes/ a+%23?#.txt ", Size: 7}, {Key: "drive/owner/Empty/"}},
		Index:  []IndexEntry{{ID: "private-1", Path: "Notes", Key: "drive/owner/Notes/", Folder: true}, {ID: "private-2", Path: "Notes/ a+%23?#.txt ", Key: "drive/owner/Notes/ a+%23?#.txt ", Size: 7}, {ID: "private-3", Path: "Empty", Key: "drive/owner/Empty/", Folder: true}}}
}

func TestExactNamesImplicitParentsAndOverQuotaPreservation(t *testing.T) {
	s := cleanSnapshot()
	s.Native = append(s.Native, NativeEntry{Key: "drive/owner/", Size: 0})
	before, _ := json.Marshal(s)
	r := Compare(s, 3)
	if !r.ReservationBaselineReady || !r.OverQuota || r.NativeBytes != 7 || r.IndexBytes != 7 || r.NativeFiles != 1 || r.NativeFolders != 2 {
		t.Fatalf("unexpected report: %+v", r)
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("comparison mutated snapshot")
	}
	if !strings.Contains(string(after), " a+%23?#.txt ") {
		t.Fatal("exact filename spelling was lost")
	}
}

func TestBaselineRefusesUnresolvedEvidence(t *testing.T) {
	cases := []struct {
		name, code string
		mutate     func(*Snapshot)
	}{
		{"incomplete native", "incomplete_baseline", func(s *Snapshot) { s.NativeComplete = false }},
		{"incomplete index", "incomplete_baseline", func(s *Snapshot) { s.IndexComplete = false }},
		{"active writers", "incomplete_baseline", func(s *Snapshot) { s.WritersQuiesced = false }},
		{"wrong owner", "outside_root", func(s *Snapshot) { s.Native = append(s.Native, NativeEntry{Key: "drive/other/private", Size: 100}) }},
		{"missing index", "native_only", func(s *Snapshot) { s.Native = append(s.Native, NativeEntry{Key: "drive/owner/unindexed", Size: 9}) }},
		{"missing native", "index_only", func(s *Snapshot) {
			s.Index = append(s.Index, IndexEntry{Path: "missing", Key: "drive/owner/missing", Size: 4})
		}},
		{"mismatch", "size_mismatch", func(s *Snapshot) { s.Index[1].Size = 99 }},
		{"mapping", "index_mapping_mismatch", func(s *Snapshot) { s.Index[1].Key = "drive/other/stolen" }},
		{"collision", "normalization_conflict", func(s *Snapshot) {
			s.Native = append(s.Native, NativeEntry{Key: "drive/owner/caf\u00e9"}, NativeEntry{Key: "drive/owner/cafe\u0301"})
		}},
		{"file parent", "name_conflict", func(s *Snapshot) { s.Native = append(s.Native, NativeEntry{Key: "drive/owner/Notes"}) }},
		{"repeated page", "duplicate_key", func(s *Snapshot) { s.Native = append(s.Native, s.Native[0]) }},
		{"invalid path", "invalid_name", func(s *Snapshot) { s.Native = append(s.Native, NativeEntry{Key: "drive/owner/a//b"}) }},
		{"double slash root marker", "invalid_name", func(s *Snapshot) { s.Native = append(s.Native, NativeEntry{Key: "drive/owner//"}) }},
		{"missing implicit parent", "native_only", func(s *Snapshot) { s.Index = s.Index[1:] }},
		{"invalid Unicode control", "invalid_name", func(s *Snapshot) { s.Native = append(s.Native, NativeEntry{Key: "drive/owner/a\u0085b"}) }},
		{"nonempty root marker", "nonempty_folder_marker", func(s *Snapshot) { s.Native = append(s.Native, NativeEntry{Key: "drive/owner/", Size: 1}) }},
		{"legacy effects", "legacy_sessions_present", func(s *Snapshot) {
			s.LegacyUploads = []json.RawMessage{json.RawMessage(`{"cancelled_at":"2026-10-30"}`)}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := cleanSnapshot()
			tc.mutate(&s)
			r := Compare(s, 0)
			if r.ReservationBaselineReady {
				t.Fatal("unresolved evidence was admitted")
			}
			found := false
			for _, i := range r.Issues {
				if i.Code == tc.code {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing %s in %+v", tc.code, r.Issues)
			}
			if tc.name == "missing index" && r.NativeBytes != 16 {
				t.Fatal("unindexed native bytes were not charged")
			}
		})
	}
}

func TestNativeDirectoryMarkerAndOverflow(t *testing.T) {
	s := cleanSnapshot()
	s.Native = append(s.Native, NativeEntry{Key: "drive/owner/", Size: 0}, NativeEntry{Key: "drive/owner/bad/", Size: 1}, NativeEntry{Key: "drive/owner/huge", Size: 9007199254740991})
	r := Compare(s, 0)
	codes := map[string]bool{}
	for _, i := range r.Issues {
		codes[i.Code] = true
	}
	if !codes["nonempty_folder_marker"] || !codes["usage_overflow"] || r.ReservationBaselineReady {
		t.Fatalf("unexpected %+v", r)
	}
}
