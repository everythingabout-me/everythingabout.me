package api

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storage"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	filesDefaultLimit          = 200
	filesMaxLimit              = 1000
	filesReadBound             = 100000
	filesNativePageBound       = 1000
	filesMaxJSONBytes    int64 = 9007199254740991
	filesSort                  = "kind-folder-first,nfc-name-bytes,nfc-path-bytes:v1"
)

// FileReadIndex supplies application metadata and bounded search candidates. The
// driver remains authoritative for paths, existence, names and file attributes.
// Prefixes and paths are root-relative; implementations must isolate owner first.
type FileReadIndex interface {
	Live(context.Context, string, string) (*nodes.Row, error)
	FileReadRecords(context.Context, string, string, int) ([]nodes.Row, error)
}

type filesReadQuery struct {
	Path                             fc.Path
	Limit                            int
	Cursor, Keyword, Kind, MediaType string
}

func parseFilesReadQuery(r *http.Request, scope fc.Path, pathRequired bool) (filesReadQuery, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	invalid := func(detail string) (filesReadQuery, error) {
		return filesReadQuery{}, fc.NewProblem(400, "invalid_request", detail)
	}
	if err != nil {
		return invalid("The query encoding is invalid.")
	}
	for _, values := range q {
		if len(values) != 1 {
			return invalid("Query parameters must occur once.")
		}
	}
	p := string(scope)
	if v, ok := q["path"]; ok {
		p = v[0]
	} else if pathRequired {
		return invalid("Exactly one path query value is required.")
	}
	parsed, err := fc.ParsePath(p)
	if err != nil {
		return filesReadQuery{}, err
	}
	result := filesReadQuery{Path: parsed, Limit: filesDefaultLimit, Cursor: q.Get("cursor"), Keyword: q.Get("q"), Kind: q.Get("kind"), MediaType: q.Get("mediaType")}
	if values, ok := q["limit"]; ok {
		n, err := strconv.Atoi(values[0])
		if err != nil || n < 1 || n > filesMaxLimit {
			return invalid("Limit must be between 1 and 1000.")
		}
		result.Limit = n
	}
	if !utf8.ValidString(result.Keyword) || !utf8.ValidString(result.MediaType) || len(result.Keyword) > 4096 || len(result.MediaType) > 255 || len(result.Cursor) > 32768 {
		return invalid("The query exceeds the supported limits or contains invalid text.")
	}
	if result.Kind != "" && result.Kind != "file" && result.Kind != "folder" {
		return invalid("Kind must be file or folder.")
	}
	if strings.ContainsAny(result.MediaType, "\r\n\x00") {
		return invalid("The media type filter is invalid.")
	}
	return result, nil
}

func filesPathKey(f *fileRequest) fc.SegmentKey {
	if f.Driver.Describe().Names.CaseSensitive {
		return norm.NFC.String
	}
	return func(s string) string { return cases.Fold().String(norm.NFC.String(s)) }
}

func filesEntryPath(entry storage.Entry) (fc.Path, error) {
	p, err := fc.FromRelative(string(entry.Path))
	if err != nil || (entry.Kind != storage.File && entry.Kind != storage.Folder) || entry.Size < 0 || entry.Size > filesMaxJSONBytes {
		return "", fc.NewProblem(502, "storage_failure", "Storage returned an unsupported file representation.")
	}
	return p, nil
}

func filesStat(ctx context.Context, f *fileRequest, p fc.Path) (storage.Entry, error) {
	if err := f.RequireRead(p); err != nil {
		return storage.Entry{}, err
	}
	entry, err := f.Driver.Stat(ctx, storage.Path(p.Relative()))
	if err != nil {
		return storage.Entry{}, err
	}
	actual, err := filesEntryPath(entry)
	if err != nil {
		return storage.Entry{}, err
	}
	if err := f.RequireRead(actual); err != nil {
		return storage.Entry{}, err
	}
	if filesPathKey(f)(string(actual)) != filesPathKey(f)(string(p)) {
		return storage.Entry{}, fc.NewProblem(502, "storage_failure", "Storage returned an unexpected path.")
	}
	return entry, nil
}

// filesChildren exhausts the private driver cursor before applying public sorting.
// Driver cursors may use adapter-specific ordering and must never reach clients.
func filesChildren(ctx context.Context, f *fileRequest, parent storage.Entry) ([]storage.Entry, error) {
	return filesObserveChildren(ctx, f, parent, &filesObservationBudget{})
}

