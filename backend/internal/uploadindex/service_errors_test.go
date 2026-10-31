package uploadindex

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestProviderDiagnosticRemainsValidDatabaseText(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	j := newMemoryJournal()
	providerErr := errors.New("\x00" + strings.Repeat("漢", 800))
	s := testService(j, ObserverFunc(func(context.Context, string, string, bool) (string, error) {
		return "", providerErr
	}), &now)
	registerTest(t, s, now)
	if err := s.RunOnce(context.Background()); !errors.Is(err, providerErr) {
		t.Fatalf("provider failure was hidden: %v", err)
	}
	message := j.record(testKey()).result.Error
	if !utf8.ValidString(message) || strings.ContainsRune(message, '\x00') {
		t.Fatal("provider diagnostic would prevent PostgreSQL retry scheduling")
	}
	if len(message) > 2051 {
		t.Fatal("provider diagnostic was not bounded")
	}
}
