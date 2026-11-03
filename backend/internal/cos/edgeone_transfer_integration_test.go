package cos

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"everythingabout.me/backend/internal/api"
	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
	"github.com/jackc/pgx/v5"
	sdk "github.com/tencentyun/cos-go-sdk-v5"
)

// This is an isolated HTTP forwarding fixture, not an EdgeOne deployment or a
// claim that a real COS bucket has passed its provider admission probes.
type edgeOneCOSProvider struct {
	mu                                        sync.Mutex
	key, uploadID, mediaType, intent, version string
	parts                                     map[int][]byte
	published                                 []byte
	completes, wholePuts, browserPuts         int
	loseNext                                  bool
	queried                                   int
}

func (p *edgeOneCOSProvider) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	q := r.URL.Query()
	key := strings.TrimPrefix(r.URL.Path, "/")
	w.Header().Set("Content-Type", "application/xml")
	if q.Get("q-signature") != "" && (r.Method != "PUT" || !verifyFixturePartSignature(r)) {
		w.WriteHeader(403)
		return
	}
	switch {
	case r.Method == "POST" && q.Has("uploads"):
		p.key = key
		p.uploadID = "native-upload-opaque-id"
		p.mediaType = r.Header.Get("Content-Type")
		p.intent = r.Header.Get("X-Cos-Meta-Eam-Intent")
		p.parts = map[int][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", xmlText(p.uploadID))
	case r.Method == "PUT" && q.Get("uploadId") != "":
		// Browser upload authorization is checked from the actual request received
		// after the fixed-prefix rewrite and native Host replacement.
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("X-CSRF-Token") != "" {
			t.Error("forwarder leaked application authority to COS")
		}
		if !verifyFixturePartSignature(r) || r.Header.Get("X-Cos-Copy-Source") != "" {
			w.WriteHeader(403)
			return
		}
		if q.Get("uploadId") != p.uploadID || key != p.key {
			w.WriteHeader(404)
			return
		}
		data, e := io.ReadAll(r.Body)
		if e != nil {
			t.Error(e)
			w.WriteHeader(400)
			return
		}
		sum := md5.Sum(data)
		if base64.StdEncoding.EncodeToString(sum[:]) != r.Header.Get("Content-MD5") {
			w.WriteHeader(400)
			return
		}
		number, e := strconv.Atoi(q.Get("partNumber"))
		if e != nil || number < 1 {
			w.WriteHeader(400)
			return
		}
		p.parts[number] = bytes.Clone(data)
		p.browserPuts++
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		if p.loseNext {
			p.loseNext = false
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close()
			return
		}
	case r.Method == "GET" && q.Get("uploadId") != "":
		p.queried++
		if key != p.key || q.Get("uploadId") != p.uploadID {
			w.WriteHeader(404)
			io.WriteString(w, `<Error><Code>NoSuchUpload</Code></Error>`)
			return
		}
		marker, _ := strconv.Atoi(q.Get("part-number-marker"))
		number := marker + 1
		io.WriteString(w, "<ListPartsResult>")
		if data, ok := p.parts[number]; ok {
			sum := md5.Sum(data)
			fmt.Fprintf(w, "<Part><PartNumber>%d</PartNumber><Size>%d</Size><ETag>\"%s\"</ETag></Part>", number, len(data), hex.EncodeToString(sum[:]))
		}
		io.WriteString(w, "</ListPartsResult>")
	case r.Method == "POST" && q.Get("uploadId") != "":
		if key != p.key || q.Get("uploadId") != p.uploadID {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("X-Cos-Forbid-Overwrite") != "true" || r.Header.Get("If-None-Match") != "*" {
			t.Error("completion lost create-only native conditions")
		}
		var manifest sdk.CompleteMultipartUploadOptions
		if err := xml.NewDecoder(r.Body).Decode(&manifest); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		for i, part := range manifest.Parts {
			data, ok := p.parts[part.PartNumber]
			sum := md5.Sum(data)
			if !ok || part.PartNumber != i+1 || strings.Trim(part.ETag, `"`) != hex.EncodeToString(sum[:]) {
				w.WriteHeader(400)
				return
			}
			p.published = append(p.published, data...)
		}
		p.completes++
		p.version = "native-version-one"
		w.Header().Set("X-Cos-Version-Id", p.version)
		io.WriteString(w, `<CompleteMultipartUploadResult><ETag>"whole-object-etag"</ETag></CompleteMultipartUploadResult>`)
	case r.Method == "HEAD":
		if key != p.key || p.version == "" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(p.published)))
		w.Header().Set("Content-Type", p.mediaType)
		w.Header().Set("X-Cos-Version-Id", p.version)
		w.Header().Set("ETag", `"whole-object-etag"`)
		w.Header().Set("Last-Modified", "Tue, 03 Nov 2026 00:00:00 GMT")
		w.Header().Set("X-Cos-Meta-Eam-Intent", p.intent)
	case r.Method == "GET":
		// The native root's delimiter listing is intentionally empty before and after
		// this single file; root identity does not depend on a full inventory.
		io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
	case r.Method == "DELETE" && q.Get("uploadId") != "":
		if p.version != "" {
			w.WriteHeader(404)
			io.WriteString(w, `<Error><Code>NoSuchUpload</Code></Error>`)
			return
		}
		w.WriteHeader(204)
	case r.Method == "PUT":
		p.wholePuts++
		t.Error("application sent a whole-file PUT")
		w.WriteHeader(500)
	default:
		t.Errorf("unexpected native request %s %s", r.Method, r.URL)
		w.WriteHeader(405)
	}
}
func xmlText(value string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}

