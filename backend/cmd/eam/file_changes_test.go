package main

import "testing"

func TestFileChangeMaintenanceRejectsUnsafeArgumentsBeforeConfiguration(t *testing.T) {
	for _, tc := range []struct {
		reset bool
		args  []string
	}{
		{false, nil}, {true, nil}, {true, []string{"--owner-id", "11111111-1111-4111-8111-111111111111"}},
		{false, []string{"--owner-id", "not-a-uuid"}},
		{false, []string{"--owner-id", "11111111-1111-4111-8111-111111111111", "--writers-quiesced"}},
		{true, []string{"--owner-id", "11111111-1111-4111-8111-111111111111", "--writers-quiesced", "extra"}},
	} {
		if err := runFileChangesMaintenance(tc.args, tc.reset); err == nil {
			t.Fatalf("unsafe maintenance admitted: %+v", tc)
		}
	}
}
