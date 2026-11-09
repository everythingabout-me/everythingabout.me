package localstorage

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

func fixture(t *testing.T, options Options) (*Factory, storage.Config) {
	t.Helper()
	base := t.TempDir()
	cfg := storage.Config{OwnerID: "owner-a", ConnectionID: "local-test", Generation: 1, ContentRoot: filepath.Join(base, "content"), SystemRoot: filepath.Join(base, "system")}
	for _, root := range []string{cfg.ContentRoot, cfg.SystemRoot} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	factory, err := NewFactory(options)
	if err != nil {
		t.Fatal(err)
	}
	return factory, cfg
}

func bind(t *testing.T, factory *Factory, cfg storage.Config) storage.Driver {
	t.Helper()
	driver, err := factory.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := driver.Close(); err != nil {
			t.Error(err)
		}
	})
	return driver
}

func writeFixture(t *testing.T, root, relative, contents string) {
	t.Helper()
	native := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(native), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if !storage.IsCode(err, code) {
		t.Fatalf("error = %v, want typed %s", err, code)
	}
}

func readContents(t *testing.T, tree storage.Tree, path storage.Path, options storage.ReadOptions) (storage.Read, string) {
	t.Helper()
	result, err := tree.Open(context.Background(), path, options)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	data, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) != result.Length {
		t.Fatalf("read length = %d, descriptor = %d", len(data), result.Length)
	}
	return result, string(data)
}

