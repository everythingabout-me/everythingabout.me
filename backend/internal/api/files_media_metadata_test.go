package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"everythingabout.me/backend/internal/mediameta"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storage"
)

// mediaTestCache is an in-memory mediameta.Cache that records its traffic.
type mediaTestCache struct {
	entries    map[string]mediameta.Info
	gets, puts int
	getErr     error
	putErr     error
}

func (c *mediaTestCache) Get(_ context.Context, owner, entry, key string) (mediameta.Info, bool, error) {
	c.gets++
	if c.getErr != nil {
		return mediameta.Info{}, false, c.getErr
	}
	info, ok := c.entries[owner+"/"+entry+"/"+key]
	return info, ok, nil
}
func (c *mediaTestCache) Put(_ context.Context, owner, entry, key string, info mediameta.Info) error {
	c.puts++
	if c.putErr != nil {
		return c.putErr
	}
	if c.entries == nil {
		c.entries = map[string]mediameta.Info{}
	}
	c.entries[owner+"/"+entry+"/"+key] = info
	return nil
}

// mediaTestJPEG is the smallest JPEG that states a fact: a 640x480 frame header.
func mediaTestJPEG() string {
	return "\xFF\xD8" + "\xFF\xC0\x00\x0B\x08\x01\xE0\x02\x80\x01\x01\x11\x00" + "\xFF\xDA\x00\x08\x01\x01\x00\x00\x3F\x00" + "scan data"
}

type mediaFixture struct {
	driver *referenceContentDriver
	server *Server
	file   *fileRequest
	index  *filesReadTestIndex
	cache  *mediaTestCache
}

func newMediaFixture(name, data string) *mediaFixture {
	key := "drive/owner-a/" + name
	d, s, f := referenceContentFixture(key)
	d.entry.Path, d.entry.Size, d.entry.MediaType, d.data = storage.Path(name), int64(len(data)), "", data
	row := referenceContentRow(name, key, int64(len(data)))
	row.ID = "entry-1"
	index := &filesReadTestIndex{rows: []nodes.Row{row}, revisions: map[string]*string{}}
	cache := &mediaTestCache{}
	s.filesIndex, s.mediaCache = index, cache
	return &mediaFixture{driver: d, server: s, file: f, index: index, cache: cache}
}

func (m *mediaFixture) get(t *testing.T, name string) (map[string]any, error) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/files/media-metadata?"+url.Values{"path": {"/" + name}}.Encode(), nil)
	w := httptest.NewRecorder()
	if err := m.server.handleFilesMediaMetadata(w, r, m.file); err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, w.Body.String())
	}
	return body, nil
}

func TestFilesMediaMetadataParsesOnceThenServesFromTheCache(t *testing.T) {
	m := newMediaFixture("photo.jpg", mediaTestJPEG())
	body, err := m.get(t, "photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if body["state"] != "parsed" || body["id"] != "entry-1" || body["width"] != float64(640) || body["height"] != float64(480) {
		t.Fatalf("first response: %v", body)
	}
	// Absent facts are explicit nulls, so a client can tell "not stated" from "old server".
	for _, field := range []string{"capturedAt", "location", "make", "model", "lens", "durationSeconds", "fNumber", "exposureSeconds", "iso", "focalLength", "focalLength35mm"} {
		if value, present := body[field]; !present || value != nil {
			t.Errorf("field %q = %v (present=%v), want an explicit null", field, value, present)
		}
	}
	if m.driver.statRefs != 1 || m.driver.opens != 1 || m.driver.traversalStats != 0 || m.driver.traversalOpens != 0 {
		t.Fatalf("first parse: %d observations, %d ranged reads, %d+%d traversal calls", m.driver.statRefs, m.driver.opens, m.driver.traversalStats, m.driver.traversalOpens)
	}
	if len(m.driver.options) != 1 || m.driver.options[0].Length == nil {
		t.Fatalf("the parse did not use a ranged read: %+v", m.driver.options)
	}
	if m.cache.puts != 1 {
		t.Fatalf("parsed result was stored %d times", m.cache.puts)
	}

	// Once the index names the same content, a repeat view touches no storage at all.
	m.index.revisions["photo.jpg"] = filesContentRevision(m.file.ContentNamespace, m.driver.entry)
	body, err = m.get(t, "photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if body["state"] != "parsed" || body["width"] != float64(640) {
		t.Fatalf("cached response: %v", body)
	}
	if m.driver.statRefs != 1 || m.driver.opens != 1 {
		t.Fatalf("a cached view reached storage: %d observations, %d reads", m.driver.statRefs, m.driver.opens)
	}
}

