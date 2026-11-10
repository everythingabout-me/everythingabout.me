package objectmutation

import (
	"context"
	"errors"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

// listETagTree is a read tree whose listings carry each entry's ETag, which is what
// a real delimiter listing does. It is the only thing the mutation driver needs to
// stop re-reading every child.
type listETagTree struct{ *fakeTree }

func (t listETagTree) List(ctx context.Context, path storage.Path, req storage.PageRequest) (storage.Page, error) {
	page, err := t.fakeTree.List(ctx, path, req)
	if err != nil {
		return page, err
	}
	for i := range page.Entries {
		if object, ok := t.backend.current[t.root+string(page.Entries[i].Path)]; ok {
			page.Entries[i].NativeETag = []byte(object.ETag)
		}
	}
	return page, nil
}

func fixtureWithTree(t *testing.T, listingETag bool, build func(*fakeTree) storage.Driver) (*Driver, *fakeBackend, storage.Config) {
	t.Helper()
	b := newFake()
	cfg := storage.Config{OwnerID: "owner", ConnectionID: "fixture", Generation: 1, ContentRoot: "drive/owner/", SystemRoot: "system/owner/"}
	options := Options{SpoolRoot: t.TempDir(), MaxPublishBytes: 1024, Admission: Admission{ConnectionID: cfg.ConnectionID, Endpoint: "https://fixture.invalid", Bucket: "bucket", Region: "region", CheckedAt: time.Now().UTC(), ProbeReference: "isolated-positive-negative-test", AtomicPublish: true, ConditionalCreate: true, SingleGateway: true, ListingETag: listingETag}}
	d, err := New(build(&fakeTree{backend: b, root: cfg.ContentRoot}), cfg, b, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, b, cfg
}

// A listing that already reported each child's ETag must not be turned back into one
// HEAD per child. Those HEADs are the reason a 103-entry recursive delete spent ~430
// provider requests before it answered: three passes over the tree, and every pass
// re-read a value the listing had already returned.
func TestListTrustsTheListedETagInsteadOfReheadingEveryChild(t *testing.T) {
	d, b, cfg := fixtureWithTree(t, true, func(tree *fakeTree) storage.Driver { return listETagTree{tree} })
	ctx := context.Background()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		key := cfg.ContentRoot + name
		b.current[key] = Object{Key: key, ETag: `"etag-` + name + `"`, Size: 3}
	}
	page, err := d.List(ctx, "", storage.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if b.heads != 0 {
		t.Fatalf("listing issued %d HEADs for entries the listing already named", b.heads)
	}
	if len(page.Entries) != 3 {
		t.Fatalf("entries=%d", len(page.Entries))
	}
	for _, entry := range page.Entries {
		want := nativeRevision(`"etag-` + string(entry.Path) + `"`)
		if string(entry.Revision) != string(want) {
			t.Fatalf("%s revision=%q want %q", entry.Path, entry.Revision, want)
		}
		if entry.NativeETag != nil {
			t.Fatalf("%s kept listing detail in its entry: %q", entry.Path, entry.NativeETag)
		}
	}
}

// The other half: an entry a listing cannot name — a folder that exists only as a
// common prefix — still gets observed, because nothing else proves its identity.
func TestListStillObservesEntriesTheListingCouldNotName(t *testing.T) {
	d, b, cfg := fixtureWithTree(t, true, func(tree *fakeTree) storage.Driver { return listETagTree{tree} })
	ctx := context.Background()
	b.current[cfg.ContentRoot+"Prefix/child.txt"] = Object{Key: cfg.ContentRoot + "Prefix/child.txt", ETag: `"etag-child"`, Size: 3}
	page, err := d.List(ctx, "", storage.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Kind != storage.Folder {
		t.Fatalf("entries=%+v", page.Entries)
	}
	if page.Entries[0].Revision != nil {
		t.Fatalf("implicit folder invented a revision: %q", page.Entries[0].Revision)
	}
}

// A delete whose durable checkpoint was lost sits at "ready": identity validated, and
// "publishing" — the marker written before the only remote request — never reached the
// ledger. It has no spool file to inspect (that is upload machinery), so an inspection
// that demanded one could never resolve it, and one recursively deleted folder stayed
// unresolvable forever while the API retried it every thirty seconds.
func TestDeleteLedgerThatNeverPublishedResolvesAndRestarts(t *testing.T) {
	d, b, cfg, _ := fixture(t)
	ctx := context.Background()
	key := cfg.ContentRoot + "doomed"
	b.current[key] = Object{Key: key, ETag: `"etag-doomed"`, Size: 4}
	in := storage.Mutation{ID: "delete-op/entry/0", Path: "doomed", Conditions: storage.Conditions{ExpectedRevision: nativeRevision(`"etag-doomed"`)}}
	lostCheckpoint := func(context.Context, []byte) error { return errors.New("checkpoint store unavailable") }
	if _, err := d.Remove(ctx, in, lostCheckpoint); err == nil {
		t.Fatal("a lost checkpoint was reported as success")
	}
	ledger, err := d.load("delete", in.ID)
	if err != nil || ledger.Phase != "ready" {
		t.Fatalf("ledger after the lost checkpoint=%+v %v", ledger, err)
	}
	effect := storage.EffectIntent{ID: in.ID, Action: "delete", Source: in.Path, SourceRevision: in.Conditions.ExpectedRevision}
	inspected, err := d.InspectEffect(ctx, effect, nil)
	if err != nil || inspected.State != storage.NotApplied {
		t.Fatalf("inspection of an unpublished delete=%+v %v", inspected, err)
	}
	if _, ok := b.current[key]; !ok {
		t.Fatal("inspection removed the object")
	}
	result, err := d.Remove(ctx, in, cpOK)
	if err != nil || len(result.Evidence) == 0 {
		t.Fatalf("restarted delete=%+v %v", result, err)
	}
	if _, ok := b.current[key]; ok {
		t.Fatal("restarted delete did not remove the object")
	}
	if b.deletes != 1 {
		t.Fatalf("native deletes=%d", b.deletes)
	}
}

// Without the per-endpoint evidence, a listing's ETag is not trusted: the entry is
// observed exactly as before. That is what keeps the fast path a decision about one
// connection instead of an assumption about every provider.
func TestListObservesEveryChildWithoutListingETagEvidence(t *testing.T) {
	d, b, cfg := fixtureWithTree(t, false, func(tree *fakeTree) storage.Driver { return listETagTree{tree} })
	ctx := context.Background()
	for _, name := range []string{"a.txt", "b.txt"} {
		key := cfg.ContentRoot + name
		b.current[key] = Object{Key: key, ETag: `"etag-` + name + `"`, Size: 3}
	}
	page, err := d.List(ctx, "", storage.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if b.heads != 2 {
		t.Fatalf("heads=%d, want one observation per child without evidence", b.heads)
	}
	for _, entry := range page.Entries {
		want := nativeRevision(`"etag-` + string(entry.Path) + `"`)
		if string(entry.Revision) != string(want) {
			t.Fatalf("%s revision=%q want %q", entry.Path, entry.Revision, want)
		}
	}
}

// The purge's deletion is one request: no ledger, no re-observation. What it may not skip
// is the identity check, so a revision that moved is a refusal the caller adopts.
func TestPurgeIsOneConditionalDeleteWithoutObservation(t *testing.T) {
	d, b, cfg, _ := fixture(t)
	ctx := context.Background()
	key := cfg.ContentRoot + "gone.txt"
	b.current[key] = Object{Key: key, ETag: `"etag"`, Size: 3}
	heads, deletes := b.heads, b.deletes
	if err := d.Purge(ctx, "gone.txt", storage.File, nativeRevision(`"etag"`)); err != nil {
		t.Fatal(err)
	}
	if b.heads != heads {
		t.Fatalf("purge observed the object %d times", b.heads-heads)
	}
	if b.deletes != deletes+1 {
		t.Fatalf("purge issued %d deletes", b.deletes-deletes)
	}
	if _, ok := b.current[key]; ok {
		t.Fatal("purge left the object")
	}
	// A revision that no longer matches is refused rather than deleted on trust.
	b.current[key] = Object{Key: key, ETag: `"rewritten"`, Size: 3}
	if err := d.Purge(ctx, "gone.txt", storage.File, nativeRevision(`"etag"`)); !storage.IsCode(err, "changed") {
		t.Fatalf("stale purge=%v", err)
	}
	if _, ok := b.current[key]; !ok {
		t.Fatal("stale purge deleted the object")
	}
}
