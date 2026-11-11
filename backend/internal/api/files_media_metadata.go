package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/mediameta"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storage"
)

// filesMediaMetadata is what a photo or video states about its own capture. State says
// how to read the null fields: "parsed" found at least one fact, "empty" read the file
// and found none, "unsupported" means the file was not read at all (not a container this
// service parses, or a connection without ranged reads).
type filesMediaMetadata struct {
	ID    string `json:"id"`
	State string `json:"state"`
	mediameta.Info
}

// handleFilesMediaMetadata answers GET B/media-metadata for one file.
//
// Nothing is parsed at upload: with presigned parts the bytes never cross this service,
// and most files are never opened. The first request for a file reads the few ranges that
// hold its capture facts (see mediameta for the bound) and stores the result; later
// requests are one index read and one cache read, with no storage request.
//
// Read permission on the path is the whole authorization, for cookies and tokens alike:
// every fact returned here is inside bytes the caller may already download.
func (s *Server) handleFilesMediaMetadata(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	q, err := s.filesReadSelectorQuery(r, f, true)
	if err != nil {
		return err
	}
	metadata, record, err := s.filesIndexedEntry(r.Context(), f, q.Path)
	if err != nil {
		return err
	}
	if q.SelectorID != "" && metadata.ID != q.SelectorID {
		return fc.NewProblem(404, "not_found", "The selected file changed location.")
	}
	if metadata.Kind != "file" {
		return fc.NewProblem(409, "not_a_file", "The selected entry is not a file.")
	}
	mediaType := ""
	if metadata.MediaType != nil {
		mediaType = *metadata.MediaType
	}
	// Decided from the name and type alone, so a document never costs a storage request.
	if !mediameta.Supported(metadata.Name, mediaType) || !f.Driver.Describe().RangeRead {
		return writeFilesJSON(w, 200, filesMediaMetadata{ID: metadata.ID, State: "unsupported"})
	}
	respond := func(info mediameta.Info) error {
		state := "parsed"
		if info.Empty() {
			state = "empty"
		}
		return writeFilesJSON(w, 200, filesMediaMetadata{ID: metadata.ID, State: state, Info: info})
	}

	// The index's content revision names the bytes without touching storage. Like every
	// indexed read it can trail a change made outside the application until the next
	// sync; in exchange a repeat view of a file is free.
	indexed := ""
	if record.ContentRevision != nil {
		indexed = *record.ContentRevision
	}
	if info, ok := s.filesMediaCached(r.Context(), f, metadata.ID, indexed); ok {
		return respond(info)
	}

	source, err := s.filesMediaSource(r.Context(), f, q.Path, record.Row)
	if err != nil {
		return err
	}
	// The revision of the object actually observed is the key the result is stored
	// under. It equals the indexed one when the index is current, and differs exactly
	// when storing under the indexed key would describe other bytes.
	observed := ""
	if revision, err := s.filesObservedContentRevision(r.Context(), f, source.entry); err != nil {
		return err
	} else if revision != nil {
		observed = *revision
	}
	if observed != indexed {
		if info, ok := s.filesMediaCached(r.Context(), f, metadata.ID, observed); ok {
			return respond(info)
		}
	}
	info, err := mediameta.Parse(r.Context(), source)
	if err != nil {
		return err
	}
	if s.mediaCache != nil && observed != "" {
		// A lost write costs the next viewer one more parse; it must not fail this read.
		if err := s.mediaCache.Put(r.Context(), f.Principal.User.ID, metadata.ID, observed, info); err != nil {
			slog.Warn("media metadata cache write failed", "request_id", w.Header().Get("X-Request-Id"), "owner_id", f.Principal.User.ID, "entry_id", metadata.ID, "error", err)
		}
	}
	return respond(info)
}

// filesMediaCached reads the cache and treats its failure as a miss: the file itself can
// still answer, so a cache outage degrades to a slower response, not an error.
func (s *Server) filesMediaCached(ctx context.Context, f *fileRequest, entryID, contentKey string) (mediameta.Info, bool) {
	if s.mediaCache == nil || contentKey == "" {
		return mediameta.Info{}, false
	}
	info, found, err := s.mediaCache.Get(ctx, f.Principal.User.ID, entryID, contentKey)
	if err != nil {
		slog.Warn("media metadata cache read failed", "owner_id", f.Principal.User.ID, "entry_id", entryID, "error", err)
		return mediameta.Info{}, false
	}
	return info, found
}

// filesMediaReader is ranged access to one observed object for the parser.
type filesMediaReader struct {
	path  storage.Path
	entry storage.Entry
	open  func(context.Context, storage.ReadOptions) (storage.Read, error)
}

func (m *filesMediaReader) Size() int64 { return m.entry.Size }

func (m *filesMediaReader) ReadAt(ctx context.Context, offset int64, length int) ([]byte, error) {
	n := int64(length)
	options := storage.ReadOptions{Offset: offset, Length: &n, PinnedVersion: m.entry.Revision}
	read, err := m.open(ctx, options)
	if err != nil {
		return nil, err
	}
	defer read.Body.Close()
	if err := filesValidateRead(m.path, read, options); err != nil {
		return nil, err
	}
	// Ranges from two different objects must never be stitched into one parse.
	if read.Entry.Size != m.entry.Size {
		return nil, storage.NewError("changed", m.path, nil)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(read.Body, data); err != nil {
		return nil, err
	}
	return data, nil
}

// filesMediaSource observes the file once and returns ranged access to that observation.
// The index-named key is used when the driver can read by reference (one HEAD, then one
// GET per range, no ancestor listing); otherwise the path is proven natively.
func (s *Server) filesMediaSource(ctx context.Context, f *fileRequest, p fc.Path, row nodes.Row) (*filesMediaReader, error) {
	native := storage.Path(p.Relative())
	if reader, ok := f.Driver.(storage.ReferenceReader); ok && row.Key != "" {
		ref := []byte(row.Key)
		entry, err := reader.StatReference(ctx, native, ref)
		switch {
		case err == nil:
			if entry.Kind != storage.File {
				return nil, fc.NewProblem(409, "not_a_file", "The path is not a file.")
			}
			return &filesMediaReader{path: native, entry: entry, open: func(ctx context.Context, options storage.ReadOptions) (storage.Read, error) {
				return reader.OpenReference(ctx, native, ref, options)
			}}, nil
		case storage.IsCode(err, "not_found"):
			return nil, filesNativeAbsent(p)
		// A key from another root generation proves nothing about this tree; fall
		// through to the traversal that does.
		case !storage.IsCode(err, "stale_reference"):
			return nil, err
		}
	}
	entry, err := f.Driver.Stat(ctx, native)
	if err != nil {
		return nil, err
	}
	if entry.Kind != storage.File {
		return nil, fc.NewProblem(409, "not_a_file", "The path is not a file.")
	}
	return &filesMediaReader{path: native, entry: entry, open: func(ctx context.Context, options storage.ReadOptions) (storage.Read, error) {
		return f.Driver.Open(ctx, native, options)
	}}, nil
}
