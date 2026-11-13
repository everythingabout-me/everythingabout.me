package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/connections"
	"everythingabout.me/backend/internal/filechanges"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

// fakeRootStore keeps the root-binding paths testable without a database: the
// derived view is exactly these maps, and every write is recorded.
type fakeRootStore struct {
	bindings    map[string]rootBinding
	heads       map[string]string
	pending     map[string]pendingWork
	backfilled  []string
	rebinds     []fakeRebind
	head        filechanges.Head
	bindingsErr error
	rebindErr   error
}

type fakeRebind struct {
	Owner     string
	Binding   rootBinding
	Namespace string
}

func newFakeRootStore() *fakeRootStore {
	return &fakeRootStore{
		bindings: map[string]rootBinding{},
		heads:    map[string]string{},
		pending:  map[string]pendingWork{},
		head:     filechanges.Head{RootEpoch: "epoch-after-rebind"},
	}
}

func (f *fakeRootStore) Bindings(context.Context) (map[string]rootBinding, error) {
	if f.bindingsErr != nil {
		return nil, f.bindingsErr
	}
	return f.bindings, nil
}

func (f *fakeRootStore) ChangeNamespaces(context.Context) (map[string]string, error) {
	return f.heads, nil
}

func (f *fakeRootStore) BackfillCoordinates(_ context.Context, owner, bucket, region string) error {
	f.backfilled = append(f.backfilled, owner+"|"+bucket+"|"+region)
	return nil
}

func (f *fakeRootStore) PendingWork(_ context.Context, owner string) (pendingWork, error) {
	return f.pending[owner], nil
}

func (f *fakeRootStore) Rebind(_ context.Context, owner string, binding rootBinding, namespace string) (filechanges.Head, error) {
	if f.rebindErr != nil {
		return filechanges.Head{}, f.rebindErr
	}
	f.rebinds = append(f.rebinds, fakeRebind{Owner: owner, Binding: binding, Namespace: namespace})
	return f.head, nil
}

// fakeBindingSource is the configured side, without a connection row or a provider.
type fakeBindingSource struct {
	roots map[string]rootBinding
	err   error
}

func newFakeBindingSource() *fakeBindingSource {
	return &fakeBindingSource{roots: map[string]rootBinding{}}
}

func (f *fakeBindingSource) lookup(owner, name string) (rootBinding, bool) {
	if name == "" {
		root, ok := f.roots[owner]
		return root, ok
	}
	if root, ok := f.roots[owner+"|"+name]; ok {
		return root, true
	}
	// The fixture usually stores one root per owner under the plain owner key; it
	// answers a named lookup when that root names the requested connection.
	if root, ok := f.roots[owner]; ok && connectionName(root.ConnectionID) == name {
		return root, true
	}
	return rootBinding{}, false
}

func (f *fakeBindingSource) BindingFor(_ context.Context, owner, name string) (storage.Config, error) {
	if f.err != nil {
		return storage.Config{}, f.err
	}
	root, ok := f.lookup(owner, name)
	if !ok {
		return storage.Config{}, fmt.Errorf("no connection named %q is configured for owner %s", name, owner)
	}
	return storage.Config{OwnerID: root.OwnerID, ConnectionID: root.ConnectionID, Generation: root.Generation, ContentRoot: root.ContentRoot, SystemRoot: root.SystemRoot}, nil
}

func (f *fakeBindingSource) ConnectionFor(_ context.Context, owner, name string) (connections.Connection, error) {
	if f.err != nil {
		return connections.Connection{}, f.err
	}
	root, ok := f.lookup(owner, name)
	if !ok {
		return connections.Connection{}, fmt.Errorf("no connection named %q is configured for owner %s", name, owner)
	}
	conn := connections.Connection{Name: connectionName(root.ConnectionID), Backend: "cos"}
	conn.Config.COS.Bucket = root.Bucket
	conn.Config.COS.Region = root.Region
	return conn, nil
}

const (
	ownerA = "11111111-1111-1111-1111-111111111111"
	ownerB = "22222222-2222-2222-2222-222222222222"
)

