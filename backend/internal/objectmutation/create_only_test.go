package objectmutation

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

// unversionedBackend models ordinary COS keys without retained generations. The
// publication hook can install a competing external object after preflight checks.
type unversionedBackend struct {
	*nativeTestBackend
	beforePublish func(Put)
}

func (b *unversionedBackend) Put(ctx context.Context, in Put) (Object, error) {
	if b.beforePublish != nil {
		b.beforePublish(in)
	}
	out, err := b.fakeBackend.Put(ctx, in)
	return b.withoutVersion(in.Key, out, err)
}
func (b *unversionedBackend) CompleteMultipart(ctx context.Context, in Put, id string, parts []NativePart) (Object, error) {
	if b.beforePublish != nil {
		b.beforePublish(in)
	}
	out, err := b.nativeTestBackend.CompleteMultipart(ctx, in, id, parts)
	return b.withoutVersion(in.Key, out, err)
}
func (b *unversionedBackend) withoutVersion(key string, out Object, err error) (Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if current, ok := b.current[key]; ok {
		data := bytes.Clone(b.versions[key][current.VersionID])
		current.VersionID = "null"
		b.current[key] = current
		b.versions[key] = map[string][]byte{"null": data}
	}
	out.VersionID = "null"
	return out, err
}
func (b *unversionedBackend) external(key, content string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.current[key] = Object{Key: key, VersionID: "null", ETag: "external", Size: int64(len(content)), MediaType: "text/plain"}
	b.versions[key] = map[string][]byte{"null": []byte(content)}
}

type unversionedTree struct{ *fakeTree }

func (t *unversionedTree) Open(ctx context.Context, path storage.Path, options storage.ReadOptions) (storage.Read, error) {
	if len(options.Version) != 0 {
		return storage.Read{}, fail("unsupported", path)
	}
	entry, err := t.Stat(ctx, path)
	if err != nil {
		return storage.Read{}, err
	}
	body, err := t.backend.Open(ctx, t.root+string(path), "null")
	if err != nil {
		return storage.Read{}, err
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		return storage.Read{}, err
	}
	length := int64(len(data)) - options.Offset
	if options.Length != nil {
		length = *options.Length
	}
	if options.Offset < 0 || length < 0 || options.Offset+length > int64(len(data)) {
		return storage.Read{}, fail("invalid_range", path)
	}
	entry.NativeRef = []byte(t.root + string(path))
	return storage.Read{Body: io.NopCloser(bytes.NewReader(data[options.Offset : options.Offset+length])), Entry: entry, Offset: options.Offset, Length: length}, nil
}

func createOnlyFixture(t *testing.T) (*Driver, *unversionedBackend, storage.Config, Options) {
	t.Helper()
	backend := &unversionedBackend{nativeTestBackend: &nativeTestBackend{fakeBackend: newFake()}}
	binding := storage.Config{OwnerID: "owner", ConnectionID: "fixture", Generation: 1, ContentRoot: "drive/owner/", SystemRoot: "system/owner/"}
	admission := validAdmissionFixture()
	admission.CreateOnly = true
	admission.VersionedReads = false
	admission.CurrentKeyDelete = false
	admission.CoordinatedExclusiveWriter = false
	admission.LegacyWritersDrained = false
	admission.MultipartUploads = true
	options := Options{SpoolRoot: t.TempDir(), MaxPublishBytes: 1024, Admission: admission}
	driver, err := New(&unversionedTree{&fakeTree{backend: backend.fakeBackend, root: binding.ContentRoot}}, binding, backend, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { driver.Close() })
	return driver, backend, binding, options
}

func createOnlyStage(t *testing.T, d *Driver, in storage.WriteIntent, content string, multipart bool) storage.Staged {
	t.Helper()
	if !multipart {
		stage, err := d.Stage(context.Background(), in, strings.NewReader(content), cpOK)
		if err != nil {
			t.Fatal(err)
		}
		return stage
	}
	metadata := &memoryUploadMetadata{}
	var parts []storage.UploadPart
	if in.Size > 0 {
		part, err := d.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: in.Size}, strings.NewReader(content), metadata, keepPart)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, part)
	}
	stage, err := d.SealStage(context.Background(), in, parts, metadata, cpOK)
	if err != nil {
		t.Fatal(err)
	}
	return stage
}