type filesObservationBudget struct{ pages, entries int }

func filesObserveChildren(ctx context.Context, f *fileRequest, parent storage.Entry, budget *filesObservationBudget) ([]storage.Entry, error) {
	if parent.Kind != storage.Folder {
		return nil, fc.NewProblem(409, "not_a_folder", "The selected path is not a folder.")
	}
	parentPath, err := filesEntryPath(parent)
	if err != nil {
		return nil, err
	}
	items := make([]storage.Entry, 0)
	seenPaths := make(map[string]bool)
	seenCursors := make(map[string]bool)
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if budget.pages >= filesNativePageBound {
			return nil, fc.NewProblem(503, "resource_limit", "The native directory read budget was exceeded.")
		}
		budget.pages++
		page, err := f.Driver.List(ctx, parent.Path, storage.PageRequest{Cursor: cursor, Limit: filesMaxLimit})
		if err != nil {
			return nil, err
		}
		if len(items)+len(page.Entries) > filesReadBound || budget.entries+len(page.Entries) > filesReadBound {
			return nil, fc.NewProblem(503, "resource_limit", "The native directory read budget was exceeded.")
		}
		budget.entries += len(page.Entries)
		for _, entry := range page.Entries {
			p, err := filesEntryPath(entry)
			if err != nil {
				return nil, err
			}
			if filesPathKey(f)(path.Dir(string(p))) != filesPathKey(f)(string(parentPath)) || p == parentPath {
				return nil, fc.NewProblem(502, "storage_failure", "Storage returned a child outside the selected folder.")
			}
			if err := f.RequireRead(p); err != nil {
				return nil, err
			}
			key := filesPathKey(f)(string(p))
			if seenPaths[key] {
				return nil, fc.NewProblem(409, "name_conflict", "The folder contains ambiguous native names.")
			}
			seenPaths[key] = true
			items = append(items, entry)
		}
		if page.NextCursor == "" {
			break
		}
		if len(page.Entries) == 0 || seenCursors[page.NextCursor] {
			return nil, fc.NewProblem(502, "storage_failure", "Storage returned an invalid listing cursor.")
		}
		seenCursors[page.NextCursor] = true
		cursor = page.NextCursor
	}
	return items, nil
}

// filesNativeObservations is request-local metadata, never a byte cache or a
// snapshot. Each needed directory is listed once, so sibling candidates share
// native observations instead of triggering repeated driver Stat scans.
type filesNativeObservations struct {
	root        storage.Entry
	directories map[storage.Path][]storage.Entry
	byName      map[storage.Path]map[string]storage.Entry
	budget      filesObservationBudget
}

func newFilesNativeObservations(root storage.Entry) *filesNativeObservations {
	return &filesNativeObservations{root: root, directories: make(map[storage.Path][]storage.Entry), byName: make(map[storage.Path]map[string]storage.Entry)}
}

func (o *filesNativeObservations) children(ctx context.Context, f *fileRequest, parent storage.Entry) ([]storage.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if entries, ok := o.directories[parent.Path]; ok {
		return entries, nil
	}
	entries, err := filesObserveChildren(ctx, f, parent, &o.budget)
	if err != nil {
		return nil, err
	}
	names := make(map[string]storage.Entry, len(entries))
	for _, entry := range entries {
		names[filesPathKey(f)(path.Base(string(entry.Path)))] = entry
	}
	o.directories[parent.Path] = entries
	o.byName[parent.Path] = names
	return entries, nil
}

