package storageprobe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

const reservedProbeRoot = "__eam_storage_probe__/"

type cosProbeConfig struct {
	bucket, region, secretID, secretKey, token, root string
}

func dedicatedCOSConfig(getenv func(string) string) (cosProbeConfig, bool, error) {
	if getenv("EAM_PROBE_COS_ENABLE") != "1" {
		return cosProbeConfig{}, false, nil
	}
	var c cosProbeConfig
	for _, field := range []struct {
		name string
		dest *string
	}{{"BUCKET", &c.bucket}, {"REGION", &c.region}, {"SECRET_ID", &c.secretID}, {"SECRET_KEY", &c.secretKey}, {"ROOT", &c.root}} {
		*field.dest = getenv("EAM_PROBE_COS_" + field.name)
		if *field.dest == "" {
			return c, true, fmt.Errorf("EAM_PROBE_COS_%s is required when probes are enabled", field.name)
		}
	}
	c.token = getenv("EAM_PROBE_COS_SESSION_TOKEN")
	if c.root != reservedProbeRoot {
		return c, true, fmt.Errorf("EAM_PROBE_COS_ROOT must explicitly equal %q, outside drive/", reservedProbeRoot)
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*-[0-9]+$`).MatchString(c.bucket) || !regexp.MustCompile(`^[a-z]+-[a-z0-9-]+$`).MatchString(c.region) {
		return c, true, fmt.Errorf("invalid dedicated COS bucket or region")
	}
	return c, true, nil
}

func TestCOSProbeConfigurationIsolation(t *testing.T) {
	// Regular production variables are intentionally present and ignored.
	env := map[string]string{"COS_BUCKET": "production", "COS_SECRET_ID": "production", "COS_SECRET_KEY": "production"}
	get := func(key string) string { return env[key] }
	if _, enabled, err := dedicatedCOSConfig(get); enabled || err != nil {
		t.Fatalf("ordinary configuration activated probes: enabled=%t err=%v", enabled, err)
	}
	env["EAM_PROBE_COS_ENABLE"] = "1"
	if _, _, err := dedicatedCOSConfig(get); err == nil {
		t.Fatal("incomplete dedicated configuration must fail closed")
	}
	for key, value := range map[string]string{"BUCKET": "disposable-123456", "REGION": "ap-test", "SECRET_ID": "test", "SECRET_KEY": "test", "ROOT": reservedProbeRoot} {
		env["EAM_PROBE_COS_"+key] = value
	}
	if _, enabled, err := dedicatedCOSConfig(get); !enabled || err != nil {
		t.Fatalf("dedicated configuration rejected: %v", err)
	}
	for _, root := range []string{"drive/", "drive/owner/", "", "/", "__eam_storage_probe__/../drive/", "another-root/"} {
		env["EAM_PROBE_COS_ROOT"] = root
		if _, _, err := dedicatedCOSConfig(get); err == nil {
			t.Fatalf("unsafe/nonreserved root accepted: %q", root)
		}
	}
}

type liveCOSProbe struct {
	client    *cos.Client
	ctx       context.Context
	prefix    string
	host      string
	keys      map[string]bool
	uploads   map[string]string // returned upload ID -> exact generated key
	versioned bool
}

func newLiveCOSProbe(t *testing.T, c cosProbeConfig) *liveCOSProbe {
	t.Helper()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	bucketURL, err := url.Parse(fmt.Sprintf("https://%s.cos.%s.myqcloud.com", c.bucket, c.region))
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout: 15 * time.Second, MaxIdleConns: 2,
	}
	client := cos.NewClient(&cos.BaseURL{BucketURL: bucketURL}, &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &cos.AuthorizationTransport{SecretID: c.secretID, SecretKey: c.secretKey, SessionToken: c.token, Transport: transport},
	})
	client.Conf.RetryOpt.Count = 1 // Includes initiation: never invisibly allocate another upload.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	p := &liveCOSProbe{client: client, ctx: ctx, prefix: c.root + hex.EncodeToString(random[:]) + "/", host: bucketURL.Host, keys: make(map[string]bool), uploads: make(map[string]string)}
	t.Cleanup(func() { cancel(); transport.CloseIdleConnections() })
	// Read bucket settings only. Never enable/disable versioning or lifecycle rules.
	versioning, _, err := client.Bucket.GetVersioning(ctx)
	if err != nil {
		t.Fatal("read dedicated bucket versioning before writes: ", err)
	}
	p.versioned = versioning.Status != ""
	if p.versioned {
		// Verify version-list permission before creating billable versions.
		if _, _, err := client.Bucket.GetObjectVersions(ctx, &cos.BucketGetObjectVersionsOptions{Prefix: p.prefix, MaxKeys: 1}); err != nil {
			t.Fatal("dedicated versioned bucket requires version-list permission: ", err)
		}
	}
	t.Cleanup(func() { p.cleanup(t) })
	t.Logf("live isolated COS prefix=%s; SDK attempts=1 client timeout=15s total probe context=3m; versioned=%t", p.prefix, p.versioned)
	return p
}

func (p *liveCOSProbe) key(t *testing.T, suffix string) string {
	t.Helper()
	if suffix == "" || strings.HasPrefix(suffix, "/") || strings.Contains(suffix, "..") {
		t.Fatal("invalid fixture key suffix")
	}
	key := p.prefix + suffix
	p.keys[key] = true // Record before native effects, including ambiguous errors.
	return key
}

func (p *liveCOSProbe) cleanup(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for id, key := range p.uploads {
		if _, err := p.client.Object.AbortMultipartUpload(ctx, key, id); err != nil && !cos.IsNotFoundError(err) {
			t.Errorf("cleanup exact upload %s key %q: %v", id, key, err)
		}
	}
	for key := range p.keys {
		if !strings.HasPrefix(key, p.prefix) || key == p.prefix {
			t.Errorf("refuse cleanup outside generated exact keys: %q", key)
			continue
		}
		if !p.versioned {
			if _, err := p.client.Object.Delete(ctx, key); err != nil {
				t.Errorf("cleanup exact key %q: %v", key, err)
				continue
			}
		} else {
			// Filter exact equality: a listing prefix is never deletion authority.
			// Capture all version IDs before deleting so pagination is stable.
			var ids []string
			opts := &cos.BucketGetObjectVersionsOptions{Prefix: key, MaxKeys: 100}
			for pages := 0; ; pages++ {
				if pages >= 20 {
					t.Errorf("cleanup bounded version listing exceeded for %q", key)
					break
				}
				result, _, err := p.client.Bucket.GetObjectVersions(ctx, opts)
				if err != nil {
					t.Errorf("cleanup list exact-key versions %q: %v", key, err)
					break
				}
				for _, v := range result.Version {
					if v.Key == key {
						ids = append(ids, v.VersionId)
					}
				}
				for _, v := range result.DeleteMarker {
					if v.Key == key {
						ids = append(ids, v.VersionId)
					}
				}
				if !result.IsTruncated {
					break
				}
				if opts.KeyMarker == result.NextKeyMarker && opts.VersionIdMarker == result.NextVersionIdMarker {
					t.Errorf("cleanup version cursor did not advance for %q", key)
					break
				}
				opts.KeyMarker, opts.VersionIdMarker = result.NextKeyMarker, result.NextVersionIdMarker
			}
			for _, id := range ids {
				if id == "" {
					t.Errorf("refuse versionless cleanup on versioned key %q", key)
					continue
				}
				query := url.Values{"versionId": {id}}
				if _, err := p.client.Object.Delete(ctx, key, &cos.ObjectDeleteOptions{XOptionQuery: &query}); err != nil {
					t.Errorf("cleanup exact version key=%q version=%q: %v", key, id, err)
				}
			}
		}
		if _, err := p.client.Object.Head(ctx, key, nil); !cos.IsNotFoundError(err) {
			t.Errorf("cleanup absence not confirmed for exact key %q: %v", key, err)
		}
	}
	// A lost initiation reply gives no upload ID. Do not discover-and-abort by a
	// broad prefix; report the isolated run prefix for explicit operator recovery.
	t.Logf("cleanup attempted generated exact keys/known upload IDs only; allocation/lost-response uncertainty requires inspecting %s", p.prefix)
}

func (p *liveCOSProbe) put(t *testing.T, key, data string, headers http.Header) *cos.Response {
	t.Helper()
	response, err := p.client.Object.Put(p.ctx, key, strings.NewReader(data), &cos.ObjectPutOptions{ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{XOptionHeader: &headers}})
	if err != nil {
		t.Fatalf("put generated fixture %q: %v", key, err)
	}
	return response
}

func (p *liveCOSProbe) bytes(t *testing.T, key, want string) {
	t.Helper()
	response, err := p.client.Object.Get(p.ctx, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || string(data) != want {
		t.Fatalf("native GET %q: len=%d read=%v close=%v; want len=%d", key, len(data), readErr, closeErr, len(want))
	}
}

func (p *liveCOSProbe) multipart(t *testing.T, key, data string) *cos.CompleteMultipartUploadOptions {
	t.Helper()
	result, _, err := p.client.Object.InitiateMultipartUpload(p.ctx, key, nil)
	if err != nil {
		t.Fatalf("initiation outcome may be unknown; inspect isolated prefix %s: %v", p.prefix, err)
	}
	if result.UploadID == "" {
		t.Fatal("initiation returned no upload ID")
	}
	p.uploads[result.UploadID] = key
	response, err := p.client.Object.UploadPart(p.ctx, key, result.UploadID, 1, strings.NewReader(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	return &cos.CompleteMultipartUploadOptions{Parts: []cos.Object{{PartNumber: 1, ETag: response.Header.Get("ETag")}}}
}

func (p *liveCOSProbe) complete(key string, opts *cos.CompleteMultipartUploadOptions) (*cos.Response, error) {
	for id, uploadKey := range p.uploads {
		if uploadKey == key {
			_, response, err := p.client.Object.CompleteMultipartUpload(p.ctx, key, id, opts)
			return response, err
		}
	}
	return nil, fmt.Errorf("no captured upload ID for generated key")
}

// classifyMutation checks actual native state after both successful and rejected
// requests. A condition header being accepted is not evidence it was enforced.
func (p *liveCOSProbe) classifyMutation(t *testing.T, key, before, after string, response *cos.Response, err error, negative bool, deleting bool) {
	t.Helper()
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	if err == nil {
		if deleting {
			if _, err := p.client.Object.Head(p.ctx, key, nil); !cos.IsNotFoundError(err) {
				t.Fatalf("successful delete did not remove fixture: %v", err)
			}
		} else {
			p.bytes(t, key, after)
		}
		if negative {
			t.Logf("UNSUPPORTED for this request: negative native condition ignored, HTTP %d, effect observed", status)
		} else {
			t.Logf("positive native request applied, HTTP %d; no provider-wide atomicity/fencing claim", status)
		}
		return
	}
	if status != 412 && status != 409 && status != 400 && status != 501 {
		t.Fatalf("infrastructure/unknown native outcome HTTP %d: %v", status, err)
	}
	p.bytes(t, key, before)
	if !negative && status == 412 {
		t.Fatalf("matching condition unexpectedly rejected with HTTP 412: %v", err)
	}
	t.Logf("native request rejected HTTP %d, unchanged bytes confirmed; 400/501=unsupported request, 409 alone is not proof of a version condition", status)
}

func TestCOSNativeCapabilities(t *testing.T) {
	configuration, enabled, err := dedicatedCOSConfig(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Skip("live COS UNVERIFIED: set EAM_PROBE_COS_ENABLE=1 and dedicated EAM_PROBE_COS_{BUCKET,REGION,SECRET_ID,SECRET_KEY,ROOT}; no application config/.env is loaded")
	}
	p := newLiveCOSProbe(t, configuration)
	t.Run("native_tree_ranges_and_stale_reads", func(t *testing.T) { p.probeReads(t) })
	t.Run("copy_independence", func(t *testing.T) {
		source, target := p.key(t, "copy-source"), p.key(t, "copy-target")
		p.put(t, source, "original", nil)
		if _, _, err := p.client.Object.Copy(p.ctx, target, p.host+"/"+source, nil); err != nil {
			t.Fatal(err)
		}
		p.put(t, source, "changed", nil)
		p.bytes(t, target, "original")
		if _, err := p.client.Object.Delete(p.ctx, source); err != nil {
			t.Fatal(err)
		}
		p.bytes(t, target, "original")
	})
	for _, operation := range []string{"put", "copy", "complete", "delete"} {
		for _, condition := range []string{"matching_if_match", "stale_if_match", "if_none_match_star", "forbid_overwrite"} {
			if operation == "delete" && condition != "matching_if_match" && condition != "stale_if_match" {
				continue
			}
			t.Run(operation+"/"+condition, func(t *testing.T) { p.probeMutation(t, operation, condition) })
		}
	}
	t.Run("copy_source_condition_is_distinct_from_target", func(t *testing.T) {
		source, target := p.key(t, "source-condition-source"), p.key(t, "source-condition-target")
		version := p.put(t, source, "captured", nil).Header.Get("ETag")
		p.put(t, source, "changed", nil)
		p.put(t, target, "destination", nil)
		_, response, err := p.client.Object.Copy(p.ctx, target, p.host+"/"+source, &cos.ObjectCopyOptions{ObjectCopyHeaderOptions: &cos.ObjectCopyHeaderOptions{XCosCopySourceIfMatch: version}})
		p.classifyMutation(t, target, "destination", "changed", response, err, true, false)
	})
	t.Log("live conditions are per-operation observations only; atomic visibility, delayed provider effects, ABA and recovery quiescence remain unverified")
}

func (p *liveCOSProbe) probeReads(t *testing.T) {
	t.Helper()
	key := p.key(t, "Tree/ Spelling +#?% 中文.txt ")
	marker := p.key(t, "Tree/Empty/")
	zero := p.key(t, "Tree/zero")
	first := p.put(t, key, "0123456789", nil)
	oldETag, oldVersion := first.Header.Get("ETag"), first.Header.Get("x-cos-version-id")
	if oldETag == "" {
		t.Fatal("native PUT returned no ETag")
	}
	p.put(t, marker, "", nil)
	p.put(t, zero, "", nil)
	p.bytes(t, zero, "")
	p.bytes(t, marker, "")
	listing, _, err := p.client.Bucket.Get(p.ctx, &cos.BucketGetOptions{Prefix: p.prefix + "Tree/", MaxKeys: 100})
	if err != nil || listing.IsTruncated {
		t.Fatalf("native fixture listing incomplete: %v", err)
	}
	found := make(map[string]bool)
	for _, object := range listing.Contents {
		found[object.Key] = true
	}
	if len(found) != 3 || !found[key] || !found[marker] || !found[zero] {
		t.Fatalf("exact native spelling/empty markers lost: %v", found)
	}
	for _, tc := range []struct{ request, want, contentRange string }{{"bytes=2-5", "2345", "bytes 2-5/10"}, {"bytes=9-9", "9", "bytes 9-9/10"}} {
		headers := http.Header{"If-Match": {oldETag}}
		response, err := p.client.Object.Get(p.ctx, key, &cos.ObjectGetOptions{Range: tc.request, XOptionHeader: &headers})
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 1024))
		closeErr := response.Body.Close()
		if response.StatusCode != 206 || response.Header.Get("Content-Range") != tc.contentRange || response.Header.Get("Content-Length") != fmt.Sprint(len(tc.want)) || string(body) != tc.want || readErr != nil || closeErr != nil {
			t.Fatalf("native exact range %s: HTTP %d headers=%v body=%q read=%v close=%v", tc.request, response.StatusCode, response.Header, body, readErr, closeErr)
		}
	}
	response, err := p.client.Object.Get(p.ctx, key, &cos.ObjectGetOptions{Range: "bytes=10-11"})
	if err == nil {
		response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != 416 {
		t.Fatalf("unsatisfiable native range must return 416: response=%v err=%v", response, err)
	}
	notModified := http.Header{"If-None-Match": {oldETag}}
	response, err = p.client.Object.Get(p.ctx, key, &cos.ObjectGetOptions{XOptionHeader: &notModified})
	if err == nil {
		response.Body.Close()
	}
	if response == nil || response.StatusCode != 304 {
		t.Fatalf("native matching If-None-Match should return 304: %v, %v", response, err)
	}
	p.put(t, key, "abcdefghij", nil)
	stale := http.Header{"If-Match": {oldETag}}
	response, err = p.client.Object.Get(p.ctx, key, &cos.ObjectGetOptions{Range: "bytes=2-5", XOptionHeader: &stale})
	if err == nil {
		response.Body.Close()
		t.Fatal("stale native read condition was ignored")
	}
	if response == nil || response.StatusCode != 412 {
		t.Fatalf("stale native read must reject with 412: %v, %v", response, err)
	}
	p.bytes(t, key, "abcdefghij")
	if oldVersion != "" && oldVersion != "null" {
		response, err := p.client.Object.Get(p.ctx, key, &cos.ObjectGetOptions{Range: "bytes=2-5"}, oldVersion)
		if err != nil {
			t.Fatal("native captured version read: ", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 1024))
		closeErr := response.Body.Close()
		if response.StatusCode != 206 || response.Header.Get("Content-Range") != "bytes 2-5/10" || string(body) != "2345" || readErr != nil || closeErr != nil {
			t.Fatalf("captured native version range: HTTP %d body=%q read=%v close=%v", response.StatusCode, body, readErr, closeErr)
		}
		t.Log("captured versionId range remained readable after replacement; future retention is not established")
	} else {
		t.Log("UNVERIFIED: immutable versionId reads; dedicated bucket returned no retained version ID")
	}
	// Reusing content reproduces ETag on many backends. Never call an ETag a
	// monotonic path generation or rely on it for preventing ABA stale effects.
	last := p.put(t, key, "0123456789", nil).Header.Get("ETag")
	t.Logf("ETag ABA observation: original=%q restored=%q equal=%t; ETag remains opaque", oldETag, last, oldETag == last)
}

func (p *liveCOSProbe) probeMutation(t *testing.T, operation, condition string) {
	t.Helper()
	key := p.key(t, operation+"-"+condition)
	before, after := "baseline", "replacement"
	version := p.put(t, key, before, nil).Header.Get("ETag")
	headers := make(http.Header)
	negative := condition != "matching_if_match"
	switch condition {
	case "matching_if_match":
		headers.Set("If-Match", version)
	case "stale_if_match":
		// Capture a real old validator, then change the actual final path before
		// the delayed request. This probes native semantics, not a Stat/write shim.
		p.put(t, key, "newer-generation", nil)
		before = "newer-generation"
		headers.Set("If-Match", version)
	case "if_none_match_star":
		headers.Set("If-None-Match", "*")
	case "forbid_overwrite":
		headers.Set("x-cos-forbid-overwrite", "true")
	}
	var response *cos.Response
	var err error
	switch operation {
	case "put":
		response, err = p.client.Object.Put(p.ctx, key, strings.NewReader(after), &cos.ObjectPutOptions{ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{XOptionHeader: &headers}})
	case "copy":
		source := p.key(t, operation+"-"+condition+"-source")
		p.put(t, source, after, nil)
		_, response, err = p.client.Object.Copy(p.ctx, key, p.host+"/"+source, &cos.ObjectCopyOptions{ObjectCopyHeaderOptions: &cos.ObjectCopyHeaderOptions{XOptionHeader: &headers}})
	case "complete":
		// 1 MiB avoids depending on a provider exception for a tiny final part.
		after = strings.Repeat("x", 1<<20)
		opts := p.multipart(t, key, after)
		p.bytes(t, key, before) // Receiving the unpublished part must not publish.
		opts.XOptionHeader = &headers
		response, err = p.complete(key, opts)
	case "delete":
		response, err = p.client.Object.Delete(p.ctx, key, &cos.ObjectDeleteOptions{XOptionHeader: &headers})
	}
	p.classifyMutation(t, key, before, after, response, err, negative, operation == "delete")
	// Create-only positives must also exercise absent destinations. A rejected
	// occupied request alone could just mean an invalid/unsupported header.
	if condition == "if_none_match_star" || condition == "forbid_overwrite" {
		absent := p.key(t, operation+"-"+condition+"-absent")
		switch operation {
		case "put":
			response, err = p.client.Object.Put(p.ctx, absent, strings.NewReader(after), &cos.ObjectPutOptions{ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{XOptionHeader: &headers}})
		case "copy":
			source := p.key(t, operation+"-"+condition+"-source")
			_, response, err = p.client.Object.Copy(p.ctx, absent, p.host+"/"+source, &cos.ObjectCopyOptions{ObjectCopyHeaderOptions: &cos.ObjectCopyHeaderOptions{XOptionHeader: &headers}})
		case "complete":
			opts := p.multipart(t, absent, after)
			opts.XOptionHeader = &headers
			response, err = p.complete(absent, opts)
		}
		if err == nil {
			p.bytes(t, absent, after)
			t.Logf("absent-destination positive applied HTTP %d; pair with occupied negative before classifying create-only support", response.StatusCode)
		} else if response != nil && (response.StatusCode == 400 || response.StatusCode == 501) {
			if _, err := p.client.Object.Head(p.ctx, absent, nil); !cos.IsNotFoundError(err) {
				t.Fatalf("rejected create-only positive left native effect: %v", err)
			}
			t.Logf("UNSUPPORTED request: absent-destination positive HTTP %d", response.StatusCode)
		} else {
			t.Fatalf("absent-destination positive failed: %v", err)
		}
	}
}