func TestCreateOnlyUnversionedPublicationAndExternalChanges(t *testing.T) {
	for _, transport := range []string{"staged", "multipart", "empty"} {
		t.Run(transport, func(t *testing.T) {
			d, b, binding, options := createOnlyFixture(t)
			ctx := context.Background()
			b.external(binding.ContentRoot+"folder/existing", "external tree")
			parent, err := d.Stat(ctx, "folder")
			if err != nil || parent.Kind != storage.Folder || len(parent.Version) != 0 {
				t.Fatalf("implicit parent: %+v %v", parent, err)
			}
			content := "original bytes"
			if transport == "empty" {
				content = ""
			}
			in := createIntent("new-file", "folder/中文 +%2F.mov", content)
			in.ParentPath, in.ParentVersion = parent.Path, parent.Version
			stage := createOnlyStage(t, d, in, content, transport != "staged")
			result, err := d.Publish(ctx, stage, cpOK)
			if err != nil || result.Entry == nil || result.Entry.Size != in.Size || len(result.Entry.Version) != 0 {
				t.Fatalf("unversioned publish: %+v %v", result, err)
			}
			if b.heads != 0 {
				t.Fatal("unversioned publication used a later HEAD as outcome evidence")
			}
			if transport == "multipart" && (b.completeCalls != 1 || b.puts != 0) {
				t.Fatal("native parts were published through a whole-file PUT")
			}
			read, err := d.Open(ctx, in.Target, storage.ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(read.Body)
			read.Body.Close()
			if err != nil || string(data) != content || len(read.Entry.Version) != 0 {
				t.Fatalf("ordinary native content: %q %v", data, err)
			}
			page, err := d.List(ctx, "folder", storage.PageRequest{})
			if err != nil || len(page.Entries) != 2 {
				t.Fatalf("implicit folder listing: %+v %v", page, err)
			}
			b.external(binding.ContentRoot+string(in.Target), "outside replacement")
			restarted, err := New(&unversionedTree{&fakeTree{backend: b.fakeBackend, root: binding.ContentRoot}}, binding, b, options)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			inspection, err := restarted.Inspect(ctx, in, result.Evidence)
			if err != nil || inspection.State != storage.Applied || inspection.Result.Entry.Size != in.Size || len(inspection.Result.Entry.Version) != 0 {
				t.Fatalf("external replacement changed saved publication outcome: %+v %v", inspection, err)
			}
			calls := b.puts + b.completeCalls
			if _, err = restarted.Publish(ctx, stage, cpOK); err != nil || b.puts+b.completeCalls != calls {
				t.Fatalf("successful operation replayed native publication: %v", err)
			}
			if b.heads != 0 {
				t.Fatal("recovery attributed an external object's HEAD to the upload")
			}
		})
	}
}

func TestCreateOnlyNativeConditionProtectsExternalWriters(t *testing.T) {
	for _, multipart := range []bool{false, true} {
		t.Run(map[bool]string{false: "staged", true: "multipart"}[multipart], func(t *testing.T) {
			d, b, binding, _ := createOnlyFixture(t)
			in := nativeIntent(t, d, "competing", 4)
			stage := createOnlyStage(t, d, in, "ours", multipart)
			b.beforePublish = func(p Put) {
				if !p.Absent || p.ExpectedETag != "" || p.Key != binding.ContentRoot+string(in.Target) {
					t.Fatal("final request did not bind native create-only semantics")
				}
				b.external(p.Key, "outside")
			}
			_, err := d.Publish(context.Background(), stage, cpOK)
			requireCode(t, err, "precondition_failed")
			inspection, err := d.Inspect(context.Background(), in, stage.Handle)
			if err != nil || inspection.State != storage.NotApplied {
				t.Fatalf("definite native conflict became unknown: %+v %v", inspection, err)
			}
			b.mu.Lock()
			data := string(b.versions[binding.ContentRoot+string(in.Target)]["null"])
			b.mu.Unlock()
			if data != "outside" {
				t.Fatal("external file was overwritten")
			}
		})
	}
}

func TestCreateOnlyUnknownCompleteRemainsFencedAcrossRestart(t *testing.T) {
	d, b, binding, options := createOnlyFixture(t)
	in := nativeIntent(t, d, "unknown", 4)
	stage := createOnlyStage(t, d, in, "ours", true)
	b.failComplete = true
	if _, err := d.Publish(context.Background(), stage, cpOK); err == nil {
		t.Fatal("lost completion reply reported success")
	}
	b.external(binding.ContentRoot+string(in.Target), "external replacement")
	restarted, err := New(&unversionedTree{&fakeTree{backend: b.fakeBackend, root: binding.ContentRoot}}, binding, b, options)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	inspection, err := restarted.Inspect(context.Background(), in, stage.Handle)
	if err != nil || inspection.State != storage.OutcomeUnknown {
		t.Fatalf("unversioned HEAD resolved unknown publication: %+v %v", inspection, err)
	}
	_, err = restarted.Publish(context.Background(), stage, cpOK)
	requireCode(t, err, "outcome_unknown")
	requireCode(t, restarted.Discard(context.Background(), in, stage.Handle), "outcome_unknown")
	if b.completeCalls != 1 || b.heads != 0 {
		t.Fatal("unknown completion replayed or used current object as terminal evidence")
	}
}

func TestCreateOnlyRejectsOtherMutationsBeforeNativeEffects(t *testing.T) {
	d, b, _, _ := createOnlyFixture(t)
	descriptor := d.Describe()
	if !descriptor.CreateOnly || !descriptor.AtomicCreate || descriptor.AtomicReplace || descriptor.PinnedRead || descriptor.RetainsVersions {
		t.Fatalf("overstated create-only capabilities: %+v", descriptor)
	}
	in := createIntent("replacement", "occupied", "ours")
	in.Conditions = storage.Conditions{ExpectedVersion: []byte("native-version:pretend")}
	_, err := d.Stage(context.Background(), in, strings.NewReader("ours"), cpOK)
	requireCode(t, err, "unsupported")
	_, err = d.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: 4}, strings.NewReader("ours"), &memoryUploadMetadata{}, keepPart)
	requireCode(t, err, "unsupported")
	_, err = d.Mkdir(context.Background(), storage.Mutation{ID: "mkdir", Path: "folder", Conditions: storage.Conditions{Absent: true}}, cpOK)
	requireCode(t, err, "unsupported")
	_, err = d.Copy(context.Background(), storage.MoveRequest{ID: "copy", Source: "old", Target: "new", SourceVersion: []byte("native-version:pretend"), TargetConditions: storage.Conditions{Absent: true}}, cpOK)
	requireCode(t, err, "unsupported")
	_, err = d.Remove(context.Background(), storage.Mutation{ID: "delete", Path: "occupied", Conditions: in.Conditions}, cpOK)
	requireCode(t, err, "unsupported")
	_, err = d.Move(context.Background(), storage.MoveRequest{}, cpOK)
	requireCode(t, err, "unsupported")
	if b.puts != 0 || b.deletes != 0 || b.beginCalls != 0 || b.heads != 0 {
		t.Fatal("unsupported mutation reached the object backend")
	}
}