// binding builds one side of the comparison. The digest is derived from the
// location so a changed bucket, region, prefix or generation produces the same
// connection identity a real digest would: only bucket/region/prefix are in the
// digest, generation is its own field.
func binding(owner, connection, bucket, region, content string, generation int64) rootBinding {
	digest := fmt.Sprintf("digest-%s-%s-%s", bucket, region, content)
	return rootBinding{
		Root: fileops.Root{
			OwnerID:       owner,
			ConnectionID:  connection + ":" + digest,
			Generation:    generation,
			ContentRoot:   content,
			SystemRoot:    "__eam_system__/" + owner + "/",
			CaseSensitive: true,
		},
		Bucket: bucket,
		Region: region,
	}
}

// The refusal is a value comparison, so every identity field is exercised without a
// bucket: changing one field must produce exactly one finding naming it and both
// values.
func TestCompareRootBindingReportsEveryField(t *testing.T) {
	base := binding(ownerA, "default", "bucket-a", "ap-hongkong", "drive/"+ownerA+"/", 3)
	if diffs := compareRootBinding(base, base); len(diffs) != 0 {
		t.Fatalf("identical bindings reported as different: %v", diffs)
	}
	cases := []struct {
		name       string
		field      string
		mutate     func(*rootBinding)
		bound      string
		configured string
	}{
		{"bucket", "bucket", func(b *rootBinding) { b.Bucket = "bucket-b" }, "bucket-a", "bucket-b"},
		{"region", "region", func(b *rootBinding) { b.Region = "ap-beijing" }, "ap-hongkong", "ap-beijing"},
		{"content prefix", "content prefix", func(b *rootBinding) { b.ContentRoot = "drive/moved/" }, "drive/" + ownerA + "/", "drive/moved/"},
		{"system prefix", "system prefix", func(b *rootBinding) { b.SystemRoot = "__eam_system__/moved/" }, "__eam_system__/" + ownerA + "/", "__eam_system__/moved/"},
		{"generation", "generation", func(b *rootBinding) { b.Generation = 4 }, "3", "4"},
		{"connection identity", "connection identity", func(b *rootBinding) { b.ConnectionID = "other:digest" }, base.ConnectionID, "other:digest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configured := base
			tc.mutate(&configured)
			diffs := compareRootBinding(base, configured)
			if len(diffs) != 1 {
				t.Fatalf("got %d findings, want exactly one: %v", len(diffs), diffs)
			}
			got := diffs[0]
			if got.Field != tc.field {
				t.Fatalf("field = %q, want %q", got.Field, tc.field)
			}
			if got.Bound != tc.bound || got.Configured != tc.configured {
				t.Fatalf("values = bound %q configured %q, want %q and %q", got.Bound, got.Configured, tc.bound, tc.configured)
			}
			if !strings.Contains(got.String(), tc.bound) || !strings.Contains(got.String(), tc.configured) {
				t.Fatalf("finding omits a value: %s", got.String())
			}
		})
	}
}

// A binding written before coordinates were recorded has no bucket or region. The
// digest still covers them, so the comparison must not invent a bucket mismatch
// from the empty field.
func TestCompareRootBindingDoesNotInventMissingCoordinates(t *testing.T) {
	bound := binding(ownerA, "default", "bucket-a", "ap-hongkong", "drive/"+ownerA+"/", 1)
	bound.Bucket, bound.Region = "", ""
	configured := bound
	configured.Bucket, configured.Region = "bucket-a", "ap-hongkong"
	if diffs := compareRootBinding(bound, configured); len(diffs) != 0 {
		t.Fatalf("missing coordinates reported as a mismatch: %v", diffs)
	}
	configured.ConnectionID = "default:other-digest"
	diffs := compareRootBinding(bound, configured)
	if len(diffs) != 1 || diffs[0].Field != "connection identity" {
		t.Fatalf("digest mismatch = %v, want the connection identity", diffs)
	}
	for _, diff := range diffs {
		if diff.Field == "bucket" || diff.Field == "region" {
			t.Fatalf("unrecorded coordinates were compared: %v", diffs)
		}
	}
}

func TestRootMismatchesNoBindingYet(t *testing.T) {
	called := false
	mismatches := rootMismatches(nil, nil, func(string) (rootBinding, error) {
		called = true
		return rootBinding{}, nil
	})
	if len(mismatches) != 0 {
		t.Fatalf("a database with no binding was refused: %v", mismatches)
	}
	if called {
		t.Fatal("a database with no binding asked the configuration to bind an owner")
	}
}

