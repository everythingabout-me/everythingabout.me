package main

import "testing"

func TestStorageResolveUploadRejectsUnsafeScopeBeforeConfiguration(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--operation-id", ""},
		{"--operation-id", "not-a-uuid"},
		{"--operation-id", "E6B3F4D3-C530-41F0-827D-D0935804FB66"},
		{"--operation-id", "e6b3f4d3-c530-41f0-827d-d0935804fb66", "--timeout", "0s"},
		{"--operation-id", "e6b3f4d3-c530-41f0-827d-d0935804fb66", "--timeout", "2h"},
		{"--operation-id", "e6b3f4d3-c530-41f0-827d-d0935804fb66", "unexpected"},
	} {
		if err := runStorageResolveUpload(args); err == nil {
			t.Fatalf("unsafe options accepted: %v", args)
		}
	}
}
