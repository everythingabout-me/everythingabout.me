package objectmutation

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/snapshot"
	"everythingabout.me/backend/internal/storage"
)

// memorySource is a small native tree a snapshot can copy into an object driver. It
// reports listing revisions (the snapshot's remove path must name one) and refuses to
// list a file as a folder, like the real object tree.
type memorySource struct{ files map[storage.Path][]byte }

func (s *memorySource) Stat(_ context.Context, p storage.Path) (storage.Entry, error) {
	if data, ok := s.files[p]; ok {
		return storage.Entry{Path: p, Kind: storage.File, Revision: sourceRevision(p, data), Size: int64(len(data))}, nil
	}
	for f := range s.files {
		if p == "" || strings.HasPrefix(string(f), string(p)+"/") {
			return storage.Entry{Path: p, Kind: storage.Folder, Revision: sourceRevision(p, nil)}, nil
		}
	}
	return storage.Entry{}, fail("not_found", p)
}

func (s *memorySource) List(ctx context.Context, dir storage.Path, _ storage.PageRequest) (storage.Page, error) {
	entry, err := s.Stat(ctx, dir)
	if err != nil {
		return storage.Page{}, err
	}
	if entry.Kind != storage.Folder {
		return storage.Page{}, fail("not_folder", dir)
	}
	seen := map[storage.Path]storage.Entry{}
	for f, data := range s.files {
		if dir != "" && !strings.HasPrefix(string(f), string(dir)+"/") {
			continue
		}
		rest := strings.TrimPrefix(strings.TrimPrefix(string(f), string(dir)), "/")
		if rest == "" {
			continue
		}
		segment, _, deeper := strings.Cut(rest, "/")
		p := storage.Path(segment)
		if dir != "" {
			p = dir + "/" + p
		}
		if deeper {
			seen[p] = storage.Entry{Path: p, Kind: storage.Folder, Revision: sourceRevision(p, nil)}
			continue
		}
		seen[p] = storage.Entry{Path: p, Kind: storage.File, Revision: sourceRevision(p, data), Size: int64(len(data))}
	}
	out := make([]storage.Entry, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return storage.Page{Entries: out}, nil
}

func (s *memorySource) Open(_ context.Context, p storage.Path, _ storage.ReadOptions) (storage.Read, error) {
	data, ok := s.files[p]
	if !ok {
		return storage.Read{}, fail("not_found", p)
	}
	return storage.Read{Body: io.NopCloser(bytes.NewReader(data)), Entry: storage.Entry{Path: p, Kind: storage.File, Revision: sourceRevision(p, data), Size: int64(len(data))}, Length: int64(len(data))}, nil
}

func (s *memorySource) Describe() storage.Descriptor { return storage.Descriptor{} }
func (s *memorySource) Close() error                 { return nil }

func sourceRevision(p storage.Path, data []byte) []byte {
	return []byte("source:" + string(p) + ":" + string(data))
}

// A rotation must delete the objects it reports removed. Before this test the prune's
// Remove named no revision, so the object driver settled each call as an implicit-folder
// deletion: a file was reported removed with no native request, and a folder that still
// had its marker failed the whole prune with "native final effect did not start".
func TestSnapshotPruneDeletesThroughTheObjectDriver(t *testing.T) {
	d, b, cfg, _ := fixture(t)
	ctx := context.Background()
	src := &memorySource{files: map[storage.Path][]byte{
		"Photos/2026/trip.jpg": []byte("jpeg-bytes"),
		"Notes/diary.md":       []byte("text"),
	}}
	result, err := snapshot.Copy(ctx, src, "", d, "2026-11-10")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if result.Files != 2 || result.Folders != 4 {
		t.Fatalf("copy = %+v, want 2 files and 4 folders", result)
	}
	removed, err := snapshot.Prune(ctx, d, d, "", 0)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(removed) != 1 || removed[0] != "2026-11-10" {
		t.Fatalf("removed %v, want the only snapshot", removed)
	}
	var left []string
	for key := range b.current {
		left = append(left, strings.TrimPrefix(key, cfg.ContentRoot))
	}
	sort.Strings(left)
	if len(left) != 0 {
		t.Fatalf("objects left after prune: %v", left)
	}
	// Two file markers and four folder markers (the date, Notes, Photos, Photos/2026)
	// were deleted.
	if b.deletes != 6 {
		t.Fatalf("native deletes = %d, want 6", b.deletes)
	}
	if _, err := d.Stat(ctx, "2026-11-10"); !storage.IsCode(err, "not_found") {
		t.Fatalf("pruned snapshot is still visible: %v", err)
	}
}

// A delete that names no native state is refused, not settled as an implicit folder.
func TestObjectRemoveRefusesARevisionlessDelete(t *testing.T) {
	d, b, cfg, _ := fixture(t)
	ctx := context.Background()
	b.current[cfg.ContentRoot+"keep.txt"] = Object{Key: cfg.ContentRoot + "keep.txt", ETag: "etag-keep", Size: 1}
	b.current[cfg.ContentRoot+"Folder/"] = Object{Key: cfg.ContentRoot + "Folder/", ETag: "etag-folder", Size: 0}
	for _, path := range []storage.Path{"keep.txt", "Folder"} {
		if _, err := d.Remove(ctx, storage.Mutation{ID: "no-revision-" + string(path), Path: path}, cpOK); !storage.IsCode(err, "missing_revision") {
			t.Fatalf("Remove(%q) without a revision = %v, want missing_revision", path, err)
		}
	}
	if b.deletes != 0 || len(b.current) != 2 {
		t.Fatalf("a refused delete changed native state: deletes=%d objects=%d", b.deletes, len(b.current))
	}
	for _, path := range []storage.Path{"keep.txt", "Folder"} {
		if _, err := d.Remove(ctx, storage.Mutation{ID: "implicit-and-revision-" + string(path), Path: path, Conditions: storage.Conditions{ImplicitFolder: true, ExpectedRevision: nativeRevision("etag-keep")}}, cpOK); !storage.IsCode(err, "unsupported") {
			t.Fatalf("implicit delete of %q with a revision = %v, want unsupported", path, err)
		}
	}
}

// The implicit-folder escape hatch is real: it settles only a folder with no marker,
// and only while nothing under it remains.
func TestObjectImplicitFolderNeedsAnExplicitRequest(t *testing.T) {
	d, b, cfg, _ := fixture(t)
	ctx := context.Background()
	b.current[cfg.ContentRoot+"Prefix/child"] = Object{Key: cfg.ContentRoot + "Prefix/child", ETag: "external", Size: 1}
	if _, err := d.Remove(ctx, storage.Mutation{ID: "prefix", Path: "Prefix", Conditions: storage.Conditions{ImplicitFolder: true}}, cpOK); err == nil {
		t.Fatal("a nonempty implicit folder was removed")
	}
	if _, ok := b.current[cfg.ContentRoot+"Prefix/child"]; !ok {
		t.Fatal("the child was deleted with its implicit folder")
	}
	// A file is not a marker-less folder, even when the caller says so.
	b.current[cfg.ContentRoot+"file.txt"] = Object{Key: cfg.ContentRoot + "file.txt", ETag: "etag-file", Size: 1}
	if _, err := d.Remove(ctx, storage.Mutation{ID: "file", Path: "file.txt", Conditions: storage.Conditions{ImplicitFolder: true}}, cpOK); !storage.IsCode(err, "changed") {
		t.Fatalf("implicit delete of a file = %v, want a changed refusal", err)
	}
	if _, ok := b.current[cfg.ContentRoot+"file.txt"]; !ok {
		t.Fatal("the file was deleted as an implicit folder")
	}
}

// A snapshot stages entries one at a time, so the bound it needs is the largest entry
// it admits. The connection's interactive 32 MiB single-PUT policy is a different piece
// of work and used to stop the copy at the first larger file.
func TestSnapshotStagesAnEntryLargerThanTheDefaultSinglePutPolicy(t *testing.T) {
	b := newFake()
	cfg := storage.Config{OwnerID: "owner", ConnectionID: "fixture", Generation: 1, ContentRoot: "drive/owner/", SystemRoot: "system/owner/"}
	options := Options{SpoolRoot: t.TempDir(), Admission: Admission{Version: AdmissionVersion, ConnectionID: cfg.ConnectionID, Endpoint: "https://fixture.invalid", Bucket: "bucket", Region: "region", CheckedAt: time.Now().UTC(), ProbeReference: "isolated-positive-negative-test", AtomicPublish: true, ConditionalCreate: true, SingleGateway: true}}
	d, err := New(&fakeTree{backend: b, root: cfg.ContentRoot}, cfg, b, options)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if got := d.Describe().MaxFileBytes; got != 32<<20 {
		t.Fatalf("interactive publication limit = %d, want the 32 MiB default", got)
	}
	const size = 32<<20 + 1
	src := &memorySource{files: map[storage.Path][]byte{"Big/one.bin": bytes.Repeat([]byte("x"), size)}}
	result, err := snapshot.Copy(context.Background(), src, "", d, "2026-11-10")
	if err != nil {
		t.Fatalf("copy of a %d-byte entry: %v", size, err)
	}
	if result.Files != 1 || result.Bytes != size {
		t.Fatalf("copy = %+v, want one %d-byte file", result, size)
	}
	object, ok := b.current[cfg.ContentRoot+"2026-11-10/Big/one.bin"]
	if !ok || object.Size != size {
		t.Fatalf("staged object = %+v (present=%v), want %d bytes", object, ok, size)
	}
	if err := d.AdmitStagedBytes(singleRequestLimit + 1); !storage.IsCode(err, "too_large") {
		t.Fatalf("bound above the provider single-request limit = %v, want too_large", err)
	}
}

// A rotation and a re-take of the same date must both act. Intent ids used to name only
// the path, so the driver still held the first run's settled create and delete receipts:
// the second copy or prune answered from them instead of writing or deleting anything.
func TestSnapshotCanPruneAndRebuildTheSameDate(t *testing.T) {
	d, b, cfg, _ := fixture(t)
	ctx := context.Background()
	src := &memorySource{files: map[storage.Path][]byte{
		"Notes/diary.md": []byte("text"),
		"Photos/pic.jpg": []byte("jpeg"),
	}}
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := snapshot.Copy(ctx, src, "", d, "2026-11-10"); err != nil {
			t.Fatalf("copy %d: %v", attempt, err)
		}
		if _, ok := b.current[cfg.ContentRoot+"2026-11-10/Notes/diary.md"]; !ok {
			t.Fatalf("copy %d did not write the date", attempt)
		}
		if _, err := snapshot.Prune(ctx, d, d, "", 0); err != nil {
			t.Fatalf("prune %d: %v", attempt, err)
		}
		if len(b.current) != 0 {
			t.Fatalf("objects left after prune %d: %v", attempt, b.current)
		}
	}
}
