package cos

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
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
	sdk.AddAuthorizationHeader("upload-fixture-id", "upload-fixture-secret", "", check, &sdk.AuthTime{SignStartTime: a, SignEndTime: b, KeyStartTime: c, KeyEndTime: d})
	want, _ := url.ParseQuery(check.Header.Get("Authorization"))
	return q.Get("q-ak") == "upload-fixture-id" && q.Get("q-signature") == want.Get("q-signature")
}
func TestMutationSignedPartRestrictsMethodPathHostQueryAndDigest(t *testing.T) {
	payload := []byte("signed bytes")
	digest := md5.Sum(payload)
	md5value := base64.StdEncoding.EncodeToString(digest[:])
	key := "drive/owner/ +%2F?#中文.bin"
	_, backend, origin := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !verifyFixturePartSignature(r) {
			w.WriteHeader(403)
			return
		}
		// Model the required UploadPart-only IAM policy: unsigned copy-source
		// headers cannot turn this identity into an UploadPartCopy capability.
		if r.Header.Get("X-Cos-Copy-Source") != "" {
			w.WriteHeader(403)
			return
		}
		body, _ := io.ReadAll(r.Body)
		sum := md5.Sum(body)
		if base64.StdEncoding.EncodeToString(sum[:]) != r.Header.Get("Content-MD5") {
			w.WriteHeader(400)
			return
		}
		if r.URL.Path != "/"+key {
			t.Error("decoded native name changed", r.URL.Path)
		}
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	}))
	// Dedicated signer models a CAM identity with only UploadPart permission;
	// the native control client remains fixture-id and can never issue browser grants.
	control := backend.(*mutationBackend)
	control.signer = sdk.NewClient(control.client.BaseURL, &http.Client{Transport: &sdk.AuthorizationTransport{SecretID: "upload-fixture-id", SecretKey: "upload-fixture-secret"}})
	signed := backend.(objectmutation.SignedMultipartBackend)
	grant, err := signed.SignPart(context.Background(), key, "native-id", 3, int64(len(payload)), md5value, time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(grant.URL)
	for _, name := range []string{"content-length", "content-md5", "content-type", "host"} {
		if !strings.Contains(u.Query().Get("q-header-list"), name) {
			t.Fatal("unbound header", name, u)
		}
	}
	if !strings.Contains(u.Query().Get("q-url-param-list"), "partnumber") || !strings.Contains(u.Query().Get("q-url-param-list"), "uploadid") {
		t.Fatal("unbound slot", u)
	}
	send := func(change func(*http.Request)) int {
		request, _ := http.NewRequest("PUT", grant.URL, bytes.NewReader(payload))
		for k, v := range grant.Headers {
			request.Header.Set(k, v)
		}
		// Go's transport derives Content-Length from the Blob-equivalent body.
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
		func(r *http.Request) {
			r.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(make([]byte, 16)))
		},
	}
	for i, change := range mutations {
		if status := send(change); status != 403 {
			t.Fatal(fmt.Sprintf("tamper%d accepted: %d", i, status))
		}
	}
}
func TestMutationSignedPartQueryUsesPrivateNativeEvidence(t *testing.T) {
	_, backend, _ := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Query().Get("uploadId") != "native" || r.URL.Query().Get("part-number-marker") != "2" || r.URL.Query().Get("max-parts") != "1" {
			t.Error("wrong native query", r.URL)
		}
		io.WriteString(w, `<ListPartsResult><Part><PartNumber>3</PartNumber><Size>12</Size><ETag>"abc"</ETag></Part></ListPartsResult>`)
	}))
	part, found, err := backend.(objectmutation.SignedMultipartBackend).QueryPart(context.Background(), "key", "native", 3)
	if err != nil || !found || part.Size != 12 || part.ETag != `"abc"` {
		t.Fatal(part, found, err)
	}
}

func TestMutationSignedPartRequiresDedicatedSigner(t *testing.T) {
	_, backend, _ := mutationFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("signing must perform no request") }))
	signed := backend.(objectmutation.SignedMultipartBackend)
	if signed.SignedPartsSupported() {
		t.Fatal("broad control credential enabled browser signing")
	}
	_, err := signed.SignPart(context.Background(), "key", "id", 1, 4, "jXd/OF09/siBXSD3SWAm3A==", time.Now().Add(time.Minute))
	if err == nil {
		t.Fatal("missing dedicated signer issued grant")
	}
}