type edgeOneCOSFactory struct {
	tree    *TreeFactory
	native  objectmutation.Backend
	options objectmutation.Options
}

func (f *edgeOneCOSFactory) binding(owner string) storage.Config {
	return storage.Config{OwnerID: owner, ConnectionID: "edgeone-sdk-fixture", Generation: 1, ContentRoot: "drive/" + owner + "/", SystemRoot: "__eam_system__/" + owner + "/"}
}
func (f *edgeOneCOSFactory) Open(ctx context.Context, owner string) (storage.Driver, error) {
	binding := f.binding(owner)
	tree, e := f.tree.Open(ctx, binding)
	if e != nil {
		return nil, e
	}
	driver, e := objectmutation.New(tree, binding, f.native, f.options)
	if e != nil {
		tree.Close()
	}
	return driver, e
}
func edgeOneDisposableDB(t *testing.T) (context.Context, *store.DB) {
	t.Helper()
	address := os.Getenv("EAM_TEST_DATABASE_URL")
	if address == "" {
		t.Skip("EAM_TEST_DATABASE_URL unset; isolated EdgeOne/COS SDK API integration not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	admin, e := pgx.Connect(ctx, address)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	var entropy [8]byte
	if _, e = rand.Read(entropy[:]); e != nil {
		t.Fatal(e)
	}
	name := "eam_edgeone_sdk_" + hex.EncodeToString(entropy[:])
	quoted := pgx.Identifier{name}.Sanitize()
	if _, e = admin.Exec(ctx, "create database "+quoted); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+quoted+" with (force)"); err != nil {
			t.Error(err)
		}
	})
	addressURL, e := url.Parse(address)
	if e != nil {
		t.Fatal(e)
	}
	addressURL.Path = "/" + name
	db, e := store.Open(ctx, addressURL.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(db.Close)
	migrations, e := filepath.Glob("../../../sql/*.sql")
	if e != nil || len(migrations) < 25 {
		t.Fatalf("migration discovery: %d %v", len(migrations), e)
	}
	for pass := 0; pass < 2; pass++ {
		for _, file := range migrations {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Pool().Exec(ctx, string(data)); err != nil {
				t.Fatalf("migration %s pass%d: %v", file, pass, err)
			}
		}
	}
	return ctx, db
}

// edgeOneBrowserUpload exercises the browser-owned transport headers, not a
// separate implementation of signing or the application's upload state machine.
func edgeOneBrowserUpload(t *testing.T, ctx context.Context, origin, cookie string, grant map[string]any, data []byte, status int) {
	t.Helper()
	script, err := filepath.Abs("../../../web/tests/edgeone-browser-acceptance.mjs")
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]any{"origin": origin, "cookie": cookie, "grant": grant, "data": base64.StdEncoding.EncodeToString(data), "size": len(data), "expectedStatus": status, "module": os.Getenv("EAM_TEST_PLAYWRIGHT_MODULE"), "chrome": os.Getenv("EAM_TEST_CHROME")})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "node", script)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("signed browser transport: %v\n%s", err, output)
	}
	t.Log(string(output))
}

