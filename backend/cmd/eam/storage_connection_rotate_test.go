package main

import (
	"strings"
	"testing"
)

func TestStorageConnectionRotateRequiresAnExplicitSource(t *testing.T) {
	if err := runStorageConnectionRotate(nil); err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("a rotation without a name was accepted: %v", err)
	}
	err := runStorageConnectionRotate([]string{"--name", "default"})
	if err == nil || !strings.Contains(err.Error(), "--from-environment") {
		t.Fatalf("a rotation without an explicit source was accepted: %v", err)
	}
}