func TestFilesMediaMetadataWithoutIndexedRevisionRevalidatesWithOneObservation(t *testing.T) {
	m := newMediaFixture("photo.jpg", mediaTestJPEG())
	if _, err := m.get(t, "photo.jpg"); err != nil {
		t.Fatal(err)
	}
	// The index still has no content revision: the object is observed again, the
	// stored result matches what was observed, and no bytes are transferred.
	if _, err := m.get(t, "photo.jpg"); err != nil {
		t.Fatal(err)
	}
	if m.driver.statRefs != 2 || m.driver.opens != 1 || m.cache.puts != 1 {
		t.Fatalf("revalidation: %d observations, %d reads, %d writes", m.driver.statRefs, m.driver.opens, m.cache.puts)
	}
}

func TestFilesMediaMetadataReparsesReplacedContent(t *testing.T) {
	m := newMediaFixture("photo.jpg", mediaTestJPEG())
	if _, err := m.get(t, "photo.jpg"); err != nil {
		t.Fatal(err)
	}
	m.driver.entry.Revision = []byte("native-etag:replaced")
	if _, err := m.get(t, "photo.jpg"); err != nil {
		t.Fatal(err)
	}
	if m.driver.opens != 2 || m.cache.puts != 2 {
		t.Fatalf("replaced content was served from the old result: %d reads, %d writes", m.driver.opens, m.cache.puts)
	}
}

func TestFilesMediaMetadataSkipsFilesItCannotParse(t *testing.T) {
	m := newMediaFixture("notes.txt", "plain text")
	body, err := m.get(t, "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if body["state"] != "unsupported" {
		t.Fatalf("response: %v", body)
	}
	if m.driver.statRefs+m.driver.opens+m.driver.traversalStats+m.driver.traversalOpens != 0 || m.cache.gets+m.cache.puts != 0 {
		t.Fatal("a document cost a storage or cache request")
	}
}

func TestFilesMediaMetadataEmptyResultIsCachedToo(t *testing.T) {
	m := newMediaFixture("blank.jpg", "this is not a JPEG at all")
	body, err := m.get(t, "blank.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if body["state"] != "empty" || m.cache.puts != 1 {
		t.Fatalf("response %v, %d cache writes: a file with nothing to say must not be re-read on every view", body, m.cache.puts)
	}
}

func TestFilesMediaMetadataMissingObjectIsNativeAbsent(t *testing.T) {
	m := newMediaFixture("photo.jpg", mediaTestJPEG())
	m.file.Driver = &missingReferenceDriver{referenceContentDriver: m.driver}
	_, err := m.get(t, "photo.jpg")
	if problem := filesProblem(err); err == nil || problem.Status != 404 || problem.Code != "native_absent" {
		t.Fatalf("missing object: %v", err)
	}
	if m.cache.puts != 0 {
		t.Fatal("a missing object was cached as a file without metadata")
	}
}

type failingReadDriver struct{ *referenceContentDriver }

func (d *failingReadDriver) OpenReference(_ context.Context, p storage.Path, _ []byte, _ storage.ReadOptions) (storage.Read, error) {
	return storage.Read{}, storage.NewError("provider_unavailable", p, errors.New("connection reset"))
}

func TestFilesMediaMetadataReadFailureIsNotCached(t *testing.T) {
	m := newMediaFixture("photo.jpg", mediaTestJPEG())
	m.file.Driver = &failingReadDriver{referenceContentDriver: m.driver}
	if _, err := m.get(t, "photo.jpg"); err == nil {
		t.Fatal("a failed read answered as if the file had been parsed")
	}
	if m.cache.puts != 0 {
		t.Fatal("a failed read was cached as a file without metadata")
	}
}

func TestFilesMediaMetadataSurvivesACacheOutage(t *testing.T) {
	m := newMediaFixture("photo.jpg", mediaTestJPEG())
	m.cache.getErr, m.cache.putErr = errors.New("database down"), errors.New("database down")
	m.index.revisions["photo.jpg"] = filesContentRevision(m.file.ContentNamespace, m.driver.entry)
	body, err := m.get(t, "photo.jpg")
	if err != nil {
		t.Fatalf("a cache outage failed a read the file itself can answer: %v", err)
	}
	if body["state"] != "parsed" {
		t.Fatalf("response: %v", body)
	}
}

func TestFilesMediaMetadataFallsBackToTraversalOnStaleKey(t *testing.T) {
	m := newMediaFixture("photo.jpg", mediaTestJPEG())
	m.index.rows[0].Key = "drive/owner-a/old-name.jpg"
	body, err := m.get(t, "photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if body["state"] != "parsed" || m.driver.traversalStats != 1 || m.driver.traversalOpens != 1 {
		t.Fatalf("stale key: %v, %d stats, %d opens", body, m.driver.traversalStats, m.driver.traversalOpens)
	}
}

func TestFilesMediaMetadataRejectsFolders(t *testing.T) {
	m := newMediaFixture("photo.jpg", mediaTestJPEG())
	m.index.rows[0].IsDir = true
	_, err := m.get(t, "photo.jpg")
	if problem := filesProblem(err); err == nil || problem.Status != 409 || problem.Code != "not_a_file" {
		t.Fatalf("folder: %v", err)
	}
}