func TestNativeTreeConformanceAndDirectExit(t *testing.T) {
	factory, cfg := fixture(t, Options{})
	files := map[string]string{" folder /子目录/ cafe\u0301 +#?%.txt": "original\x00raw\xffbytes", " folder /zero": "", "plain.txt": "plain", "copied.txt": "original\x00raw\xffbytes"}
	for name, contents := range files {
		writeFixture(t, cfg.ContentRoot, name, contents)
	}
	if err := os.Mkdir(filepath.Join(cfg.ContentRoot, "empty folder"), 0700); err != nil {
		t.Fatal(err)
	}
	// Capture native spelling and metadata independently, before any adapter exists.
	native := make(map[string]os.FileInfo)
	err := filepath.Walk(cfg.ContentRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(cfg.ContentRoot, path)
		if err != nil {
			return err
		}
		if relative != "." {
			native[filepath.ToSlash(relative)] = info
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	driver := bind(t, factory, cfg)
	descriptor := driver.Describe()
	if !descriptor.RangeRead || descriptor.PinnedRead || descriptor.AtomicCreate || descriptor.AtomicReplace || descriptor.NativeConditionalMutation || descriptor.NativeTreeMove {
		t.Fatalf("unsafe descriptor: %+v", descriptor)
	}
	if descriptor.Names.CaseSensitive || descriptor.Names.MaxSegmentBytes != 255 || descriptor.Names.MaxPathBytes != 4096 || descriptor.Names.MaxDepth != 256 {
		t.Fatalf("unexpected name rules: %+v", descriptor.Names)
	}
	root, err := driver.Stat(context.Background(), "")
	if err != nil || root.Kind != storage.Folder || root.Path != "" {
		t.Fatalf("root = %+v, %v", root, err)
	}
	var scan func(storage.Path)
	seen := make(map[string]bool)
	scan = func(parent storage.Path) {
		page, err := driver.List(context.Background(), parent, storage.PageRequest{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		if page.NextCursor != "" {
			t.Fatal("unexpected pagination in small fixture")
		}
		for _, e := range page.Entries {
			info, ok := native[string(e.Path)]
			if !ok {
				t.Fatalf("non-native spelling %q", e.Path)
			}
			seen[string(e.Path)] = true
			stat, err := driver.Stat(context.Background(), e.Path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stat, e) || !stat.ModifiedAt.Equal(info.ModTime()) || len(e.Revision) != 0 {
				t.Fatalf("metadata differs for %q", e.Path)
			}
			if e.Kind == storage.Folder {
				scan(e.Path)
				continue
			}
			if e.Size != info.Size() {
				t.Fatalf("size differs for %q", e.Path)
			}
			result, data := readContents(t, driver, e.Path, storage.ReadOptions{})
			raw, err := os.ReadFile(filepath.Join(cfg.ContentRoot, string(e.Path)))
			if err != nil {
				t.Fatal(err)
			}
			if data != string(raw) || result.Entry.Path != e.Path || result.Offset != 0 {
				t.Fatalf("native bytes differ for %q", e.Path)
			}
		}
	}
	scan("")
	if len(seen) != len(native) {
		t.Fatalf("scanned %d entries, native tree has %d", len(seen), len(native))
	}
	empty, err := driver.List(context.Background(), "empty folder", storage.PageRequest{})
	if err != nil || len(empty.Entries) != 0 || empty.NextCursor != "" {
		t.Fatalf("empty folder = %+v, %v", empty, err)
	}
	if err := driver.Close(); err != nil {
		t.Fatal(err)
	}
	// Plain filesystem reads require no DB, manifest or adapter after Close.
	for name, old := range native {
		now, err := os.Stat(filepath.Join(cfg.ContentRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(old, now) || !old.ModTime().Equal(now.ModTime()) {
			t.Fatalf("read rewrote native file %q", name)
		}
	}
	// A pre-existing ordinary copy remains independent when the source is edited.
	writeFixture(t, cfg.ContentRoot, " folder /子目录/ cafe\u0301 +#?%.txt", "changed source")
	copied, err := os.ReadFile(filepath.Join(cfg.ContentRoot, "copied.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(copied) != files["copied.txt"] {
		t.Fatal("ordinary copied file changed with source")
	}
	system, err := os.ReadDir(cfg.SystemRoot)
	if err != nil || len(system) != 0 {
		t.Fatalf("read created system material: %v, %v", system, err)
	}
}

func TestRangesVersionsAndTypedErrors(t *testing.T) {
	factory, cfg := fixture(t, Options{})
	writeFixture(t, cfg.ContentRoot, "file", "0123456789")
	writeFixture(t, cfg.ContentRoot, "empty", "")
	if err := os.Mkdir(filepath.Join(cfg.ContentRoot, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	driver := bind(t, factory, cfg)
	ptr := func(n int64) *int64 { return &n }
	for _, tc := range []struct {
		name    string
		path    storage.Path
		options storage.ReadOptions
		want    string
		code    string
	}{
		{name: "full", path: "file", want: "0123456789"},
		{name: "suffix", path: "file", options: storage.ReadOptions{Offset: 7}, want: "789"},
		{name: "section", path: "file", options: storage.ReadOptions{Offset: 2, Length: ptr(4)}, want: "2345"},
		{name: "last byte", path: "file", options: storage.ReadOptions{Offset: 9, Length: ptr(1)}, want: "9"},
		{name: "empty eof", path: "file", options: storage.ReadOptions{Offset: 10}, want: ""},
		{name: "zero file", path: "empty", want: ""},
		{name: "past end", path: "file", options: storage.ReadOptions{Offset: 11}, code: "invalid_range"},
		{name: "too long", path: "file", options: storage.ReadOptions{Offset: 7, Length: ptr(4)}, code: "invalid_range"},
		{name: "negative offset", path: "file", options: storage.ReadOptions{Offset: -1}, code: "invalid_range"},
		{name: "zero length", path: "file", options: storage.ReadOptions{Length: ptr(0)}, code: "invalid_range"},
		{name: "negative length", path: "file", options: storage.ReadOptions{Length: ptr(-1)}, code: "invalid_range"},
		{name: "overflow", path: "file", options: storage.ReadOptions{Offset: 1, Length: ptr(math.MaxInt64)}, code: "invalid_range"},
		{name: "version", path: "file", options: storage.ReadOptions{PinnedVersion: []byte("mtime-size-is-not-immutable")}, code: "unsupported"},
		{name: "missing", path: "missing", code: "not_found"},
		{name: "folder", path: "folder", code: "not_file"},
		{name: "root", path: "", code: "not_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := driver.Open(context.Background(), tc.path, tc.options)
			if tc.code != "" {
				requireCode(t, err, tc.code)
				if result.Body != nil {
					result.Body.Close()
					t.Fatal("failed read returned a body")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer result.Body.Close()
			data, err := io.ReadAll(result.Body)
			if err != nil || string(data) != tc.want {
				t.Fatalf("data=%q err=%v want=%q", data, err, tc.want)
			}
			if result.Offset != tc.options.Offset || result.Length != int64(len(tc.want)) || len(result.Entry.Revision) != 0 {
				t.Fatalf("read descriptor = %+v", result)
			}
			if tc.path == "file" && result.Entry.Size != 10 {
				t.Fatal("section metadata lost full size")
			}
		})
	}
	_, err := driver.Stat(context.Background(), "missing")
	requireCode(t, err, "not_found")
	_, err = driver.List(context.Background(), "missing", storage.PageRequest{})
	requireCode(t, err, "not_found")
	_, err = driver.List(context.Background(), "file", storage.PageRequest{})
	requireCode(t, err, "not_folder")
	for _, path := range []storage.Path{"/file", "../file", "folder/../file", "a//b", "a\\b", "bad\x00name", storage.Path(strings.Repeat("a", 256)), storage.Path(strings.Repeat("a/", 256) + "x")} {
		_, err := driver.Stat(context.Background(), path)
		requireCode(t, err, "invalid_name")
		_, err = driver.List(context.Background(), path, storage.PageRequest{})
		requireCode(t, err, "invalid_name")
		_, err = driver.Open(context.Background(), path, storage.ReadOptions{})
		requireCode(t, err, "invalid_name")
	}
}

func TestExternalTruncationReportsIncompleteRange(t *testing.T) {
	factory, cfg := fixture(t, Options{})
	writeFixture(t, cfg.ContentRoot, "file", "0123456789")
	tree := bind(t, factory, cfg)
	result, err := tree.Open(context.Background(), "file", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	if result.Length != 10 || result.Entry.Size != 10 {
		t.Fatal("initial representation length is wrong")
	}
	if err := os.Truncate(filepath.Join(cfg.ContentRoot, "file"), 3); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(result.Body)
	if string(data) != "012" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated read = %q, %v", data, err)
	}
}

func TestManyChildrenCursorValidationAndBindings(t *testing.T) {
	factory, cfg := fixture(t, Options{})
	const count = 413
	for i := 0; i < count; i++ {
		writeFixture(t, cfg.ContentRoot, fmt.Sprintf("child %04d", count-i-1), "x")
	}
	if err := os.Mkdir(filepath.Join(cfg.ContentRoot, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	driver := bind(t, factory, cfg)
	first, err := driver.List(context.Background(), "", storage.PageRequest{Limit: 17})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != 17 || first.NextCursor == "" {
		t.Fatalf("first page = %+v", first)
	}
	names := make([]string, 0, count+1)
	token := ""
	for pages := 0; ; pages++ {
		if pages > count {
			t.Fatal("cursor did not finish")
		}
		page, err := driver.List(context.Background(), "", storage.PageRequest{Limit: 17, Cursor: token})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Entries) > 17 {
			t.Fatal("page exceeded requested limit")
		}
		for _, e := range page.Entries {
			names = append(names, string(e.Path))
			if strings.Contains(string(e.Path), "/") {
				t.Fatal("listing recursed")
			}
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == token {
			t.Fatal("cursor did not advance")
		}
		token = page.NextCursor
	}
	if len(names) != count+1 || !sort.StringsAreSorted(names) {
		t.Fatalf("listed %d names, sorted=%v", len(names), sort.StringsAreSorted(names))
	}
	for i := 1; i < len(names); i++ {
		if names[i] == names[i-1] {
			t.Fatal("duplicate paginated name")
		}
	}
	for _, bad := range []string{"not base64", "a.b.c", first.NextCursor + "=", strings.Repeat("x", 9000), base64.RawURLEncoding.EncodeToString([]byte(`{"v":1}`)) + "." + strings.Repeat("A", 43)} {
		page, err := driver.List(context.Background(), "", storage.PageRequest{Cursor: bad, Limit: 1})
		requireCode(t, err, "invalid_cursor")
		if len(page.Entries) != 0 {
			t.Fatal("invalid cursor returned partial entries")
		}
	}
	_, err = driver.List(context.Background(), "sub", storage.PageRequest{Cursor: first.NextCursor})
	requireCode(t, err, "invalid_cursor")
	for _, change := range []func(*storage.Config){func(c *storage.Config) { c.OwnerID = "owner-b" }, func(c *storage.Config) { c.Generation++ }, func(c *storage.Config) { c.ConnectionID = "another-connection" }} {
		changed := cfg
		change(&changed)
		other := bind(t, factory, changed)
		_, err := other.List(context.Background(), "", storage.PageRequest{Cursor: first.NextCursor})
		requireCode(t, err, "invalid_cursor")
	}
	for _, limit := range []int{-1, 1001} {
		_, err := driver.List(context.Background(), "", storage.PageRequest{Limit: limit})
		requireCode(t, err, "invalid_page")
	}
	// No snapshot claim: a later entry can appear after the first page was issued.
	writeFixture(t, cfg.ContentRoot, "zz external", "external")
	page, err := driver.List(context.Background(), "", storage.PageRequest{Cursor: first.NextCursor, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if page.Entries[len(page.Entries)-1].Path != "zz external" {
		t.Fatal("live listing did not observe external addition")
	}
}

func TestCancellationAdmissionAndClose(t *testing.T) {
	factory, cfg := fixture(t, Options{MaxConcurrentOperations: 1})
	writeFixture(t, cfg.ContentRoot, "file", strings.Repeat("0123456789", 100))
	driver := bind(t, factory, cfg)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := driver.Stat(canceled, "file")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stat canceled = %v", err)
	}
	_, err = driver.List(canceled, "", storage.PageRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("List canceled = %v", err)
	}
	_, err = driver.Open(canceled, "file", storage.ReadOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Open canceled = %v", err)
	}
	ctx, cancelRead := context.WithCancel(context.Background())
	result, err := driver.Open(ctx, "file", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, stopWait := context.WithTimeout(context.Background(), 25*time.Millisecond)
	_, err = driver.Stat(waitCtx, "file")
	stopWait()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pool admission did not wait: %v", err)
	}
	cancelRead()
	_, err = result.Body.Read(make([]byte, 10))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled body = %v", err)
	}
	// Cancellation releases the admission slot even if a caller forgot Close.
	afterCtx, stopAfter := context.WithTimeout(context.Background(), time.Second)
	defer stopAfter()
	_, err = driver.Stat(afterCtx, "file")
	if err != nil {
		t.Fatalf("canceled read retained resources: %v", err)
	}
	result.Body.Close()
	active, err := driver.Open(context.Background(), "file", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = active.Body.Read(make([]byte, 1))
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("driver Close left read active: %v", err)
	}
	active.Body.Close()
	_, err = driver.Stat(context.Background(), "file")
	requireCode(t, err, "closed")
}

func TestConfigValidationCanonicalSeparationAndNoCreation(t *testing.T) {
	factory, cfg := fixture(t, Options{})
	for _, change := range []func(*storage.Config){
		func(c *storage.Config) { c.OwnerID = "" }, func(c *storage.Config) { c.OwnerID = "../owner" }, func(c *storage.Config) { c.OwnerID = " owner " },
		func(c *storage.Config) { c.ConnectionID = "" }, func(c *storage.Config) { c.ConnectionID = "bad\x00id" }, func(c *storage.Config) { c.Generation = 0 },
		func(c *storage.Config) { c.ContentRoot = "relative" }, func(c *storage.Config) { c.SystemRoot = "relative" },
		func(c *storage.Config) { c.SystemRoot = c.ContentRoot }, func(c *storage.Config) { c.SystemRoot = filepath.Dir(c.ContentRoot) },
		func(c *storage.Config) { c.ContentRoot = filepath.Dir(c.SystemRoot) },
		func(c *storage.Config) { c.ContentRoot = filepath.Join(c.ContentRoot, "missing") },
	} {
		changed := cfg
		change(&changed)
		driver, err := factory.Open(context.Background(), changed)
		if driver != nil {
			driver.Close()
			t.Fatal("invalid binding opened")
		}
		requireCode(t, err, "invalid_config")
	}
	if _, err := os.Stat(filepath.Join(cfg.ContentRoot, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing root was created")
	}
	nested := filepath.Join(cfg.ContentRoot, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	changed := cfg
	changed.SystemRoot = nested
	_, err := factory.Open(context.Background(), changed)
	requireCode(t, err, "invalid_config")
	alias := filepath.Join(filepath.Dir(cfg.ContentRoot), "alias")
	if err := os.Symlink(cfg.ContentRoot, alias); err != nil {
		t.Fatal(err)
	}
	changed = cfg
	changed.SystemRoot = alias
	_, err = factory.Open(context.Background(), changed)
	requireCode(t, err, "invalid_config")
	changed = cfg
	changed.ContentRoot = alias
	changed.SystemRoot = nested
	_, err = factory.Open(context.Background(), changed)
	requireCode(t, err, "invalid_config")
	// A symlinked ancestor is resolved physically before separation is checked.
	changed = cfg
	changed.ContentRoot = filepath.Join(alias, "nested")
	changed.SystemRoot = cfg.ContentRoot
	_, err = factory.Open(context.Background(), changed)
	requireCode(t, err, "invalid_config")
	for _, options := range []Options{{MaxDirectoryEntries: -1}, {MaxConcurrentOperations: -1}, {MaxConcurrentOperations: 4097}, {MaxDirectoryEntries: 1000001}} {
		_, err := NewFactory(options)
		requireCode(t, err, "invalid_config")
	}
}

func TestSymlinksNeverExposeSystemOrOutside(t *testing.T) {
	for _, target := range []string{"outside", "system", "inside"} {
		t.Run(target, func(t *testing.T) {
			factory, cfg := fixture(t, Options{})
			writeFixture(t, cfg.ContentRoot, "safe", "safe")
			writeFixture(t, cfg.SystemRoot, "secret", "secret")
			outside := filepath.Join(filepath.Dir(cfg.ContentRoot), "outside")
			if err := os.Mkdir(outside, 0700); err != nil {
				t.Fatal(err)
			}
			writeFixture(t, outside, "secret", "outside secret")
			linkTarget := outside
			if target == "system" {
				linkTarget = cfg.SystemRoot
			}
			if target == "inside" {
				linkTarget = "real"
				writeFixture(t, cfg.ContentRoot, "real/secret", "inside")
			}
			if err := os.Symlink(linkTarget, filepath.Join(cfg.ContentRoot, "link")); err != nil {
				t.Fatal(err)
			}
			driver := bind(t, factory, cfg)
			for _, path := range []storage.Path{"link", "link/secret"} {
				_, err := driver.Stat(context.Background(), path)
				requireCode(t, err, "unsupported")
				read, err := driver.Open(context.Background(), path, storage.ReadOptions{})
				requireCode(t, err, "unsupported")
				if read.Body != nil {
					read.Body.Close()
					t.Fatal("symlink returned a body")
				}
			}
			page, err := driver.List(context.Background(), "", storage.PageRequest{Limit: 1})
			requireCode(t, err, "unsupported")
			if len(page.Entries) != 0 || page.NextCursor != "" {
				t.Fatal("unsafe listing returned a partial page")
			}
		})
	}
}

func TestNamesCollisionAndExactSpelling(t *testing.T) {
	factory, cfg := fixture(t, Options{})
	tree := bind(t, factory, cfg)
	local := tree.(*driver)
	for _, pair := range [][2]string{{"cafe\u0301", "caf\u00e9"}, {"Report", "report"}, {"Straße", "STRASSE"}} {
		if local.key(pair[0]) != local.key(pair[1]) {
			t.Fatalf("equivalent names not detected: %q", pair)
		}
	}
	writeFixture(t, cfg.ContentRoot, "Exact Name", "x")
	_, err := tree.Stat(context.Background(), "exact name")
	requireCode(t, err, "invalid_name")
	for _, pair := range [][2]string{{"collision", "COLLISION"}, {"cafe\u0301", "caf\u00e9"}} {
		t.Run(pair[0], func(t *testing.T) {
			f, c := fixture(t, Options{})
			writeFixture(t, c.ContentRoot, pair[0], "a")
			writeFixture(t, c.ContentRoot, pair[1], "b")
			entries, err := os.ReadDir(c.ContentRoot)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 2 {
				t.Skip("native filesystem does not admit distinct equivalent spellings")
			}
			d := bind(t, f, c)
			page, err := d.List(context.Background(), "", storage.PageRequest{Limit: 1})
			requireCode(t, err, "ambiguous_path")
			if len(page.Entries) != 0 || page.NextCursor != "" {
				t.Fatal("collision yielded partial scan")
			}
			_, err = d.Stat(context.Background(), storage.Path(entries[0].Name()))
			requireCode(t, err, "ambiguous_path")
			_, err = d.Open(context.Background(), storage.Path(entries[0].Name()), storage.ReadOptions{})
			requireCode(t, err, "ambiguous_path")
		})
	}
	sensitiveFactory, sensitiveCfg := fixture(t, Options{CaseSensitive: true})
	sensitive := bind(t, sensitiveFactory, sensitiveCfg)
	if !sensitive.Describe().Names.CaseSensitive || sensitive.(*driver).key("A") == sensitive.(*driver).key("a") {
		t.Fatal("explicit case sensitive option ignored")
	}
	if sensitive.(*driver).key("cafe\u0301") != sensitive.(*driver).key("caf\u00e9") {
		t.Fatal("case sensitive option disabled normalization ambiguity checks")
	}
}

func TestDirectoryBoundAndUnsafeNativeNamesFailBeforePartial(t *testing.T) {
	factory, cfg := fixture(t, Options{MaxDirectoryEntries: 2})
	for _, name := range []string{"a", "b", "z"} {
		writeFixture(t, cfg.ContentRoot, name, "x")
	}
	driver := bind(t, factory, cfg)
	page, err := driver.List(context.Background(), "", storage.PageRequest{Limit: 1})
	requireCode(t, err, "resource_limit")
	if len(page.Entries) != 0 {
		t.Fatal("bounded listing leaked partial entries")
	}
	f, c := fixture(t, Options{})
	writeFixture(t, c.ContentRoot, "a valid", "x")
	writeFixture(t, c.ContentRoot, "z\\unsafe", "x")
	d := bind(t, f, c)
	page, err = d.List(context.Background(), "", storage.PageRequest{Limit: 1})
	requireCode(t, err, "invalid_name")
	if len(page.Entries) != 0 {
		t.Fatal("unsafe name leaked partial entries")
	}
}

func TestOwnerRootsAndMovedRootHandlesRemainIndependent(t *testing.T) {
	factory, a := fixture(t, Options{})
	_, b := fixture(t, Options{})
	b.OwnerID = "owner-b"
	writeFixture(t, a.ContentRoot, "same", "owner a")
	writeFixture(t, b.ContentRoot, "same", "owner b")
	da := bind(t, factory, a)
	db := bind(t, factory, b)
	_, got := readContents(t, da, "same", storage.ReadOptions{})
	if got != "owner a" {
		t.Fatal(got)
	}
	_, got = readContents(t, db, "same", storage.ReadOptions{})
	if got != "owner b" {
		t.Fatal(got)
	}
	moved := a.ContentRoot + "-moved"
	if err := os.Rename(a.ContentRoot, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(a.ContentRoot, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, a.ContentRoot, "same", "replacement root")
	_, got = readContents(t, da, "same", storage.ReadOptions{})
	if got != "owner a" {
		t.Fatal("binding followed replacement root")
	}
	fresh := bind(t, factory, a)
	_, got = readContents(t, fresh, "same", storage.ReadOptions{})
	if got != "replacement root" {
		t.Fatal("new binding did not open current root")
	}
	// Even identical config paths do not allow a cursor from the replaced inode.
	writeFixture(t, moved, "second", "x")
	oldPage, err := da.List(context.Background(), "", storage.PageRequest{Limit: 1})
	if err != nil || oldPage.NextCursor == "" {
		t.Fatalf("old root page: %+v %v", oldPage, err)
	}
	_, err = fresh.List(context.Background(), "", storage.PageRequest{Cursor: oldPage.NextCursor})
	requireCode(t, err, "invalid_cursor")
}
