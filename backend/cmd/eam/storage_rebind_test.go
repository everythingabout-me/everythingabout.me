package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/filechanges"
)

func rebindFixture() (*fakeRootStore, *fakeBindingSource, string) {
	bound := binding(ownerA, "default", "bucket-old", "ap-hongkong", "drive/"+ownerA+"/", 1)
	configured := binding(ownerA, "default", "bucket-new", "ap-beijing", "drive/"+ownerA+"/", 2)
	store := newFakeRootStore()
	store.bindings[ownerA] = bound
	store.heads[ownerA] = bound.namespace()
	source := newFakeBindingSource()
	source.roots[ownerA] = configured
	return store, source, configured.namespace()
}

func TestRebindRefusesWithoutWritersQuiesced(t *testing.T) {
	store, source, _ := rebindFixture()
	_, err := rebindRoot(context.Background(), store, source, rebindOptions{Owner: ownerA, From: "default", To: "default"})
	if err == nil || !strings.Contains(err.Error(), "--writers-quiesced") {
		t.Fatalf("rebind without quiescence = %v, want a refusal naming --writers-quiesced", err)
	}
	if len(store.rebinds) != 0 {
		t.Fatal("rebind wrote while refusing")
	}
}

func TestRebindRefusesWhileNonTerminalWorkExists(t *testing.T) {
	store, source, _ := rebindFixture()
	store.pending[ownerA] = pendingWork{Operations: 2, Guards: 1}
	_, err := rebindRoot(context.Background(), store, source, rebindOptions{Owner: ownerA, From: "default", To: "default", WritersQuiesced: true})
	if err == nil || !strings.Contains(err.Error(), "2 non-terminal operation") || !strings.Contains(err.Error(), "1 pending guard") {
		t.Fatalf("busy rebind = %v, want a refusal naming the pending work", err)
	}
	if len(store.rebinds) != 0 {
		t.Fatal("rebind wrote while non-terminal work exists")
	}
}

func TestRebindRefusesARaceThatTurnsTheRootBusy(t *testing.T) {
	store, source, _ := rebindFixture()
	store.rebindErr = filechanges.ErrRootBusy
	_, err := rebindRoot(context.Background(), store, source, rebindOptions{Owner: ownerA, From: "default", To: "default", WritersQuiesced: true})
	if err == nil || !errors.Is(err, filechanges.ErrRootBusy) && !strings.Contains(err.Error(), "exclusive root lock") {
		t.Fatalf("busy root race = %v, want a refusal", err)
	}
}

func TestRebindRefusesWithoutABinding(t *testing.T) {
	store := newFakeRootStore()
	source := newFakeBindingSource()
	source.roots[ownerA] = binding(ownerA, "default", "bucket-new", "ap-beijing", "drive/"+ownerA+"/", 2)
	_, err := rebindRoot(context.Background(), store, source, rebindOptions{Owner: ownerA, From: "default", To: "default", WritersQuiesced: true})
	if err == nil || !strings.Contains(err.Error(), "no persisted root binding") {
		t.Fatalf("rebind without a binding = %v, want a refusal", err)
	}
}

func TestRebindRefusesFromThatIsNotTheBoundConnection(t *testing.T) {
	store, source, _ := rebindFixture()
	_, err := rebindRoot(context.Background(), store, source, rebindOptions{Owner: ownerA, From: "elsewhere", To: "default", WritersQuiesced: true})
	if err == nil || !strings.Contains(err.Error(), `--from "elsewhere"`) || !strings.Contains(err.Error(), `"default"`) {
		t.Fatalf("wrong --from = %v, want a refusal naming both", err)
	}
}

func TestRebindRefusesToThatIsNotTheConfiguredConnection(t *testing.T) {
	store, source, _ := rebindFixture()
	_, err := rebindRoot(context.Background(), store, source, rebindOptions{Owner: ownerA, From: "default", To: "elsewhere", WritersQuiesced: true})
	if err == nil || !strings.Contains(err.Error(), `--to "elsewhere"`) || !strings.Contains(err.Error(), "cannot bind owner") {
		t.Fatalf("wrong --to = %v, want a refusal naming the connection the operator asked for", err)
	}
}

func TestRebindRefusesWhenTheConfigurationCannotBindTheOwner(t *testing.T) {
	store, source, _ := rebindFixture()
	source.err = errors.New("storage connection secret is missing")
	_, err := rebindRoot(context.Background(), store, source, rebindOptions{Owner: ownerA, From: "default", To: "default", WritersQuiesced: true})
	if err == nil || !strings.Contains(err.Error(), "secret is missing") {
		t.Fatalf("unbindable configuration = %v, want the cause", err)
	}
}