func (o *filesNativeObservations) resolve(ctx context.Context, f *fileRequest, requested fc.Path) (storage.Entry, error) {
	if err := ctx.Err(); err != nil {
		return storage.Entry{}, err
	}
	if err := f.RequireRead(requested); err != nil {
		return storage.Entry{}, err
	}
	rootPath, err := filesEntryPath(o.root)
	if err != nil {
		return storage.Entry{}, err
	}
	if !fc.WithinScope(rootPath, requested, filesPathKey(f)) {
		return storage.Entry{}, storage.NewError("not_found", storage.Path(requested.Relative()), nil)
	}
	rootParts := strings.Split(rootPath.Relative(), "/")
	if rootPath == "/" {
		rootParts = nil
	}
	parts := strings.Split(requested.Relative(), "/")
	if requested == "/" {
		parts = nil
	}
	// Root spelling is native authority too; an index alias is not a public identity.
	for i, part := range rootParts {
		if parts[i] != part {
			return storage.Entry{}, storage.NewError("invalid_name", storage.Path(requested.Relative()), nil)
		}
	}
	current := o.root
	for _, segment := range parts[len(rootParts):] {
		if current.Kind != storage.Folder {
			return storage.Entry{}, storage.NewError("not_folder", current.Path, nil)
		}
		if _, err := o.children(ctx, f, current); err != nil {
			return storage.Entry{}, err
		}
		child, ok := o.byName[current.Path][filesPathKey(f)(segment)]
		if !ok {
			return storage.Entry{}, storage.NewError("not_found", storage.Path(requested.Relative()), nil)
		}
		if path.Base(string(child.Path)) != segment {
			return storage.Entry{}, storage.NewError("invalid_name", storage.Path(requested.Relative()), nil)
		}
		current = child
	}
	return current, nil
}

func filesUTC(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	v := t.UTC()
	return &v
}

func (s *Server) filesMetadata(ctx context.Context, f *fileRequest, entry storage.Entry, indexed *nodes.Row) (fc.Metadata, error) {
	return s.filesMetadataObserved(ctx, f, entry, indexed, nil)
}

func (s *Server) filesMetadataObserved(ctx context.Context, f *fileRequest, entry storage.Entry, indexed *nodes.Row, observations *filesNativeObservations) (fc.Metadata, error) {
	p, err := filesEntryPath(entry)
	if err != nil {
		return fc.Metadata{}, err
	}
	if err := f.RequireRead(p); err != nil {
		return fc.Metadata{}, err
	}
	if indexed == nil && s.filesIndex != nil {
		indexed, err = s.filesIndex.Live(ctx, string(entry.Path), f.Principal.User.ID)
		if err != nil {
			return fc.Metadata{}, err
		}
	}
	// An index row for another spelling/kind cannot override the native object.
	if indexed != nil && (filesPathKey(f)(indexed.Path) != filesPathKey(f)(string(entry.Path)) || indexed.IsDir != (entry.Kind == storage.Folder)) {
		indexed = nil
	}
	// P0's required timestamp fields use the UTC zero time when unknown;
	// native mtime supplies an observed updatedAt until index metadata exists.
	m := fc.Metadata{Path: p, ParentPath: fc.Path(path.Dir(string(p))), Name: path.Base(string(p)), Kind: string(entry.Kind), SourceModifiedAt: filesUTC(entry.ModifiedAt), UpdatedAt: entry.ModifiedAt.UTC()}
	if p == "/" {
		m.Name = ""
		m.ParentPath = "/"
	}
	if indexed != nil {
		if t, e := time.Parse(time.RFC3339Nano, indexed.CreatedAt); e == nil {
			m.CreatedAt = t.UTC()
		}
		if t, e := time.Parse(time.RFC3339Nano, indexed.UpdatedAt); e == nil {
			m.UpdatedAt = t.UTC()
		}
	}
	if entry.Kind == storage.File {
		size := entry.Size
		m.Size = &size
		media := entry.MediaType
		if media == "" && indexed != nil && indexed.Mime != nil {
			media = *indexed.Mime
		}
		if media == "" {
			media = "application/octet-stream"
		}
		m.MediaType = &media
		m.ContentRevision = filesContentRevision(f.ContentNamespace, entry)
		if entry.Digest != nil {
			m.Checksum = &fc.Checksum{Algorithm: entry.Digest.Algorithm, Value: base64.StdEncoding.EncodeToString(entry.Digest.Value)}
		}
	}
	// Hash the public representation plus trustworthy observed native evidence.
	// This is an observation revision, never a native conditional-write fence.
	h := sha256.New()
	representation, _ := json.Marshal(m)
	h.Write(representation)
	evidence, _ := json.Marshal(struct {
		Namespace string
		Version   []byte
	}{f.ContentNamespace, entry.Version})
	h.Write(evidence)
	if entry.Kind == storage.Folder {
		var children []storage.Entry
		if observations != nil {
			children, err = observations.children(ctx, f, entry)
		} else {
			children, err = filesChildren(ctx, f, entry)
		}
		if err != nil {
			return fc.Metadata{}, err
		}
		membership := make([]string, 0, len(children))
		for _, child := range children {
			membership = append(membership, string(child.Kind)+":"+string(child.Path))
		}
		sort.Strings(membership)
		encoded, _ := json.Marshal(membership)
		h.Write(encoded)
	}
	m.Revision = "nr_" + hex.EncodeToString(h.Sum(nil))
	return m, nil
}

