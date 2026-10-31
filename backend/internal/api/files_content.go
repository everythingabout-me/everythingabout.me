package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/storage"
)

// filesContentRevision identifies genuine native content evidence without exposing
// provider versions/locators. namespace binds the owner and native root, never the
// requesting credential. Native versions can be unique only within one object, so
// exact native path and any native object identity are part of the tuple. Version
// must select immutable content within that identity; an unpinnable change counter
// is insufficient. P2 external renames change this value; preserving it across
// moves requires a future durable content-generation anchor.
// The caller must prove native version selection before using it as a strong ETag.
func filesContentRevision(namespace string, e storage.Entry) *string {
	if e.Kind != storage.File || len(e.Version) == 0 {
		return nil
	}
	h := sha256.New()
	h.Write([]byte("eam/files/content/v1\x00"))
	for _, value := range [][]byte{[]byte(namespace), []byte(e.Path), e.NativeRef, e.Version} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		h.Write(length[:])
		h.Write(value)
	}
	revision := "cr_" + hex.EncodeToString(h.Sum(nil))
	return &revision
}

func (s *Server) handleFilesContent(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	p, err := fc.ParseQueryPath(r.URL.RawQuery)
	if err != nil {
		return err
	}
	if err := f.RequireRead(p); err != nil {
		return err
	}
	native := storage.Path(p.Relative())
	// Bound retries cover native replacements between metadata selection and Open.
	// All headers come from Open's actual representation, never the preceding Stat.
	for attempt := 0; attempt < 3; attempt++ {
		entry, err := f.Driver.Stat(r.Context(), native)
		if err != nil {
			return err
		}
		if entry.Kind != storage.File {
			return fc.NewProblem(409, "not_a_file", "The path is not a file.")
		}
		read, err := f.Driver.Open(r.Context(), native, storage.ReadOptions{Version: entry.Version})
		if err != nil {
			if storage.IsCode(err, "changed") {
				continue
			}
			return err
		}
		if err := filesValidateRead(native, read, storage.ReadOptions{Version: entry.Version}); err != nil {
			if closeErr := filesCloseRead(read); closeErr != nil {
				return closeErr
			}
			if storage.IsCode(err, "changed") {
				continue
			}
			return err
		}
		// An unversioned Stat may race a versioned GET. Reopen that exact version
		// before exposing its revision, including for HEAD and conditional responses.
		if len(entry.Version) == 0 && len(read.Entry.Version) != 0 {
			selected := bytes.Clone(read.Entry.Version)
			if err := read.Body.Close(); err != nil {
				return err
			}
			read, err = f.Driver.Open(r.Context(), native, storage.ReadOptions{Version: selected})
			if err != nil {
				if storage.IsCode(err, "changed") {
					continue
				}
				return err
			}
			if err := filesValidateRead(native, read, storage.ReadOptions{Version: selected}); err != nil {
				if closeErr := filesCloseRead(read); closeErr != nil {
					return closeErr
				}
				if storage.IsCode(err, "changed") {
					continue
				}
				return err
			}
		}
		tag := ""
		if revision := filesContentRevision(f.ContentNamespace, read.Entry); revision != nil {
			tag = strconv.Quote(*revision)
		}
		condition := filesContentCondition(r.Header, tag)
		if condition != 0 {
			if err := read.Body.Close(); err != nil {
				return err
			}
			if condition == http.StatusPreconditionFailed {
				return fc.NewProblem(condition, "precondition_failed", "The file does not match the requested content condition.")
			}
			filesContentCacheHeaders(w.Header(), tag)
			w.WriteHeader(condition)
			return nil
		}
		status := http.StatusOK
		var selected *filesByteRange
		if r.Method != http.MethodHead && f.Driver.Describe().RangeRead && filesIfRange(r.Header, tag) {
			selected, err = filesParseRange(strings.Join(r.Header.Values("Range"), ","), read.Entry.Size)
			if err != nil {
				size := read.Entry.Size
				if err := read.Body.Close(); err != nil {
					return err
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
				return err
			}
		}
		if selected != nil {
			original := read.Entry
			if err := read.Body.Close(); err != nil {
				return err
			}
			options := storage.ReadOptions{Offset: selected.offset, Length: &selected.length, Version: original.Version}
			read, err = f.Driver.Open(r.Context(), native, options)
			if err != nil {
				if storage.IsCode(err, "changed") || storage.IsCode(err, "invalid_range") {
					continue
				}
				return err
			}
			if err := filesValidateRead(native, read, options); err != nil {
				if closeErr := filesCloseRead(read); closeErr != nil {
					return closeErr
				}
				if storage.IsCode(err, "changed") {
					continue
				}
				return err
			}
			actual, rangeErr := filesParseRange(strings.Join(r.Header.Values("Range"), ","), read.Entry.Size)
			// A suffix/clamped range can change when an unversioned file grows. Retry
			// selection instead of pairing old bounds/conditions with new GET bytes.
			if rangeErr != nil || actual == nil || *actual != *selected || read.Entry.Size != original.Size || !bytes.Equal(read.Entry.Version, original.Version) || !bytes.Equal(read.Entry.NativeRef, original.NativeRef) {
				if err := read.Body.Close(); err != nil {
					return err
				}
				continue
			}
			status = http.StatusPartialContent
		}
		if err := r.Context().Err(); err != nil {
			read.Body.Close()
			return err
		}
		if r.Method == http.MethodHead {
			if err := read.Body.Close(); err != nil {
				return err
			}
		}
		header := w.Header()
		filesContentCacheHeaders(header, tag)
		header.Set("Content-Type", filesContentType(read.Entry))
		disposition := "attachment"
		if r.URL.Query().Get("disposition") == "inline" && filesSafeInline(header.Get("Content-Type")) {
			disposition = "inline"
		}
		header.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": path.Base(string(p))}))
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Content-Length", strconv.FormatInt(read.Length, 10))
		if f.Driver.Describe().RangeRead {
			header.Set("Accept-Ranges", "bytes")
		}
		if selected != nil {
			header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", selected.offset, selected.offset+selected.length-1, read.Entry.Size))
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(status)
			return nil
		}
		// Cancellation must unblock a native Read, including a blocked network read.
		// Native bodies need not support concurrent duplicate Close calls.
		var closeOnce sync.Once
		var closeErr error
		closeBody := func() error {
			closeOnce.Do(func() { closeErr = read.Body.Close() })
			return closeErr
		}
		stop := context.AfterFunc(r.Context(), func() { _ = closeBody() })
		defer stop()
		defer func() {
			if err := closeBody(); err != nil {
				panic(http.ErrAbortHandler)
			}
		}()
		w.WriteHeader(status)
		reader := &filesContextReader{ctx: r.Context(), reader: read.Body}
		if _, err := io.CopyN(w, reader, read.Length); err != nil || reader.failure != nil {
			panic(http.ErrAbortHandler)
		}
		// Verify EOF as well as length; drivers can discover truncation/transport errors
		// on the read after the last advertised byte. Never append a JSON error here.
		var extra [1]byte
		n, err := reader.Read(extra[:])
		if n != 0 || err != io.EOF {
			panic(http.ErrAbortHandler)
		}
		return nil
	}
	return storage.NewError("changed", native, nil)
}

func filesCloseRead(read storage.Read) error {
	if read.Body == nil {
		return nil
	}
	return read.Body.Close()
}

func filesValidateRead(expected storage.Path, read storage.Read, options storage.ReadOptions) error {
	if read.Body == nil {
		return storage.NewError("invalid_backend_response", expected, nil)
	}
	if read.Entry.Path != expected || read.Entry.Kind != storage.File || read.Entry.Size < 0 || read.Entry.Size > 1<<53-1 || read.Offset != options.Offset || read.Length < 0 || read.Offset > read.Entry.Size || read.Length > read.Entry.Size-read.Offset {
		return storage.NewError("invalid_backend_response", read.Entry.Path, nil)
	}
	if len(options.Version) != 0 && !bytes.Equal(options.Version, read.Entry.Version) {
		return storage.NewError("changed", read.Entry.Path, nil)
	}
	if options.Length != nil {
		if read.Length != *options.Length {
			return storage.NewError("invalid_backend_response", read.Entry.Path, nil)
		}
	} else if read.Length != read.Entry.Size-read.Offset {
		return storage.NewError("invalid_backend_response", read.Entry.Path, nil)
	}
	return nil
}

func filesContentCacheHeaders(header http.Header, tag string) {
	header.Set("Cache-Control", "private, no-cache")
	if tag != "" {
		header.Set("ETag", tag)
	} else {
		header.Del("ETag")
	}
}

func filesContentType(entry storage.Entry) string {
	value := entry.MediaType
	if value == "" {
		value = mime.TypeByExtension(path.Ext(string(entry.Path)))
	}
	if kind, params, err := mime.ParseMediaType(value); err == nil {
		return mime.FormatMediaType(kind, params)
	}
	return "application/octet-stream"
}

func filesSafeInline(value string) bool {
	kind, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	switch kind {
	case "text/plain", "image/jpeg", "image/png", "image/gif", "image/webp", "image/avif", "image/bmp", "image/x-icon", "image/vnd.microsoft.icon",
		"audio/mpeg", "audio/mp4", "audio/ogg", "audio/wav", "audio/webm", "audio/flac", "video/mp4", "video/webm", "video/ogg":
		return true
	}
	return false
}

type filesContextReader struct {
	ctx     context.Context
	reader  io.Reader
	failure error
}

func (r *filesContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if err != nil && err != io.EOF {
		r.failure = err
	}
	return n, err
}

func filesContentCondition(header http.Header, tag string) int {
	if values := header.Values("If-Match"); len(values) != 0 && !filesETagMatches(strings.Join(values, ","), tag, false) {
		return http.StatusPreconditionFailed
	}
	if values := header.Values("If-None-Match"); len(values) != 0 && filesETagMatches(strings.Join(values, ","), tag, true) {
		return http.StatusNotModified
	}
	return 0
}

func filesIfRange(header http.Header, tag string) bool {
	values := header.Values("If-Range")
	return len(values) == 0 || len(values) == 1 && tag != "" && strings.TrimSpace(values[0]) == tag
}

// If-None-Match uses HTTP weak comparison; If-Match and If-Range require
// strong comparison. Wildcards test existence, which Open has already proven.
func filesETagMatches(value, tag string, weak bool) bool {
	value = strings.TrimSpace(value)
	if value == "*" {
		return true
	}
	matched := false
	for value != "" {
		value = strings.TrimLeft(value, " \t")
		isWeak := strings.HasPrefix(value, "W/")
		if isWeak {
			value = value[2:]
		}
		if len(value) == 0 || value[0] != '"' {
			return false
		}
		end := strings.IndexByte(value[1:], '"')
		if end < 0 {
			return false
		}
		candidate := value[:end+2]
		for _, b := range []byte(candidate[1 : len(candidate)-1]) {
			if b < 0x21 || b == 0x7f {
				return false
			}
		}
		if tag != "" && candidate == tag && (weak || !isWeak) {
			matched = true
		}
		value = strings.TrimLeft(value[end+2:], " \t")
		if value == "" {
			break
		}
		if value[0] != ',' {
			return false
		}
		value = strings.TrimLeft(value[1:], " \t")
		if value == "" {
			return false
		}
	}
	return matched
}

type filesByteRange struct{ offset, length int64 }

func filesParseRange(value string, size int64) (*filesByteRange, error) {
	invalid := func() (*filesByteRange, error) {
		return nil, fc.NewProblem(http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable", "The requested byte range is not satisfiable.")
	}
	if value == "" || strings.Contains(value, ",") {
		return nil, nil
	}
	if !strings.HasPrefix(value, "bytes=") {
		return nil, nil
	}
	bounds := strings.Split(strings.TrimSpace(strings.TrimPrefix(value, "bytes=")), "-")
	if len(bounds) != 2 || size == 0 {
		return invalid()
	}
	parse := func(value string) (int64, error) {
		if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0, strconv.ErrSyntax
		}
		return strconv.ParseInt(value, 10, 64)
	}
	if bounds[0] == "" {
		suffix, err := parse(bounds[1])
		if err != nil || suffix == 0 {
			return invalid()
		}
		if suffix > size {
			suffix = size
		}
		return &filesByteRange{offset: size - suffix, length: suffix}, nil
	}
	start, err := parse(bounds[0])
	if err != nil || start >= size {
		return invalid()
	}
	end := size - 1
	if bounds[1] != "" {
		end, err = parse(bounds[1])
		if err != nil || end < start {
			return invalid()
		}
		if end >= size {
			end = size - 1
		}
	}
	return &filesByteRange{offset: start, length: end - start + 1}, nil
}
