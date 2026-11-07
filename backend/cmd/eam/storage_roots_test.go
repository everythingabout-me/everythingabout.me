package main

import (
	"fmt"
	"strings"
	"testing"
)

func rootFixture(owner, connection string) rootIdentity {
	return rootIdentity{
		OwnerID:      owner,
		ConnectionID: connection,
		Generation:   1,
		ContentRoot:  "drive/" + owner + "/",
		SystemRoot:   "__eam_system__/" + owner + "/",
	}
}

func expectedFrom(identities map[string]rootIdentity) func(string) (rootIdentity, error) {
	return func(owner string) (rootIdentity, error) {
		identity, ok := identities[owner]
		if !ok {
			return rootIdentity{}, fmt.Errorf("no fixture for %s", owner)
		}
		return identity, nil
	}
}

func TestCompareRootBindingsAcceptsMatchingTree(t *testing.T) {
	owner := "11111111-1111-1111-1111-111111111111"
	want := rootFixture(owner, "default:aaaa")
	if err := compareRootBindings(
		map[string]rootIdentity{owner: want},
		map[string]string{owner: want.namespace()},
		expectedFrom(map[string]rootIdentity{owner: want}),
	); err != nil {
		t.Fatalf("identical root rejected: %v", err)
	}
	if err := compareRootBindings(nil, nil, expectedFrom(nil)); err != nil {
		t.Fatalf("empty deployment rejected: %v", err)
	}
}

func TestCompareRootBindingsReportsBothConnections(t *testing.T) {
	owner := "11111111-1111-1111-1111-111111111111"
	stored := rootFixture(owner, "default:old")
	want := rootFixture(owner, "default:new")
	err := compareRootBindings(
		map[string]rootIdentity{owner: stored},
		map[string]string{owner: stored.namespace()},
		expectedFrom(map[string]rootIdentity{owner: want}),
	)
	if err == nil {
		t.Fatal("changed connection admitted")
	}
	for _, fragment := range []string{owner, "file_budgets", "default:old", "default:new", "root_conflict"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("mismatch message omits %q:\n%s", fragment, err)
		}
	}
	// The change stream is a separate observable failure, and it is worth naming.
	if !strings.Contains(err.Error(), "file_change_heads") {
		t.Fatalf("stale change namespace not reported:\n%s", err)
	}
}

func TestCompareRootBindingsChecksChangeStreamWithMatchingBudget(t *testing.T) {
	owner := "22222222-2222-2222-2222-222222222222"
	want := rootFixture(owner, "default:aaaa")
	stale := rootFixture(owner, "default:bbbb")
	err := compareRootBindings(
		map[string]rootIdentity{owner: want},
		map[string]string{owner: stale.namespace()},
		expectedFrom(map[string]rootIdentity{owner: want}),
	)
	if err == nil {
		t.Fatal("stale change namespace admitted while the budget matched")
	}
	if !strings.Contains(err.Error(), "file_change_heads") {
		t.Fatalf("stale change namespace not reported:\n%s", err)
	}
	// The budget matched, so it must not be blamed for this failure.
	if strings.Contains(err.Error(), "file_budgets:") {
		t.Fatalf("matching budget reported as mismatched:\n%s", err)
	}
}

func TestCompareRootBindingsCoversOwnersWithoutBudget(t *testing.T) {
	owner := "33333333-3333-3333-3333-333333333333"
	want := rootFixture(owner, "default:aaaa")
	err := compareRootBindings(
		nil,
		map[string]string{owner: rootFixture(owner, "default:cccc").namespace()},
		expectedFrom(map[string]rootIdentity{owner: want}),
	)
	if err == nil || !strings.Contains(err.Error(), owner) {
		t.Fatalf("change-only owner not validated: %v", err)
	}
}

func TestCompareRootBindingsReportsUnbindableOwner(t *testing.T) {
	owner := "44444444-4444-4444-4444-444444444444"
	err := compareRootBindings(
		map[string]rootIdentity{owner: rootFixture(owner, "default:aaaa")},
		nil,
		func(string) (rootIdentity, error) { return rootIdentity{}, fmt.Errorf("bad owner id") },
	)
	if err == nil || !strings.Contains(err.Error(), "cannot bind") || !strings.Contains(err.Error(), "bad owner id") {
		t.Fatalf("unbindable owner not reported: %v", err)
	}
}

func TestCompareRootBindingsReportsEveryOwnerSortedByID(t *testing.T) {
	first := "55555555-5555-5555-5555-555555555555"
	second := "66666666-6666-6666-6666-666666666666"
	err := compareRootBindings(
		map[string]rootIdentity{
			second: rootFixture(second, "default:old"),
			first:  rootFixture(first, "default:old"),
		},
		nil,
		expectedFrom(map[string]rootIdentity{
			first:  rootFixture(first, "default:new"),
			second: rootFixture(second, "default:new"),
		}),
	)
	if err == nil {
		t.Fatal("multiple mismatches accepted")
	}
	if strings.Index(err.Error(), first) > strings.Index(err.Error(), second) {
		t.Fatalf("owners not reported in stable order:\n%s", err)
	}
}
