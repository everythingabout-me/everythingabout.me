package jobs

import (
	"strings"
	"testing"
)

func TestNewJobID(t *testing.T) {
	cases := []struct {
		op     Op
		prefix string
	}{
		{OpMove, "m_"},
		{OpCopy, "c_"},
	}

	for _, tc := range cases {
		id, err := newJobID(tc.op)
		if err != nil {
			t.Fatalf("%s: generate failed: %v", tc.op, err)
		}
		if len(id) != 2+24 || !strings.HasPrefix(id, tc.prefix) {
			t.Fatalf("%s: unexpected shape: %q", tc.op, id)
		}
		other, err := newJobID(tc.op)
		if err != nil {
			t.Fatalf("%s: generate failed: %v", tc.op, err)
		}
		if id == other {
			t.Fatalf("%s: two generated ids are identical", tc.op)
		}
	}
}

func TestJobTerminal(t *testing.T) {
	for status, want := range map[string]bool{
		"pending":   false,
		"running":   false,
		"done":      true,
		"failed":    true,
		"cancelled": true,
	} {
		if got := (&Job{Status: status}).Terminal(); got != want {
			t.Errorf("%s: Terminal() = %v, want %v", status, got, want)
		}
	}
}