func TestRebindWritesTheConfiguredBindingAndRotatesTheNamespace(t *testing.T) {
	store, source, configuredNamespace := rebindFixture()
	out, err := rebindRoot(context.Background(), store, source, rebindOptions{Owner: ownerA, From: "default", To: "default", WritersQuiesced: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.rebinds) != 1 {
		t.Fatalf("rebind wrote %d times, want one transaction", len(store.rebinds))
	}
	got := store.rebinds[0]
	if got.Owner != ownerA || got.Namespace != configuredNamespace {
		t.Fatalf("rebind = %+v, want owner %s and the configured namespace", got, ownerA)
	}
	if got.Binding.Bucket != "bucket-new" || got.Binding.Region != "ap-beijing" || got.Binding.Generation != 2 {
		t.Fatalf("rebind did not store the configured coordinates: %+v", got.Binding)
	}
	for _, fragment := range []string{"rebound", ownerA, "bucket", "bucket-old", "bucket-new", "epoch-after-rebind", "no bucket was listed", "eam reconcile"} {
		if !strings.Contains(out, fragment) {
			t.Fatalf("output omits %q:\n%s", fragment, out)
		}
	}
}

// The operator may add a new connection row and keep the old one; the persisted
// binding still names the old row, so only an explicit name can select the new
// one. That is the case --to exists for.
func TestRebindMovesToANamedConnectionTheOwnerIsNotBoundTo(t *testing.T) {
	old := binding(ownerA, "old", "bucket-old", "ap-hongkong", "drive/"+ownerA+"/", 1)
	moved := binding(ownerA, "new", "bucket-new", "ap-beijing", "drive/"+ownerA+"/", 2)
	store := newFakeRootStore()
	store.bindings[ownerA] = old
	store.heads[ownerA] = old.namespace()
	source := newFakeBindingSource()
	source.roots[ownerA] = old
	source.roots[ownerA+"|new"] = moved
	out, err := rebindRoot(context.Background(), store, source, rebindOptions{Owner: ownerA, From: "old", To: "new", WritersQuiesced: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.rebinds) != 1 || connectionName(store.rebinds[0].Binding.ConnectionID) != "new" {
		t.Fatalf("rebind = %+v, want the new connection", store.rebinds)
	}
	if store.rebinds[0].Namespace != moved.namespace() {
		t.Fatalf("namespace = %q, want the new root", store.rebinds[0].Namespace)
	}
	if !strings.Contains(out, `from connection "old" to connection "new"`) {
		t.Fatalf("output does not name both connections: %s", out)
	}
}

func TestRebindDryRunWritesNothing(t *testing.T) {
	store, source, configuredNamespace := rebindFixture()
	out, err := rebindRoot(context.Background(), store, source, rebindOptions{Owner: ownerA, From: "default", To: "default", WritersQuiesced: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.rebinds) != 0 {
		t.Fatal("dry-run wrote the binding")
	}
	for _, fragment := range []string{"[dry-run]", configuredNamespace, "nothing was written"} {
		if !strings.Contains(out, fragment) {
			t.Fatalf("dry-run output omits %q:\n%s", fragment, out)
		}
	}
}

// A root whose identity and change stream already match has nothing to rebind. The
// command must say so instead of rotating an epoch and expiring live cursors.
func TestRebindSaysWhenNothingChanged(t *testing.T) {
	root := binding(ownerA, "default", "bucket-a", "ap-hongkong", "drive/"+ownerA+"/", 1)
	store := newFakeRootStore()
	store.bindings[ownerA] = root
	store.heads[ownerA] = root.namespace()
	source := newFakeBindingSource()
	source.roots[ownerA] = root
	out, err := rebindRoot(context.Background(), store, source, rebindOptions{Owner: ownerA, From: "default", To: "default", WritersQuiesced: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.rebinds) != 0 {
		t.Fatal("an already-bound owner was rebound")
	}
	if !strings.Contains(out, "nothing changed") {
		t.Fatalf("no-op output = %q", out)
	}
}

// A binding whose identity matches but which predates the coordinate record gets the
// coordinates written without a namespace rotation.
func TestRebindRecordsMissingCoordinatesWithoutRotatingTheNamespace(t *testing.T) {
	root := binding(ownerA, "default", "bucket-a", "ap-hongkong", "drive/"+ownerA+"/", 1)
	legacy := root
	legacy.Bucket, legacy.Region = "", ""
	store := newFakeRootStore()
	store.bindings[ownerA] = legacy
	store.heads[ownerA] = root.namespace()
	source := newFakeBindingSource()
	source.roots[ownerA] = root
	out, err := rebindRoot(context.Background(), store, source, rebindOptions{Owner: ownerA, From: "default", To: "default", WritersQuiesced: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.rebinds) != 0 {
		t.Fatal("a coordinate backfill rotated the change stream")
	}
	if len(store.backfilled) != 1 || store.backfilled[0] != ownerA+"|bucket-a|ap-hongkong" {
		t.Fatalf("coordinates were not recorded: %v", store.backfilled)
	}
	if !strings.Contains(out, "nothing was rebound") {
		t.Fatalf("backfill output = %q", out)
	}
}
