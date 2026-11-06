package cos

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
	sdk "github.com/tencentyun/cos-go-sdk-v5"
)

// verifyFixturePartSignature models COS verification from the actual native Host,
// method/path/query and supplied headers, not the browser's public URL.
func verifyFixturePartSignature(r *http.Request) bool {
	q := r.URL.Query()
	query := url.Values{}
	for _, name := range strings.Split(q.Get("q-url-param-list"), ";") {
		for k, v := range q {
			if strings.EqualFold(name, k) {
				query[k] = v
			}
		}
	}
	native := *r.URL
	native.RawQuery = query.Encode()
	check := &http.Request{Method: r.Method, URL: &native, Host: r.Host, Header: make(http.Header)}
	for _, name := range strings.Split(q.Get("q-header-list"), ";") {
		if name != "host" {
			check.Header.Set(name, r.Header.Get(name))
		}
	}
	parse := func(value string) (time.Time, time.Time) {
		v := strings.Split(value, ";")
		if len(v) != 2 {
			return time.Time{}, time.Time{}
		}
		a, _ := strconv.ParseInt(v[0], 10, 64)
		b, _ := strconv.ParseInt(v[1], 10, 64)
		return time.Unix(a, 0), time.Unix(b, 0)
	}
	a, b := parse(q.Get("q-sign-time"))
	c, d := parse(q.Get("q-key-time"))
	if time.Now().Before(a) || time.Now().After(b) {
		return false
	}
	sdk.AddAuthorizationHeader("fixture-id", "fixture-secret", "", check, &sdk.AuthTime{SignStartTime: a, SignEndTime: b, KeyStartTime: c, KeyEndTime: d})
	want, _ := url.ParseQuery(check.Header.Get("Authorization"))
	return q.Get("q-ak") == "fixture-id" && q.Get("q-signature") == want.Get("q-signature")
}

func TestMutationClassASignsLengthWithoutDigest(t *testing.T) {
	payload := []byte("signed bytes")
	key := "drive/owner/ +%2F?#中文.bin"
	_, backend, origin := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !verifyFixturePartSignature(r) {
			w.WriteHeader(403)
			return
		}
		if r.Header.Get("X-Cos-Copy-Source") != "" {
			w.WriteHeader(403)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if int64(len(body)) != int64(r.ContentLength) || r.URL.Path != "/"+key {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("ETag", `"provider-etag"`)
		w.WriteHeader(200)
	}))
	signed := backend.(objectmutation.SignedPartBackend)
	grant, err := signed.SignPart(context.Background(), key, "native-id", 3, int64(len(payload)), time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if grant.Number != 3 || grant.Size != int64(len(payload)) || grant.Headers["Content-MD5"] != "" {
		t.Fatal("grant bound an unintended field", grant)
	}
	u, _ := url.Parse(grant.URL)
	for _, name := range []string{"content-length", "content-type", "host"} {
		if !strings.Contains(u.Query().Get("q-header-list"), name) {
			t.Fatal("unbound header", name, u)
		}
	}
	if strings.Contains(u.Query().Get("q-header-list"), "content-md5") {
		t.Fatal("content digest was signed")
	}
	if !strings.Contains(u.Query().Get("q-url-param-list"), "partnumber") || !strings.Contains(u.Query().Get("q-url-param-list"), "uploadid") {
		t.Fatal("unbound slot", u)
	}
	send := func(change func(*http.Request)) int {
		request, _ := http.NewRequest("PUT", grant.URL, bytes.NewReader(payload))
		for k, v := range grant.Headers {
			request.Header.Set(k, v)
		}
		request.Header.Set("Content-Length", strconv.Itoa(len(payload)))
		if change != nil {
			change(request)
		}
		response, e := origin.Client().Do(request)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}
	if status := send(nil); status != 200 {
		t.Fatal("valid signed part rejected", status, grant.URL)
	}
	mutations := []func(*http.Request){
		func(r *http.Request) { r.Header.Set("X-Cos-Copy-Source", "/other-bucket/private") },
		func(r *http.Request) { r.Method = "POST" }, func(r *http.Request) { r.URL.Path += "other"; r.URL.RawPath = "" }, func(r *http.Request) { r.Host = "different.example" },
		func(r *http.Request) { q := r.URL.Query(); q.Set("partNumber", "4"); r.URL.RawQuery = q.Encode() },
		func(r *http.Request) { q := r.URL.Query(); q.Set("uploadId", "other"); r.URL.RawQuery = q.Encode() },
		func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
	}
	for i, change := range mutations {
		if status := send(change); status != 403 {
			t.Fatal(fmt.Sprintf("tamper%d accepted: %d", i, status))
		}
	}
}

func TestMutationClassAListsProviderParts(t *testing.T) {
	_, backend, _ := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Query().Get("uploadId") != "native" || r.URL.Query().Get("part-number-marker") != "2" || r.URL.Query().Get("max-parts") != "1" {
			t.Error("wrong native query", r.URL)
		}
		io.WriteString(w, `<ListPartsResult><Part><PartNumber>3</PartNumber><Size>12</Size><ETag>"abc"</ETag></Part></ListPartsResult>`)
	}))
	parts, more, err := backend.(objectmutation.SignedPartBackend).ListParts(context.Background(), "key", "native", 2, 1)
	if err != nil || more || len(parts) != 1 || parts[0].Size != 12 || parts[0].ETag != `"abc"` {
		t.Fatal(parts, more, err)
	}
}

func TestMutationClassAUsesControlCredentials(t *testing.T) {
	_, backend, _ := mutationFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("signing must perform no request") }))
	signed := backend.(objectmutation.SignedPartBackend)
	if !signed.SignedPartsSupported() {
		t.Fatal("control credential did not enable part grants")
	}
	if _, err := signed.SignPart(context.Background(), "key", "id", 1, 4, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("part grant failed: %v", err)
	}
	reader := backend.(objectmutation.ReadSignerBackend)
	if !reader.ReadSigningSupported() {
		t.Fatal("control credential did not enable read grants")
	}
	if _, err := reader.PresignRead(context.Background(), "key", storage.ReadGrantOptions{MediaType: "text/plain"}); err != nil {
		t.Fatalf("read grant failed: %v", err)
	}
}

func TestMutationClassAReadGrantUsesControlCredentials(t *testing.T) {
	_, backend, _ := mutationFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("presigning must perform no request") }))
	reader := backend.(objectmutation.ReadSignerBackend)
	if !reader.ReadSigningSupported() {
		t.Fatal("control credential did not enable read grants")
	}
	grant, err := reader.PresignRead(context.Background(), "drive/owner/a b.txt", storage.ReadGrantOptions{MediaType: "text/plain", Disposition: "inline", ExpiresAt: time.Now().Add(5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(grant.URL)
	if err != nil || grant.Kind != "url" {
		t.Fatal(grant, err)
	}
	if parsed.Query().Get("q-ak") != "fixture-id" {
		t.Fatal("read grant used a different credential", parsed.Query().Get("q-ak"))
	}
	if parsed.Query().Get("response-content-type") != "text/plain" || !strings.Contains(parsed.Query().Get("response-content-disposition"), "filename*=") {
		t.Fatal("read representation parameters were not signed", parsed.Query())
	}
}