func TestEdgeOneForwardedCOSPostgresSignedUploadLifecycle(t *testing.T) {
	ctx, db := edgeOneDisposableDB(t)
	provider := &edgeOneCOSProvider{}
	client, native, origin := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { provider.serve(t, w, r) }))
	controlBackend := native.(*mutationBackend)
	controlBackend.signer = sdk.NewClient(controlBackend.client.BaseURL, &http.Client{Transport: &sdk.AuthorizationTransport{SecretID: "upload-fixture-id", SecretKey: "upload-fixture-secret"}})
	tree, e := client.TreeFactory()
	if e != nil {
		t.Fatal(e)
	}
	factory := &edgeOneCOSFactory{tree: tree, native: native, options: objectmutation.Options{SpoolRoot: t.TempDir(), MaxPublishBytes: 32 << 20, Admission: objectmutation.Admission{ConnectionID: "edgeone-sdk-fixture", Endpoint: origin.URL, Bucket: "fixture-123456", Region: "ap-test", CheckedAt: time.Now(), ProbeReference: "isolated real COS SDK and HTTP forwarder fixture, not live provider evidence", AtomicPublish: true, ConditionalCreate: true, SingleGateway: true, MultipartUploads: true, SignedMultipartParts: true}}}
	cfg := &config.Config{AppSecret: "edgeone-independent-api-secret", FileUploadIngress: "edgeone", SessionTTLDays: 1, Storage: config.Storage{Backend: "cos", CoordinatedWrites: true}}
	authentication := auth.New(db, cfg)
	var owner string
	if e = db.Pool().QueryRow(ctx, `insert into users(email,password_hash) values('edgeone-fixture@example.test','') returning id::text`).Scan(&owner); e != nil {
		t.Fatal(e)
	}
	driver, e := factory.Open(ctx, owner)
	if e != nil {
		t.Fatal(e)
	}
	actions := driver.(fileops.NativeActions)
	binding := actions.StorageBinding()
	root := fileops.Root{OwnerID: owner, ConnectionID: binding.ConnectionID, Generation: binding.Generation, ContentRoot: binding.ContentRoot, SystemRoot: binding.SystemRoot, CaseSensitive: actions.StorageNameRules().CaseSensitive, RetainsVersions: true}
	driver.Close()
	journal := fileops.NewPostgres(db.Pool())
	if e = journal.Initialize(ctx, root, fileops.Baseline{LogicalLimit: 64 << 20, PhysicalLimit: 256 << 20, NativeComplete: true, IndexCompared: true, LegacyWritersDrained: true, Evidence: json.RawMessage(`{"fixture":"fresh isolated COS root"}`)}); e != nil {
		t.Fatal(e)
	}
	cookie, _, e := authentication.CreateSession(ctx, owner, "edgeone fixture", "")
	if e != nil {
		t.Fatal(e)
	}
	server := api.New(cfg, db, nil, nil)
	server.SetStorageFactory(factory)
	workers, cancel := context.WithCancel(ctx)
	defer cancel()
	server.StartFileOperations(workers)
	var appBodyBytes atomic.Int64
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/files/transfer/") || r.Header.Get("Content-Type") == "application/octet-stream" {
			data, _ := io.ReadAll(r.Body)
			appBodyBytes.Add(int64(len(data)))
			t.Error("file-body request entered application server")
			w.WriteHeader(500)
			return
		}
		server.Handler().ServeHTTP(w, r)
	})
	originURL, _ := url.Parse(origin.URL)
	var badHost atomic.Bool
	forward := httputil.NewSingleHostReverseProxy(originURL)
	forward.Director = func(r *http.Request) {
		suffix := strings.TrimPrefix(r.URL.EscapedPath(), "/api/files/transfer/")
		r.URL.Scheme = originURL.Scheme
		r.URL.Host = originURL.Host
		r.URL.RawPath = "/" + suffix
		r.URL.Path, _ = url.PathUnescape(r.URL.RawPath)
		r.Host = originURL.Host
		if badHost.Load() {
			r.Host = "wrong-cos-host.example"
		}
		r.Header.Del("Cookie")
		r.Header.Del("Authorization")
		r.Header.Del("X-CSRF-Token")
	}
	forward.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) { w.WriteHeader(502) }
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/edgeone-browser-probe" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, "<!doctype html><title>Isolated upload transport probe</title>")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/files/transfer/") {
			forward.ServeHTTP(w, r)
			return
		}
		app.ServeHTTP(w, r)
	}))
	defer front.Close()
	cfg.FileCookieOrigins = []string{front.URL}
	httpClient := front.Client()
	request := func(method, path string, body []byte, headers http.Header) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, front.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range headers {
			req.Header[k] = append([]string(nil), v...)
		}
		req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: cookie})
		response, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if len(data) > 0 && json.Unmarshal(data, &out) != nil {
			t.Fatalf("invalid JSON HTTP%d: %s", response.StatusCode, data)
		}
		return response.StatusCode, out
	}
	status, me := request("GET", "/api/files/me", nil, nil)
	if status != 200 {
		t.Fatal(status, me)
	}
	control := http.Header{"Origin": {front.URL}, "X-CSRF-Token": {me["csrfToken"].(string)}, "Content-Type": {"application/json"}}
	target := "/ +%2F?#中文.bin"
	// Raw binary spans two assigned parts, including a short final part.
	payload := bytes.Repeat([]byte{0, 255, 13, 10, 128, 1, 2, 3}, (8<<20)/8+1)
	create, _ := json.Marshal(map[string]any{"path": target, "size": len(payload), "mediaType": "application/octet-stream"})
	control.Set("Idempotency-Key", "edgeone-full-chain")
	status, session := request("POST", "/api/files/uploads", create, control)
	if status != 201 {
		t.Fatal(status, session)
	}
	id := session["id"].(string)
	partSize := int(session["partSize"].(float64))
	prefix := "/api/files/uploads/" + id
	for number := 1; number <= 2; number++ {
		chunk := payload[(number-1)*partSize : min(number*partSize, len(payload))]
		sha := sha256.Sum256(chunk)
		md := md5.Sum(chunk)
		input, _ := json.Marshal(map[string]any{"size": len(chunk), "sha256": hex.EncodeToString(sha[:]), "md5": base64.StdEncoding.EncodeToString(md[:])})
		authorize := prefix + fmt.Sprintf("/parts/%d/authorize", number)
		status, denied := request("POST", authorize, input, http.Header{"Content-Type": {"application/json"}})
		if status != 403 {
			t.Fatal("cookie authorize lacked CSRF", status, denied)
		}
		status, grant := request("POST", authorize, input, control)
		if status != 200 {
			t.Fatal(status, grant)
		}
		if !strings.HasPrefix(grant["url"].(string), "/api/files/transfer/") || strings.Contains(grant["url"].(string), origin.URL) {
			t.Fatal("non-origin grant", grant)
		}
		confirm := prefix + fmt.Sprintf("/parts/%d/confirm", number)
		status, pending := request("POST", confirm, nil, control)
		if status != 409 || pending["code"] != "part_pending" {
			t.Fatal("no provider bytes must not ACK", status, pending)
		}
		send := func(change func(*http.Request)) int {
			t.Helper()
			req, err := http.NewRequestWithContext(ctx, "PUT", front.URL+grant["url"].(string), bytes.NewReader(chunk))
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range grant["headers"].(map[string]any) {
				req.Header.Set(k, v.(string))
			}
			req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: cookie})
			req.Header.Set("Authorization", "Bearer application-only")
			req.Header.Set("X-CSRF-Token", "application-csrf")
			if change != nil {
				change(req)
			}
			res, err := httpClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			io.Copy(io.Discard, res.Body)
			return res.StatusCode
		}
		if number == 1 {
			tests := []struct {
				name   string
				change func(*http.Request)
				want   int
			}{
				{"method", func(r *http.Request) { r.Method = "POST" }, 403},
				{"path", func(r *http.Request) { r.URL.Path += "other"; r.URL.RawPath = "" }, 403},
				{"part", func(r *http.Request) { q := r.URL.Query(); q.Set("partNumber", "2"); r.URL.RawQuery = q.Encode() }, 403},
				{"upload", func(r *http.Request) { q := r.URL.Query(); q.Set("uploadId", "other"); r.URL.RawQuery = q.Encode() }, 403},
				{"digest-header", func(r *http.Request) {
					r.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(make([]byte, 16)))
				}, 403},
				{"copy-source", func(r *http.Request) { r.Header.Set("X-Cos-Copy-Source", "other-bucket/private-object") }, 403},
				{"digest-body", func(r *http.Request) { r.Body = io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), len(chunk)))) }, 400},
				{"expired", func(r *http.Request) {
					// Generate a cryptographically valid request whose authority has expired.
					q := url.Values{"uploadId": {r.URL.Query().Get("uploadId")}, "partNumber": {"1"}}
					nativeURL := *r.URL
					nativeURL.Path = strings.TrimPrefix(nativeURL.Path, "/api/files/transfer")
					nativeURL.RawPath = strings.TrimPrefix(nativeURL.EscapedPath(), "/api/files/transfer")
					nativeURL.RawQuery = q.Encode()
					check := &http.Request{Method: "PUT", URL: &nativeURL, Host: originURL.Host, Header: http.Header{"Content-Md5": {r.Header.Get("Content-MD5")}, "Content-Type": {"application/octet-stream"}, "Content-Length": {strconv.Itoa(len(chunk))}}}
					start, end := time.Now().Add(-2*time.Minute), time.Now().Add(-time.Minute)
					sdk.AddAuthorizationHeader("upload-fixture-id", "upload-fixture-secret", "", check, &sdk.AuthTime{SignStartTime: start, SignEndTime: end, KeyStartTime: start, KeyEndTime: end})
					fields, _ := url.ParseQuery(check.Header.Get("Authorization"))
					for key, values := range fields {
						q[key] = values
					}
					r.URL.RawQuery = q.Encode()
				}, 403},
			}
			for _, test := range tests {
				if got := send(test.change); got != test.want {
					t.Fatalf("%s tamper HTTP%d want%d", test.name, got, test.want)
				}
			}
			badHost.Store(true)
			if got := send(nil); got != 403 {
				t.Fatal("wrong native Host accepted", got)
			}
			badHost.Store(false)
		}
		provider.mu.Lock()
		provider.loseNext = number == 1
		provider.mu.Unlock()
		want := 200
		if number == 1 {
			want = 502
		}
		if number == 1 && os.Getenv("EAM_TEST_PLAYWRIGHT_MODULE") != "" {
			edgeOneBrowserUpload(t, ctx, front.URL, cookie, grant, chunk, want)
		} else if got := send(nil); got != want {
			t.Fatalf("part%d PUT HTTP%d want%d", number, got, want)
		}
		// A PUT success or lost response does not become an application receipt.
		status, listed := request("GET", prefix+"/parts", nil, nil)
		if status != 200 || len(listed["items"].([]any)) != number-1 {
			t.Fatal("provider response prematurely acknowledged", status, listed)
		}
		provider.mu.Lock()
		stored := bytes.Equal(provider.parts[number], chunk)
		published := len(provider.published)
		provider.mu.Unlock()
		if !stored || published != 0 {
			t.Fatal("part was not unpublished at provider before public receipt")
		}
		status, receipt := request("POST", confirm, nil, control)
		if status != 201 || receipt["checksumSource"] != "client-sha256-cos-md5" || receipt["sha256"] != hex.EncodeToString(sha[:]) {
			t.Fatal("native verified receipt provenance", status, receipt)
		}
	}
	status, op := request("POST", prefix+"/commit", nil, control)
	if status != 202 {
		t.Fatal(status, op)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, op = request("GET", "/api/files/operations/"+id, nil, nil)
		if status != 200 {
			t.Fatal(status, op)
		}
		if op["state"] == "succeeded" {
			break
		}
		if op["state"] == "failed" || op["state"] == "outcome_unknown" || time.Now().After(deadline) {
			t.Fatal("commit did not settle", op)
		}
		time.Sleep(5 * time.Millisecond)
	}
	status, result := request("POST", prefix+"/commit", nil, control)
	if status != 200 || result["path"] != target {
		t.Fatal("saved result path mismatch", status, result)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if !bytes.Equal(provider.published, payload) || provider.completes != 1 || provider.wholePuts != 0 || provider.browserPuts != 2 || provider.queried < 2 {
		t.Fatalf("wrong native flow complete%d fullPUT%d partPUT%d query%d", provider.completes, provider.wholePuts, provider.browserPuts, provider.queried)
	}
	if appBodyBytes.Load() != 0 {
		t.Fatal("file body entered app", appBodyBytes.Load())
	}
}