func (s *Server) handleFilesMetadata(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	q, err := parseFilesReadQuery(r, f.Scope, true)
	if err != nil {
		return err
	}
	entry, err := filesStat(r.Context(), f, q.Path)
	if err != nil {
		return err
	}
	metadata, err := s.filesMetadata(r.Context(), f, entry, nil)
	if err != nil {
		return err
	}
	return writeFilesJSON(w, 200, metadata)
}

type filesSortKey struct {
	Kind       int
	Name, Path string
}

func filesKey(e storage.Entry) filesSortKey {
	kind := 1
	if e.Kind == storage.Folder {
		kind = 0
	}
	return filesSortKey{kind, norm.NFC.String(path.Base(string(e.Path))), norm.NFC.String(string(e.Path))}
}
func filesKeyLess(a, b filesSortKey) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.Path < b.Path
}

type filesCandidate struct {
	Entry        storage.Entry
	Row          *nodes.Row
	Observations *filesNativeObservations
}

func (s *Server) filesPage(w http.ResponseWriter, r *http.Request, f *fileRequest, q filesReadQuery, route string, candidates []filesCandidate) error {
	binding := s.filesCursorBinding(f, q, route)
	previous, err := s.filesDecodeCursor(q.Cursor, binding)
	if err != nil {
		return err
	}
	sort.Slice(candidates, func(i, j int) bool { return filesKeyLess(filesKey(candidates[i].Entry), filesKey(candidates[j].Entry)) })
	start := 0
	if previous != nil {
		start = sort.Search(len(candidates), func(i int) bool { return filesKeyLess(*previous, filesKey(candidates[i].Entry)) })
	}
	end := start + q.Limit
	if end > len(candidates) {
		end = len(candidates)
	}
	items := make([]fc.Metadata, 0, end-start)
	for _, candidate := range candidates[start:end] {
		metadata, err := s.filesMetadataObserved(r.Context(), f, candidate.Entry, candidate.Row, candidate.Observations)
		if err != nil {
			return err
		}
		items = append(items, metadata)
	}
	var next *string
	if end < len(candidates) {
		cursor, err := s.filesEncodeCursor(filesKey(candidates[end-1].Entry), binding)
		if err != nil {
			return err
		}
		next = &cursor
	}
	return writeFilesJSON(w, 200, fc.NewCollection(items, next))
}

func (s *Server) handleFilesList(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	q, err := parseFilesReadQuery(r, f.Scope, true)
	if err != nil {
		return err
	}
	if err := f.RequireRead(q.Path); err != nil {
		return err
	}
	if _, err := s.filesDecodeCursor(q.Cursor, s.filesCursorBinding(f, q, "list")); err != nil {
		return err
	}
	parent, err := filesStat(r.Context(), f, q.Path)
	if err != nil {
		return err
	}
	observations := newFilesNativeObservations(parent)
	children, err := observations.children(r.Context(), f, parent)
	if err != nil {
		return err
	}
	candidates := make([]filesCandidate, 0, len(children))
	for _, child := range children {
		// List is native authority already. Re-statting every child would scan the
		// same local folder repeatedly and does not provide snapshot semantics.
		candidates = append(candidates, filesCandidate{Entry: child, Observations: observations})
	}
	return s.filesPage(w, r, f, q, "list", candidates)
}

func filesHiddenIndexError(err error) bool {
	// Only a missing native path is an ordinary stale index row. Ambiguity,
	// unsupported names and concealed-native failures retain their explicit errors.
	return storage.IsCode(err, "not_found")
}

