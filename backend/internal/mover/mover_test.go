package mover

import (
	"strings"
	"testing"
)

func TestNewJobID(t *testing.T) {
	id, err := newJobID()
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if len(id) != 2+24 || !strings.HasPrefix(id, "m_") {
		t.Fatalf("形状不对: %q", id)
	}
	other, err := newJobID()
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if id == other {
		t.Fatal("两次生成的 id 相同")
	}
}
