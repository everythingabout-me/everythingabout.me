package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"everythingabout.me/backend/internal/config"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storage"
)

// grantingReferenceDriver is the reference-read fixture plus a class A grant, so a test
// can prove a grant is refused before any provider URL exists.
type grantingReferenceDriver struct {
	*referenceContentDriver
	granted []storage.Path
	absent  bool
}

func (d *grantingReferenceDriver) StatReference(ctx context.Context, p storage.Path, ref []byte) (storage.Entry, error) {
	if d.absent {
		return storage.Entry{}, storage.NewError("not_found", p, nil)
	}
	return d.referenceContentDriver.StatReference(ctx, p, ref)
}

func (d *grantingReferenceDriver) GrantRead(_ context.Context, p storage.Path, _ storage.ReadGrantOptions) (storage.ReadGrant, error) {
	d.granted = append(d.granted, p)
	return storage.ReadGrant{Kind: "url", URL: "https://storage.test/" + string(p), ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func downloadGrantFixture(t *testing.T, key string) (*grantingReferenceDriver, *Server, *fileRequest) {
	t.Helper()
	base, s, f := referenceContentFixture(key)
	d := &grantingReferenceDriver{referenceContentDriver: base}
	f.Driver = d
	s.filesIndex = &filesReadTestIndex{rows: []nodes.Row{referenceContentRow("file", key, 10)}}
	s.cfg = &config.Config{AppSecret: "stable-test-secret", Storage: config.Storage{TransferClass: storage.TransferPresignedParts}}
	return d, s, f
}

func runDownloadGrant(t *testing.T, s *Server, f *fileRequest) (*filesDownloadGrantResource, error) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/files/downloads", nil)
	item, _, _, err := s.filesDownloadGrant(r, f, fc.EntrySelector{Path: "/file"}, "attachment", storage.TransferPresignedParts, time.Now().Add(time.Minute))
	return item, err
}

func TestDownloadGrantObservesTheNativeObjectBeforeSigning(t *testing.T) {
	key := "drive/owner-a/file"
	d, s, f := downloadGrantFixture(t, key)
	item, err := runDownloadGrant(t, s, f)
	if err != nil {
		t.Fatal(err)
	}
	if d.statRefs != 1 {
		t.Fatalf("grant made %d native observations, want 1", d.statRefs)
	}
	if len(d.granted) != 1 || d.granted[0] != "file" {
		t.Fatalf("granted paths: %v", d.granted)
	}
	if item.Kind != "url" || item.URL != "https://storage.test/file" {
		t.Fatalf("grant item: %+v", item)
	}
}

func TestDownloadGrantRefusesAnEntryStorageNoLongerHas(t *testing.T) {
	d, s, f := downloadGrantFixture(t, "drive/owner-a/file")
	d.absent = true
	item, err := runDownloadGrant(t, s, f)
	problem := filesProblem(err)
	if err == nil || problem.Status != http.StatusNotFound || problem.Code != "native_absent" {
		t.Fatalf("grant outcome: item=%v problem=%+v", item, problem)
	}
	if len(d.granted) != 0 {
		t.Fatalf("issued a read grant for bytes storage does not have: %v", d.granted)
	}
	if problem.Detail == "" || problem.Detail == "The path was not found." {
		t.Fatalf("the drift must explain itself: %q", problem.Detail)
	}
}

func TestDownloadGrantKeepsWorkingWhenTheKeyCannotBeProven(t *testing.T) {
	// A key from another root generation is not evidence that the file is gone, so the
	// grant path must not turn it into a refusal.
	key := "drive/owner-a/file"
	d, s, f := downloadGrantFixture(t, key)
	d.key = "drive/owner-a/other"
	item, err := runDownloadGrant(t, s, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.granted) != 1 || item.Kind != "url" {
		t.Fatalf("stale key refused a grant: %v %+v", d.granted, item)
	}
}
