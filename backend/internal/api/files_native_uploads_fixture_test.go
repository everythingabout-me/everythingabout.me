package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

type objectAPIMultipart struct {
	key, media string
	metadata   map[string]string
	parts      map[int][]byte
	tags       map[int]string
}

// Called while the fixture mutex is held. This is isolated provider behavior,
// never capability evidence about a real deployment.
func (p *objectAPIProvider) serveMultipart(t *testing.T, w http.ResponseWriter, r *http.Request, key string) bool {
	if r.Method == "POST" && r.URL.Query().Has("uploads") {
		if p.multipart == nil {
			p.multipart = map[string]*objectAPIMultipart{}
			p.completeRequests = map[string]int{}
		}
		p.sequence++
		id := fmt.Sprintf("native-upload-%d", p.sequence)
		meta := map[string]string{}
		for k, v := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
				meta[strings.TrimPrefix(strings.ToLower(k), "x-amz-meta-")] = v[0]
			}
		}
		p.multipart[id] = &objectAPIMultipart{key: key, media: r.Header.Get("Content-Type"), metadata: meta, parts: map[int][]byte{}, tags: map[int]string{}}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, key, id)
		return true
	}
	id := r.URL.Query().Get("uploadId")
	if id == "" {
		return false
	}
	upload := p.multipart[id]
	if upload == nil || upload.key != key {
		w.WriteHeader(404)
		io.WriteString(w, `<Error><Code>NoSuchUpload</Code></Error>`)
		return true
	}
	w.Header().Set("Content-Type", "application/xml")
	switch r.Method {
	case "PUT":
		n, e := strconv.Atoi(r.URL.Query().Get("partNumber"))
		if e != nil {
			t.Error(e)
			w.WriteHeader(400)
			return true
		}
		data, e := io.ReadAll(r.Body)
		if e != nil {
			t.Error(e)
			w.WriteHeader(400)
			return true
		}
		sum := sha256.Sum256(data)
		tag := `"` + hex.EncodeToString(sum[:]) + `"`
		upload.parts[n] = data
		upload.tags[n] = tag
		p.partRequests++
		w.Header().Set("ETag", tag)
		w.WriteHeader(200)
	case "POST":
		p.completeRequests[key]++
		current, exists := p.current(key)
		if r.Header.Get("If-None-Match") == "*" && exists || r.Header.Get("If-Match") != "" && (!exists || r.Header.Get("If-Match") != current.ETag) {
			w.WriteHeader(412)
			io.WriteString(w, `<Error><Code>PreconditionFailed</Code></Error>`)
			return true
		}
		var manifest struct {
			Parts []struct {
				Number int    `xml:"PartNumber"`
				ETag   string `xml:"ETag"`
			} `xml:"Part"`
		}
		if e := xml.NewDecoder(r.Body).Decode(&manifest); e != nil {
			t.Error(e)
			w.WriteHeader(400)
			return true
		}
		var data bytes.Buffer
		for index, part := range manifest.Parts {
			if part.Number != index+1 || part.ETag != upload.tags[part.Number] {
				t.Error("invalid native part manifest")
				w.WriteHeader(400)
				return true
			}
			data.Write(upload.parts[part.Number])
		}
		result := p.record(key, data.Bytes(), upload.media, upload.metadata, false)
		delete(p.multipart, id)
		if p.lostReplyKey == key {
			conn, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
			} else {
				conn.Close()
			}
			return true
		}
		w.Header().Set("X-Amz-Version-Id", result.ID)
		fmt.Fprintf(w, `<CompleteMultipartUploadResult><Location>native</Location><Bucket>bucket</Bucket><Key>%s</Key><ETag>%s</ETag></CompleteMultipartUploadResult>`, key, result.ETag)
	case "DELETE":
		delete(p.multipart, id)
		w.WriteHeader(204)
	default:
		t.Errorf("unexpected native multipart method %s", r.Method)
		w.WriteHeader(400)
	}
	return true
}