func TestRootMismatchesReportsEveryOwnerAndBothSides(t *testing.T) {
	bindings := map[string]rootBinding{
		ownerA: binding(ownerA, "default", "bucket-old", "ap-hongkong", "drive/"+ownerA+"/", 1),
		ownerB: binding(ownerB, "default", "bucket-old", "ap-hongkong", "drive/"+ownerB+"/", 1),
	}
	heads := map[string]string{ownerA: bindings[ownerA].namespace()}
	configured := func(owner string) (rootBinding, error) {
		return binding(owner, "default", "bucket-new", "ap-beijing", "drive/"+owner+"/", 2), nil
	}
	mismatches := rootMismatches(bindings, heads, configured)
	if len(mismatches) != 2 {
		t.Fatalf("got %d mismatches, want both owners: %v", len(mismatches), mismatches)
	}
	report := (&rootMismatchError{Mismatches: mismatches}).Error()
	for _, fragment := range []string{ownerA, ownerB, "bucket", "bucket-old", "bucket-new", "region", "generation", "storage-rebind", "Two actions"} {
		if !strings.Contains(report, fragment) {
			t.Fatalf("refusal omits %q:\n%s", fragment, report)
		}
	}
	if !strings.Contains(report, "--from default --to default") {
		t.Fatalf("refusal does not name the rebind command:\n%s", report)
	}
}

func TestRootMismatchesReportsAStaleChangeStream(t *testing.T) {
	root := binding(ownerA, "default", "bucket-a", "ap-hongkong", "drive/"+ownerA+"/", 1)
	bindings := map[string]rootBinding{ownerA: root}
	heads := map[string]string{ownerA: "a-namespace-from-another-tree"}
	mismatches := rootMismatches(bindings, heads, func(string) (rootBinding, error) { return root, nil })
	if len(mismatches) != 1 || len(mismatches[0].Diffs) != 1 {
		t.Fatalf("stale change stream = %v, want one finding", mismatches)
	}
	if got := mismatches[0].Diffs[0].Field; got != "change-stream namespace" {
		t.Fatalf("field = %q, want the change-stream namespace", got)
	}
}

func TestRootMismatchesReportsAnUnbindableOwner(t *testing.T) {
	bindings := map[string]rootBinding{ownerA: binding(ownerA, "default", "bucket-a", "ap-hongkong", "drive/"+ownerA+"/", 1)}
	mismatches := rootMismatches(bindings, nil, func(string) (rootBinding, error) {
		return rootBinding{}, fmt.Errorf("storage connection %q is disabled", "default")
	})
	if len(mismatches) != 1 || mismatches[0].Resolve == nil {
		t.Fatalf("unbindable owner = %v, want a reported cause", mismatches)
	}
	if !strings.Contains(mismatches[0].String(), "disabled") {
		t.Fatalf("cause was dropped: %s", mismatches[0].String())
	}
}

func TestRefuseStorageRootsAcceptsAMatchingBinding(t *testing.T) {
	root := binding(ownerA, "default", "bucket-a", "ap-hongkong", "drive/"+ownerA+"/", 1)
	store := newFakeRootStore()
	store.bindings[ownerA] = root
	store.heads[ownerA] = root.namespace()
	source := newFakeBindingSource()
	source.roots[ownerA] = root
	if err := refuseStorageRoots(context.Background(), store, source); err != nil {
		t.Fatalf("matching binding refused: %v", err)
	}
	if len(store.backfilled) != 0 {
		t.Fatalf("a fully recorded binding was rewritten: %v", store.backfilled)
	}
}

func TestRefuseStorageRootsRecordsMissingCoordinatesOnAMatchingBinding(t *testing.T) {
	root := binding(ownerA, "default", "bucket-a", "ap-hongkong", "drive/"+ownerA+"/", 1)
	legacy := root
	legacy.Bucket, legacy.Region = "", ""
	store := newFakeRootStore()
	store.bindings[ownerA] = legacy
	store.heads[ownerA] = root.namespace()
	source := newFakeBindingSource()
	source.roots[ownerA] = root
	if err := refuseStorageRoots(context.Background(), store, source); err != nil {
		t.Fatalf("a matching legacy binding refused: %v", err)
	}
	if len(store.backfilled) != 1 || store.backfilled[0] != ownerA+"|bucket-a|ap-hongkong" {
		t.Fatalf("coordinates were not recorded: %v", store.backfilled)
	}
}

