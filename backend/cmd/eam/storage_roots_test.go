package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/fileops"
)

// fakeRootStore keeps the adoption path testable without a database: the derived
// view is exactly this pair of maps.
type fakeRootStore struct {
	budgets    map[string]fileops.Root
	heads      map[string]string
	busy       map[string]bool
	adopted    map[string]fileops.Root
	resets     []string
	adoptErr   error
	resetErr   error
	budgetsErr error
}

func newFakeRootStore() *fakeRootStore {
	return &fakeRootStore{budgets: map[string]fileops.Root{}, heads: map[string]string{}, busy: map[string]bool{}, adopted: map[string]fileops.Root{}}
}

func (f *fakeRootStore) Budgets(context.Context) (map[string]fileops.Root, error) {
	if f.budgetsErr != nil {
		return nil, f.budgetsErr
	}
	return f.budgets, nil
}

func (f *fakeRootStore) ChangeNamespaces(context.Context) (map[string]string, error) {
	return f.heads, nil
}

func (f *fakeRootStore) AdoptBinding(_ context.Context, owner string, binding fileops.Root) error {
	if f.adoptErr != nil {
		return f.adoptErr
	}
	f.adopted[owner] = binding
	return nil
}

func (f *fakeRootStore) ResetChangeRoot(_ context.Context, owner, namespace string) (bool, error) {
	if f.resetErr != nil {
		return false, f.resetErr
	}
	if f.busy[owner] {
		return false, nil
	}
	f.resets = append(f.resets, owner+"="+namespace)
	return true, nil
}

func storedRoot(owner, connection string) fileops.Root {
	return fileops.Root{
		OwnerID:         owner,
		ConnectionID:    connection,
		Generation:      1,
		ContentRoot:     "drive/" + owner + "/",
		SystemRoot:      "__eam_system__/" + owner + "/",
		CaseSensitive:   true,
		RetainsVersions: false,
	}
}

func expectedRoot(owner, connection string) func(string) (fileops.Root, error) {
	return func(requested string) (fileops.Root, error) {
		if requested != owner {
			return fileops.Root{}, fmt.Errorf("unexpected owner %s", requested)
		}
		return storedRoot(owner, connection), nil
	}
}

// The configured tree wins. A bucket, region, prefix or generation change rewrites
// the stale binding instead of refusing to start.
func TestAdoptRootsFollowsTheConfiguredTree(t *testing.T) {
	owner := "11111111-1111-1111-1111-111111111111"
	store := newFakeRootStore()
	store.budgets[owner] = storedRoot(owner, "default:old")
	store.heads[owner] = storedRoot(owner, "default:old").Namespace()

	if err := adoptRoots(context.Background(), store, expectedRoot(owner, "default:new")); err != nil {
		t.Fatal(err)
	}
	adopted, ok := store.adopted[owner]
	if !ok {
		t.Fatal("stale binding was not adopted")
	}
	if adopted.ConnectionID != "default:new" {
		t.Fatalf("adopted connection = %q, want the configured one", adopted.ConnectionID)
	}
	// Driver-derived flags must survive: dropping them would make the next
	// comparison see a different tree shape.
	if !adopted.CaseSensitive || adopted.RetainsVersions {
		t.Fatalf("driver flags were lost: %+v", adopted)
	}
	if len(store.resets) != 1 || store.resets[0] != owner+"="+storedRoot(owner, "default:new").Namespace() {
		t.Fatalf("change stream was not reset onto the new tree: %v", store.resets)
	}
}

func TestAdoptRootsLeavesAMatchingTreeAlone(t *testing.T) {
	owner := "22222222-2222-2222-2222-222222222222"
	store := newFakeRootStore()
	store.budgets[owner] = storedRoot(owner, "default:same")
	store.heads[owner] = storedRoot(owner, "default:same").Namespace()

	if err := adoptRoots(context.Background(), store, expectedRoot(owner, "default:same")); err != nil {
		t.Fatal(err)
	}
	if len(store.adopted) != 0 || len(store.resets) != 0 {
		t.Fatalf("matching tree was rewritten: adopted=%v resets=%v", store.adopted, store.resets)
	}
}

// A change stream with pending work cannot be reset. That is a warning, not a
// startup failure: the process keeps serving and the old namespace stays.
func TestAdoptRootsToleratesBusyChangeStream(t *testing.T) {
	owner := "33333333-3333-3333-3333-333333333333"
	store := newFakeRootStore()
	store.budgets[owner] = storedRoot(owner, "default:old")
	store.heads[owner] = storedRoot(owner, "default:old").Namespace()
	store.busy[owner] = true

	if err := adoptRoots(context.Background(), store, expectedRoot(owner, "default:new")); err != nil {
		t.Fatalf("busy root failed the startup path: %v", err)
	}
	if len(store.resets) != 0 {
		t.Fatalf("busy change stream reported as reset: %v", store.resets)
	}
	if _, ok := store.adopted[owner]; !ok {
		t.Fatal("budget adoption was skipped because the change stream was busy")
	}
}

func TestAdoptRootsSkipsUnbindableOwnerAndKeepsGoing(t *testing.T) {
	first := "44444444-4444-4444-4444-444444444444"
	second := "55555555-5555-5555-5555-555555555555"
	store := newFakeRootStore()
	store.budgets[first] = storedRoot(first, "default:old")
	store.budgets[second] = storedRoot(second, "default:old")

	expected := func(owner string) (fileops.Root, error) {
		if owner == first {
			return fileops.Root{}, fmt.Errorf("invalid owner id")
		}
		return storedRoot(owner, "default:new"), nil
	}
	if err := adoptRoots(context.Background(), store, expected); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.adopted[first]; ok {
		t.Fatal("unbindable owner was adopted anyway")
	}
	if _, ok := store.adopted[second]; !ok {
		t.Fatal("one unbindable owner stopped adoption for the rest")
	}
}

func TestAdoptRootsReportsDatabaseFailure(t *testing.T) {
	owner := "66666666-6666-6666-6666-666666666666"
	store := newFakeRootStore()
	store.budgets[owner] = storedRoot(owner, "default:old")
	store.adoptErr = fmt.Errorf("connection reset")

	err := adoptRoots(context.Background(), store, expectedRoot(owner, "default:new"))
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("database failure was swallowed: %v", err)
	}
}

func TestRootProblemsReportsBothSides(t *testing.T) {
	owner := "77777777-7777-7777-7777-777777777777"
	budgets := map[string]fileops.Root{owner: storedRoot(owner, "default:old")}
	heads := map[string]string{owner: storedRoot(owner, "default:old").Namespace()}

	problems := rootProblems(budgets, heads, expectedRoot(owner, "default:new"))
	joined := strings.Join(problems, "\n")
	for _, fragment := range []string{owner, "file_budgets", "file_change_heads", "default:old", "default:new"} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("report omits %q:\n%s", fragment, joined)
		}
	}
	if len(rootProblems(map[string]fileops.Root{owner: storedRoot(owner, "default:same")}, map[string]string{owner: storedRoot(owner, "default:same").Namespace()}, expectedRoot(owner, "default:same"))) != 0 {
		t.Fatal("matching tree reported as a problem")
	}
}
