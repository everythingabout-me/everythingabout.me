package main

import (
	"testing"
	"time"
)

func TestFileAuditPruneRejectsUnsafeArgumentsBeforeConfiguration(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--owner-id", "not-a-uuid", "--before", "2025-11-13"},
		{"--owner-id", "11111111-1111-4111-8111-111111111111"},
		{"--owner-id", "11111111-1111-4111-8111-111111111111", "--before", "2999-01-01"},
		{"--owner-id", "11111111-1111-4111-8111-111111111111", "--before", "13/11/2025"},
		{"--owner-id", "11111111-1111-4111-8111-111111111111", "--before", "2025-11-13", "extra"},
	} {
		if err := runFileAuditPrune(args); err == nil {
			t.Fatalf("unsafe audit prune admitted: %+v", args)
		}
	}
}

func TestAuditCutoffIsExplicitAndUTC(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   string
		want time.Time
	}{
		{"2025-11-13", time.Date(2025, 11, 13, 0, 0, 0, 0, time.UTC)},
		{"2025-11-13T08:30:00+08:00", time.Date(2025, 11, 13, 0, 30, 0, 0, time.UTC)},
	} {
		got, err := auditCutoff(tc.in, now)
		if err != nil {
			t.Fatalf("auditCutoff(%q): %v", tc.in, err)
		}
		if !got.Equal(tc.want) || got.Location() != time.UTC {
			t.Fatalf("auditCutoff(%q)=%s want %s in UTC", tc.in, got, tc.want)
		}
	}
	// A cutoff in the future would delete rows written after the run started, so it is
	// refused instead of being accepted as "prune everything".
	for _, in := range []string{"", "2999-01-01", "2999-01-01T00:00:00Z", "tomorrow"} {
		if _, err := auditCutoff(in, now); err == nil {
			t.Fatalf("auditCutoff(%q) accepted an unsafe or unparsable cutoff", in)
		}
	}
}