func (s *Server) filesIndexedCandidates(ctx context.Context, f *fileRequest, root storage.Entry) ([]filesCandidate, error) {
	if s.filesIndex == nil {
		return nil, fc.NewProblem(503, "temporarily_unavailable", "The file index is unavailable.")
	}
	records, err := s.filesIndex.FileReadRecords(ctx, f.Principal.User.ID, string(root.Path), filesReadBound+1)
	if err != nil {
		return nil, err
	}
	if len(records) > filesReadBound {
		return nil, fc.NewProblem(503, "resource_limit", "The indexed subtree exceeds the supported read limit.")
	}
	rootPath, err := filesEntryPath(root)
	if err != nil {
		return nil, err
	}
	candidates := make([]filesCandidate, 0, len(records))
	observations := newFilesNativeObservations(root)
	seen := make(map[string]bool)
	for i := range records {
		row := &records[i]
		p, err := fc.FromRelative(row.Path)
		if err != nil {
			continue
		}
		if !fc.WithinScope(rootPath, p, filesPathKey(f)) {
			continue
		}
		if err := f.RequireRead(p); err != nil {
			continue
		}
		entry, err := observations.resolve(ctx, f, p)
		if filesHiddenIndexError(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if row.IsDir != (entry.Kind == storage.Folder) {
			continue
		}
		key := filesPathKey(f)(string(entry.Path))
		if seen[key] {
			continue
		}
		seen[key] = true
		candidates = append(candidates, filesCandidate{Entry: entry, Row: row, Observations: observations})
	}
	return candidates, nil
}

func (s *Server) handleFilesSearch(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	q, err := parseFilesReadQuery(r, f.Scope, false)
	if err != nil {
		return err
	}
	if err := f.RequireRead(q.Path); err != nil {
		return err
	}
	if _, err := s.filesDecodeCursor(q.Cursor, s.filesCursorBinding(f, q, "search")); err != nil {
		return err
	}
	root, err := filesStat(r.Context(), f, q.Path)
	if err != nil {
		return err
	}
	candidates, err := s.filesIndexedCandidates(r.Context(), f, root)
	if err != nil {
		return err
	}
	keyword := cases.Fold().String(norm.NFC.String(q.Keyword))
	filtered := make([]filesCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		e := candidate.Entry
		if q.Kind != "" && string(e.Kind) != q.Kind {
			continue
		}
		actualPath := "/" + string(e.Path)
		if !strings.Contains(cases.Fold().String(norm.NFC.String(actualPath)), keyword) {
			continue
		}
		media := e.MediaType
		if media == "" && candidate.Row.Mime != nil {
			media = *candidate.Row.Mime
		}
		if q.MediaType != "" {
			filter := strings.ToLower(q.MediaType)
			value := strings.ToLower(media)
			if e.Kind != storage.File {
				continue
			}
			if strings.HasSuffix(filter, "/*") {
				filter = strings.TrimSuffix(filter, "*")
			}
			if strings.HasSuffix(filter, "/") {
				if !strings.HasPrefix(value, filter) {
					continue
				}
			} else if value != filter {
				continue
			}
		}
		filtered = append(filtered, candidate)
	}
	return s.filesPage(w, r, f, q, "search", filtered)
}

func (s *Server) handleFilesMe(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if err := f.RequireRead(f.Scope); err != nil {
		return err
	}
	root, err := filesStat(r.Context(), f, f.Scope)
	if err != nil {
		return err
	}
	candidates, err := s.filesIndexedCandidates(r.Context(), f, root)
	if err != nil {
		return err
	}
	var bytes, files int64
	for _, candidate := range candidates {
		if candidate.Entry.Kind == storage.File {
			if bytes > filesMaxJSONBytes-candidate.Entry.Size {
				return fc.NewProblem(503, "resource_limit", "Indexed usage exceeds the supported representation limit.")
			}
			bytes += candidate.Entry.Size
			files++
		}
	}
	kind := "session"
	if f.Principal.Token != nil {
		kind = "token"
	}
	result := map[string]any{"principal": map[string]any{"kind": kind}, "scope": map[string]any{"path": f.Scope}, "read": true, "write": false, "usage": map[string]any{"bytes": bytes, "files": files, "indexed": true}, "quota": nil, "reservations": nil}
	if f.CSRFToken != "" {
		result["csrfToken"] = f.CSRFToken
	}
	return writeFilesJSON(w, 200, result)
}

func (s *Server) handleFilesCapabilities(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	q, err := parseFilesReadQuery(r, f.Scope, false)
	if err != nil {
		return err
	}
	entry, err := filesStat(r.Context(), f, q.Path)
	if err != nil {
		return err
	}
	actual, err := filesEntryPath(entry)
	if err != nil {
		return err
	}
	d := f.Driver.Describe()
	var maxFile *int64
	if d.MaxFileBytes > 0 {
		n := d.MaxFileBytes
		if n > filesMaxJSONBytes {
			n = filesMaxJSONBytes
		}
		maxFile = &n
	}
	return writeFilesJSON(w, 200, map[string]any{
		"path": actual, "read": true, "write": false, "range": d.RangeRead, "pinnedRead": d.PinnedRead, "strongContentValidators": d.PinnedRead,
		"thumbnails": false, "uploads": false, "checksums": []string{},
		"maxFileBytes": maxFile, "maxSinglePutBytes": 0, "maxPartBytes": nil,
		"pagination":           map[string]any{"defaultLimit": filesDefaultLimit, "maxLimit": filesMaxLimit},
		"names":                map[string]any{"caseSensitive": d.Names.CaseSensitive, "maxSegmentBytes": d.Names.MaxSegmentBytes, "maxPathBytes": d.Names.MaxPathBytes, "maxDepth": d.Names.MaxDepth},
		"sessionExpirySeconds": nil, "terminalRetentionSeconds": nil, "quota": nil, "reservations": nil,
	})
}

// Cursors encrypt their keyset and HMAC-authenticate the ciphertext and complete
// query binding. They expose no principal/root references and survive process
// restart with AppSecret. Neither timestamps nor global mutation counters occur.
func (s *Server) filesCursorBinding(f *fileRequest, q filesReadQuery, route string) []byte {
	payload, _ := json.Marshal(struct{ Namespace, Principal, Scope, Route, Path, Keyword, Kind, MediaType, Sort string }{f.Namespace, filePrincipalID(f.Principal), string(f.Scope), route, string(q.Path), q.Keyword, q.Kind, q.MediaType, filesSort})
	h := sha256.Sum256(payload)
	return h[:]
}
func (s *Server) filesCursorKeys() (cipher.AEAD, []byte, error) {
	if s.cfg == nil || s.cfg.AppSecret == "" {
		return nil, nil, fc.NewProblem(503, "temporarily_unavailable", "Cursor signing is unavailable.")
	}
	derive := func(label string) []byte {
		h := hmac.New(sha256.New, []byte(s.cfg.AppSecret))
		h.Write([]byte(label))
		return h.Sum(nil)
	}
	block, err := aes.NewCipher(derive("eam-files-cursor-encryption-v1"))
	if err != nil {
		return nil, nil, err
	}
	aead, err := cipher.NewGCM(block)
	return aead, derive("eam-files-cursor-signature-v1"), err
}
func (s *Server) filesEncodeCursor(key filesSortKey, binding []byte) (string, error) {
	aead, secret, err := s.filesCursorKeys()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	payload, err := json.Marshal(key)
	if err != nil {
		return "", err
	}
	token := aead.Seal(nonce, nonce, payload, binding)
	signature := hmac.New(sha256.New, secret)
	signature.Write(binding)
	signature.Write(token)
	token = append(token, signature.Sum(nil)...)
	return base64.RawURLEncoding.EncodeToString(token), nil
}
func (s *Server) filesDecodeCursor(token string, binding []byte) (*filesSortKey, error) {
	if token == "" {
		return nil, nil
	}
	invalid := func() (*filesSortKey, error) {
		return nil, fc.NewProblem(400, "invalid_request", "The cursor is invalid for this query.")
	}
	if len(token) > 32768 {
		return invalid()
	}
	aead, secret, err := s.filesCursorKeys()
	if err != nil {
		return nil, err
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) < aead.NonceSize()+aead.Overhead()+sha256.Size {
		return invalid()
	}
	ciphertext, mac := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	signature := hmac.New(sha256.New, secret)
	signature.Write(binding)
	signature.Write(ciphertext)
	if !hmac.Equal(mac, signature.Sum(nil)) {
		return invalid()
	}
	plaintext, err := aead.Open(nil, ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():], binding)
	if err != nil {
		return invalid()
	}
	var key filesSortKey
	if err := json.Unmarshal(plaintext, &key); err != nil || (key.Kind != 0 && key.Kind != 1) || key.Name == "" || fc.ValidatePath("/"+key.Path) != nil || norm.NFC.String(key.Name) != key.Name || norm.NFC.String(key.Path) != key.Path || path.Base(key.Path) != key.Name {
		return invalid()
	}
	return &key, nil
}

var _ FileReadIndex = (*nodes.Service)(nil)