func TestRefuseStorageRootsRefusesAChangedRoot(t *testing.T) {
	store := newFakeRootStore()
	store.bindings[ownerA] = binding(ownerA, "default", "bucket-old", "ap-hongkong", "drive/"+ownerA+"/", 1)
	store.heads[ownerA] = store.bindings[ownerA].namespace()
	source := newFakeBindingSource()
	source.roots[ownerA] = binding(ownerA, "default", "bucket-new", "ap-hongkong", "drive/"+ownerA+"/", 1)
	err := refuseStorageRoots(context.Background(), store, source)
	if err == nil {
		t.Fatal("a changed bucket was adopted instead of refused")
	}
	report := err.Error()
	for _, fragment := range []string{ownerA, "refusing to start", `bucket: bound "bucket-old", configured "bucket-new"`, "eam storage-rebind", "empty database"} {
		if !strings.Contains(report, fragment) {
			t.Fatalf("refusal omits %q:\n%s", fragment, report)
		}
	}
	if len(store.backfilled) != 0 || len(store.rebinds) != 0 {
		t.Fatalf("refusal wrote derived state: backfilled=%v rebinds=%v", store.backfilled, store.rebinds)
	}
}

func TestInspectStorageRootsIsReadOnly(t *testing.T) {
	store := newFakeRootStore()
	legacy := binding(ownerA, "default", "bucket-a", "ap-hongkong", "drive/"+ownerA+"/", 1)
	legacy.Bucket, legacy.Region = "", ""
	store.bindings[ownerA] = legacy
	store.heads[ownerA] = legacy.namespace()
	source := newFakeBindingSource()
	source.roots[ownerA] = binding(ownerA, "default", "bucket-a", "ap-hongkong", "drive/"+ownerA+"/", 1)
	if err := inspectStorageRoots(context.Background(), store, source); err != nil {
		t.Fatalf("matching binding reported: %v", err)
	}
	if len(store.backfilled) != 0 {
		t.Fatalf("doctor wrote derived state: %v", store.backfilled)
	}
}

func TestRootOwnersIsSortedAndUnionsBothMaps(t *testing.T) {
	bindings := map[string]rootBinding{ownerB: binding(ownerB, "default", "b", "r", "c", 1)}
	heads := map[string]string{ownerA: "n", ownerB: "n"}
	owners := rootOwners(bindings, heads)
	if strings.Join(owners, ",") != ownerA+","+ownerB {
		t.Fatalf("owners = %v, want both owners sorted", owners)
	}
}

// The coordinates are extra keys in the same JSON document fileops writes and
// reads. This is what keeps the record a derived cache: fileops must keep decoding
// the Root it always decoded, and the operator side must see the coordinates.
func TestRootBindingJSONKeepsFileopsRootReadable(t *testing.T) {
	rb := binding(ownerA, "default", "bucket-a", "ap-hongkong", "drive/"+ownerA+"/", 7)
	data, err := json.Marshal(rb)
	if err != nil {
		t.Fatal(err)
	}
	var decoded fileops.Root
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("fileops.Root no longer decodes the binding JSON: %v", err)
	}
	if decoded != rb.Root {
		t.Fatalf("fileops.Root round-trip = %+v, want %+v", decoded, rb.Root)
	}
	var withCoordinates rootBinding
	if err := json.Unmarshal(data, &withCoordinates); err != nil {
		t.Fatal(err)
	}
	if withCoordinates.Bucket != "bucket-a" || withCoordinates.Region != "ap-hongkong" {
		t.Fatalf("coordinates lost in the round trip: %+v", withCoordinates)
	}
}

func TestConnectionNameSplitsTheDigest(t *testing.T) {
	if got := connectionName("default:abc:def"); got != "default:abc" {
		t.Fatalf("connectionName = %q", got)
	}
	if got := connectionName("default"); got != "default" {
		t.Fatalf("connectionName without a digest = %q", got)
	}
}
