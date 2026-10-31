package api

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/s3storage"
	"everythingabout.me/backend/internal/storage"
)

type filesS3Factory struct{ factory *s3storage.Factory }

func (f filesS3Factory) Binding(owner string) (storage.Config, error) {
	return storage.Config{OwnerID: owner, ConnectionID: "fixture-s3", Generation: 1, ContentRoot: "drive/" + owner + "/", SystemRoot: "system/" + owner + "/"}, nil
}
func (f filesS3Factory) Open(ctx context.Context, owner string) (storage.Driver, error) {
	cfg, _ := f.Binding(owner)
	return f.factory.Open(ctx, cfg)
}

type filesS3Object struct{ body, version, mediaType string }
type filesS3Provider struct {
	objects  map[string]filesS3Object
	mu       sync.Mutex
	requests []string
}

func (p *filesS3Provider) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") || r.Header.Get("X-Amz-Security-Token") != "fixture-session" {
		t.Error("native request lacked official SDK authentication")
	}
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	p.mu.Lock()
	p.requests = append(p.requests, r.Method+" "+key+"?"+r.URL.RawQuery)
	p.mu.Unlock()
	if r.URL.Query().Get("list-type") == "2" {
		prefix := r.URL.Query().Get("prefix")
		type object struct {
			Key          string
			Size         int
			LastModified string
		}
		type common struct{ Prefix string }
		result := struct {
			XMLName        xml.Name `xml:"ListBucketResult"`
			IsTruncated    bool
			Contents       []object
			CommonPrefixes []common
		}{IsTruncated: false}
		prefixes := map[string]bool{}
		keys := make([]string, 0, len(p.objects))
		for k := range p.objects {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !strings.HasPrefix(k, prefix) {
				continue
			}
			rest := strings.TrimPrefix(k, prefix)
			if i := strings.Index(rest, "/"); i >= 0 {
				prefixes[prefix+rest[:i+1]] = true
				continue
			}
			result.Contents = append(result.Contents, object{k, len(p.objects[k].body), "2026-10-31T00:00:00Z"})
		}
		for prefix := range prefixes {
			result.CommonPrefixes = append(result.CommonPrefixes, common{prefix})
		}
		sort.Slice(result.CommonPrefixes, func(i, j int) bool { return result.CommonPrefixes[i].Prefix < result.CommonPrefixes[j].Prefix })
		w.Header().Set("Content-Type", "application/xml")
		if err := xml.NewEncoder(w).Encode(result); err != nil {
			t.Error(err)
		}
		return
	}
	object, ok := p.objects[key]
	if !ok {
		w.WriteHeader(404)
		return
	}
	if r.Method == http.MethodHead && strings.HasSuffix(key, "/changed.txt") {
		object = filesS3Object{"old", "", "text/old"}
	}
	version := r.URL.Query().Get("versionId")
	// Current bytes changed after HEAD; only the explicitly retained version
	// still has the selected representation. A missing native pin must fail tests.
	if r.Method == http.MethodGet && object.version == "retained-v1" && version == "" {
		object = filesS3Object{"new-current", "current-v2", "application/octet-stream"}
	}
	if version != "" && version != object.version {
		w.WriteHeader(404)
		return
	}
	body := object.body
	w.Header().Set("Content-Type", object.mediaType)
	w.Header().Set("Last-Modified", "Sat, 31 Oct 2026 00:00:00 GMT")
	if object.version != "" {
		w.Header().Set("X-Amz-Version-Id", object.version)
	}
	status := 200
	if value := r.Header.Get("Range"); value != "" {
		var start, end int
		if _, err := fmt.Sscanf(value, "bytes=%d-%d", &start, &end); err != nil || start < 0 || end >= len(body) {
			t.Errorf("unexpected native range %q", value)
			w.WriteHeader(416)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		body = body[start : end+1]
		status = 206
		if strings.HasSuffix(key, "/bad-range.txt") {
			status = 200
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}

func TestFilesS3UnifiedHTTPReadConformance(t *testing.T) {
	const special = " +%2F?#.txt "
	provider := &filesS3Provider{objects: map[string]filesS3Object{
		"drive/owner-a/" + special:          {"0123456789", "retained-v1", "text/plain"},
		"drive/owner-a/Empty/":              {},
		"drive/owner-a/Implicit/inside.txt": {"inside", "inside-v1", "text/plain"},
		"drive/owner-a/plain.txt":           {"unversioned", "", "text/plain"},
		"drive/owner-a/changed.txt":         {"NEWER!", "new-get-version", "application/octet-stream"},
		"drive/owner-a/bad-range.txt":       {"broken", "range-v1", "text/plain"},
		"drive/owner-b/private.txt":         {"other-owner", "private-v1", "text/plain"},
		"system/owner-a/private.txt":        {"system-secret", "system-v1", "text/plain"},
	}}
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { provider.serve(t, w, r) }))
	defer native.Close()
	factory, err := s3storage.NewFactory(s3storage.Options{Endpoint: native.URL, Region: "test", Bucket: "bucket", PathStyle: true, AccessKeyID: "fixture-id", SecretAccessKey: "fixture-secret", SessionToken: "fixture-session"})
	if err != nil {
		t.Fatal(err)
	}
	makeHandler := func(owner string) http.Handler {
		return (&Server{cfg: &config.Config{AppSecret: "stable-test-secret", Storage: config.Storage{Backend: "s3"}}, filesAuth: &boundaryAuth{principal: &auth.Principal{User: &auth.User{ID: owner}}}, trees: filesS3Factory{factory: factory}}).Handler()
	}
	handler := makeHandler("owner-a")
	request := func(method, resource, path string, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/api/files"+resource+"?"+url.Values{"path": {path}}.Encode(), nil)
		r.Header.Set("Authorization", "Bearer valid")
		for key, value := range headers {
			r.Header.Set(key, value)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	metadata := func(path string) fc.Metadata {
		t.Helper()
		w := request("GET", "", path, nil)
		if w.Code != 200 {
			t.Fatalf("metadata %s: %d %s", path, w.Code, w.Body.String())
		}
		var got fc.Metadata
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	specialMetadata := metadata("/" + special)
	if specialMetadata.Path != fc.Path("/"+special) || specialMetadata.Name != special || specialMetadata.Size == nil || *specialMetadata.Size != 10 || specialMetadata.ContentRevision == nil {
		t.Fatalf("exact native metadata lost: %+v", specialMetadata)
	}
	tag := strconv.Quote(*specialMetadata.ContentRevision)
	t.Run("implicit_and_empty_folders", func(t *testing.T) {
		w := request("GET", "/list", "/", nil)
		var list fc.Collection[fc.Metadata]
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || len(list.Items) != 6 {
			t.Fatalf("list: %d %s", w.Code, w.Body.String())
		}
		if list.Items[0].Path != "/Empty" || list.Items[1].Path != "/Implicit" {
			t.Fatalf("folders missing: %+v", list.Items)
		}
		for _, folder := range []string{"/Empty", "/Implicit"} {
			if got := metadata(folder); got.Kind != "folder" {
				t.Fatal(got)
			}
		}
		w = request("GET", "/list", "/Empty", nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"items":[]`) {
			t.Fatalf("empty: %d %s", w.Code, w.Body.String())
		}
	})
	t.Run("version_ranges_conditions_and_head", func(t *testing.T) {
		cases := []struct {
			name, method               string
			headers                    map[string]string
			status                     int
			body, contentRange, length string
		}{
			{"full", "GET", nil, 200, "0123456789", "", "10"},
			{"matching range", "GET", map[string]string{"Range": "bytes=2-5", "If-Match": tag}, 206, "2345", "bytes 2-5/10", "4"},
			{"suffix", "GET", map[string]string{"Range": "bytes=-3"}, 206, "789", "bytes 7-9/10", "3"},
			{"not modified", "GET", map[string]string{"If-None-Match": tag, "Range": "bytes=99-"}, 304, "", "", ""},
			{"precondition", "GET", map[string]string{"If-Match": `"wrong"`}, 412, "problem", "", ""},
			{"if range mismatch", "GET", map[string]string{"Range": "bytes=2-5", "If-Range": `"wrong"`}, 200, "0123456789", "", "10"},
			{"head ignores range", "HEAD", map[string]string{"Range": "bytes=2-5"}, 200, "", "", "10"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				w := request(tc.method, "/content", "/"+special, tc.headers)
				if w.Code != tc.status {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				if tc.body == "problem" {
					if w.Header().Get("Content-Type") != "application/problem+json" {
						t.Fatal(w.Header())
					}
					return
				}
				if w.Body.String() != tc.body || w.Header().Get("Content-Range") != tc.contentRange || w.Header().Get("ETag") != tag {
					t.Fatalf("headers=%v body=%q", w.Header(), w.Body.String())
				}
				if tc.length != "" && w.Header().Get("Content-Length") != tc.length {
					t.Fatal(w.Header())
				}
			})
		}
	})
	t.Run("unversioned_does_not_invent_validator", func(t *testing.T) {
		if metadata("/plain.txt").ContentRevision != nil {
			t.Fatal("unversioned metadata invented revision")
		}
		w := request("GET", "/content", "/plain.txt", nil)
		if w.Code != 200 || w.Header().Get("ETag") != "" || w.Body.String() != "unversioned" {
			t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body.String())
		}
		w = request("GET", "/content", "/plain.txt", map[string]string{"If-Match": tag})
		if w.Code != 412 {
			t.Fatalf("unproven condition accepted: %d", w.Code)
		}
	})
	t.Run("bad_native_range_rejected_before_bytes", func(t *testing.T) {
		w := request("GET", "/content", "/bad-range.txt", map[string]string{"Range": "bytes=1-2"})
		if w.Code != 502 || w.Header().Get("Content-Type") != "application/problem+json" || !json.Valid(w.Body.Bytes()) || strings.HasPrefix(w.Body.String(), "ro") {
			t.Fatalf("bad native range escaped: %d %s", w.Code, w.Body.String())
		}
	})
	t.Run("actual_get_metadata_selects_version_and_headers", func(t *testing.T) {
		if metadata("/changed.txt").ContentRevision != nil {
			t.Fatal("HEAD fixture unexpectedly versioned")
		}
		w := request("GET", "/content", "/changed.txt", nil)
		if w.Code != 200 || w.Body.String() != "NEWER!" || w.Header().Get("Content-Length") != "6" || w.Header().Get("Content-Type") != "application/octet-stream" || w.Header().Get("ETag") == "" {
			t.Fatalf("stale HEAD headers: %d %v %s", w.Code, w.Header(), w.Body.String())
		}
		w = request("GET", "/content", "/changed.txt", map[string]string{"If-None-Match": w.Header().Get("ETag")})
		if w.Code != 304 {
			t.Fatalf("actual GET revision not reusable: %d", w.Code)
		}
	})
	t.Run("owner_and_system_roots", func(t *testing.T) {
		if w := request("GET", "", "/private.txt", nil); w.Code != 404 {
			t.Fatalf("other owner content visible: %d", w.Code)
		}
		r := httptest.NewRequest("GET", "/api/files/content?path=%2Fprivate.txt", nil)
		r.Header.Set("Authorization", "Bearer valid")
		w := httptest.NewRecorder()
		makeHandler("owner-b").ServeHTTP(w, r)
		if w.Code != 200 || w.Body.String() != "other-owner" {
			t.Fatalf("owner-b binding failed %d %s", w.Code, w.Body.String())
		}
	})
	provider.mu.Lock()
	defer provider.mu.Unlock()
	selected, changedSelected := false, false
	for _, r := range provider.requests {
		if strings.Contains(r, "system/") {
			t.Fatalf("system root accessed: %s", r)
		}
		if strings.HasPrefix(r, "GET drive/owner-a/"+special+"?") && strings.Contains(r, "versionId=retained-v1") {
			selected = true
		}
		if strings.Contains(r, "changed.txt?versionId=new-get-version") {
			changedSelected = true
		}
	}
	if !selected || !changedSelected {
		t.Fatalf("native version selection missing: retained=%t changed=%t", selected, changedSelected)
	}
}
