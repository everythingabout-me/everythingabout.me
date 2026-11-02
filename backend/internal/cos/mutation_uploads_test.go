package cos

import (
	"context"
	"encoding/xml"
	"hash/crc64"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
)

func TestMutationMultipartNativeWireAndConditionalComplete(t *testing.T) {
	var allocations, parts, completes, aborts atomic.Int32
	_, backend, _ := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/drive/a+%2F?#.bin" {
			t.Errorf("native key changed: %q", r.URL.Path)
		}
		switch {
		case r.Method == "POST" && r.URL.Query().Has("uploads"):
			allocations.Add(1)
			if r.Header.Get("X-Cos-Meta-Eam-Intent") != "intent" || r.Header.Get("Content-Type") != "application/custom" {
				t.Error("allocation metadata missing")
			}
			io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>fixture</Bucket><Key>key</Key><UploadId>private-id</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == "PUT" && r.URL.Query().Get("uploadId") == "private-id":
			parts.Add(1)
			data, e := io.ReadAll(r.Body)
			if e != nil || string(data) != "data" || r.ContentLength != 4 || r.URL.Query().Get("partNumber") != "1" {
				t.Errorf("bad native part: %q %v", data, e)
			}
			w.Header().Set("ETag", `"part-tag"`)
			w.Header().Set("X-Cos-Hash-Crc64ecma", strconv.FormatUint(crc64.Checksum(data, crc64.MakeTable(crc64.ECMA)), 10))
		case r.Method == "POST" && r.URL.Query().Get("uploadId") == "private-id":
			completes.Add(1)
			if r.Header.Get("If-None-Match") != "*" || r.Header.Get("X-Cos-Forbid-Overwrite") != "true" {
				t.Error("conditional native create not carried to Complete")
			}
			var manifest struct {
				Parts []struct {
					Number int    `xml:"PartNumber"`
					Tag    string `xml:"ETag"`
				} `xml:"Part"`
			}
			if e := xml.NewDecoder(r.Body).Decode(&manifest); e != nil || len(manifest.Parts) != 1 || manifest.Parts[0].Tag != `"part-tag"` {
				t.Errorf("bad completion manifest %+v %v", manifest, e)
			}
			w.Header().Set("X-Cos-Version-Id", "native-version")
			io.WriteString(w, `<CompleteMultipartUploadResult><Key>key</Key><ETag>final-tag</ETag></CompleteMultipartUploadResult>`)
		case r.Method == "DELETE" && r.URL.Query().Get("uploadId") == "private-id":
			aborts.Add(1)
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected native operation %s %s", r.Method, r.URL)
			w.WriteHeader(400)
		}
	}))
	native := backend.(objectmutation.MultipartBackend)
	input := objectmutation.Put{Key: "drive/a+%2F?#.bin", Size: 4, MediaType: "application/custom", Metadata: map[string]string{"eam-intent": "intent"}, Absent: true}
	id, err := native.BeginMultipart(context.Background(), input)
	if err != nil || id != "private-id" {
		t.Fatal(id, err)
	}
	tag, err := native.PutPart(context.Background(), input.Key, id, 1, strings.NewReader("data"), 4)
	if err != nil {
		t.Fatal(err)
	}
	out, err := native.CompleteMultipart(context.Background(), input, id, []objectmutation.NativePart{{Number: 1, ETag: tag}})
	if err != nil || out.VersionID != "native-version" || out.Size != 4 {
		t.Fatalf("native Complete %+v %v", out, err)
	}
	if err = native.AbortMultipart(context.Background(), input.Key, id); err != nil {
		t.Fatal(err)
	}
	if allocations.Load() != 1 || parts.Load() != 1 || completes.Load() != 1 || aborts.Load() != 1 {
		t.Fatal("native operation replayed")
	}
}

func TestMutationMultipartCompleteErrorsNeverReplay(t *testing.T) {
	for _, status := range []int{412, 500, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			_, backend, _ := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.Copy(io.Discard, r.Body)
				if r.Header.Get("If-Match") != `"old"` {
					t.Error("replacement condition omitted")
				}
				w.WriteHeader(status)
				io.WriteString(w, `<Error><Code>Failure</Code></Error>`)
			}))
			_, err := backend.(objectmutation.MultipartBackend).CompleteMultipart(context.Background(), objectmutation.Put{Key: "a", Size: 4, ExpectedETag: `"old"`}, "id", []objectmutation.NativePart{{Number: 1, ETag: "tag"}})
			expected := "outcome_unknown"
			if status == 412 {
				expected = "precondition_failed"
			}
			if !storage.IsCode(err, expected) || calls.Load() != 1 {
				t.Fatalf("calls=%d error=%v", calls.Load(), err)
			}
		})
	}
}
