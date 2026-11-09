package api

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storage"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	filesDefaultLimit          = 200
	filesMaxLimit              = 1000
	filesNativePageBound       = 1000
	filesMaxJSONBytes    int64 = 9007199254740991
	filesSort                  = "kind-folder-first,nfc-name-bytes,nfc-path-bytes:v1"
)

// FileReadIndex serves browsing from the owner-scoped database index. Native
// storage is accessed by content, mutations and explicit reconciliation only.
// All paths are root-relative; implementations filter scopes before pagination.
type FileReadIndex interface {
	FileBrowseMetadata(context.Context, string, string, string, ...string) (*nodes.FileBrowseRecord, error)
	FileBrowsePage(context.Context, string, string, nodes.FileBrowseQuery) ([]nodes.FileBrowseRecord, error)
	FileBrowseUsage(context.Context, string, string, ...string) (int64, int64, error)
}

type filesReadQuery struct {
	SelectorID                       string
	ResolvedID                       string
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

// Accepted legacy deletions remain hidden while their native purge completes.
// The index applies these exclusions before keyset pagination and membership hashes.
func filesHiddenRoots(f *fileRequest) []string {
	if overlay, ok := f.Driver.(*pendingDeleteDriver); ok {
		return overlay.roots
	}
	return nil
}

// filesIndexedRecord returns the owner-scoped index row for one path, including the
// native key that content reads address directly.
func (s *Server) filesIndexedRecord(ctx context.Context, f *fileRequest, p fc.Path) (*nodes.FileBrowseRecord, error) {
	if err := f.RequireRead(p); err != nil {
		return nil, err
	}
	if s.filesIndex == nil {
		return nil, fc.NewProblem(503, "temporarily_unavailable", "The file index is unavailable.")
	}
	record, err := s.filesIndex.FileBrowseMetadata(ctx, f.Principal.User.ID, f.ContentNamespace, p.Relative(), filesHiddenRoots(f)...)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, fc.NewProblem(404, "not_found", "The indexed path was not found.")
	}
	return record, nil
}

func (s *Server) filesIndexedMetadata(ctx context.Context, f *fileRequest, p fc.Path) (fc.Metadata, error) {
	metadata, _, err := s.filesIndexedContent(ctx, f, p)
	return metadata, err
}

// filesIndexedContent returns the indexed metadata together with the native key that a
// reference read addresses, so a caller that must verify the object pays one index read.
func (s *Server) filesIndexedContent(ctx context.Context, f *fileRequest, p fc.Path) (fc.Metadata, string, error) {
	record, err := s.filesIndexedRecord(ctx, f, p)
	if err != nil {
		return fc.Metadata{}, "", err
	}
	metadata, err := nodes.IndexedMetadata(f.ContentNamespace, *record)
	if err != nil {
		return fc.Metadata{}, "", err
	}
	if err := f.RequireRead(metadata.Path); err != nil {
		return fc.Metadata{}, "", err
	}
	if !filesEqualPath(f, metadata.Path, p) {
		return fc.Metadata{}, "", fc.NewProblem(404, "not_found", "The indexed path was not found.")
	}
	return filesRedactParent(f, metadata), record.Row.Key, nil
}

func (s *Server) handleFilesMetadata(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	q, err := s.filesReadSelectorQuery(r, f, true)
	if err != nil {
		return err
	}
	metadata, err := s.filesIndexedMetadata(r.Context(), f, q.Path)
	if err != nil {
		return err
	}
	if q.SelectorID != "" && metadata.ID != q.SelectorID {
		return fc.NewProblem(404, "not_found", "The selected file changed location.")
	}
	return writeFilesJSON(w, 200, metadata)
}

type filesSortKey = nodes.FileBrowseKey

func filesKeyLess(a, b filesSortKey) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.Path < b.Path
}

func (s *Server) filesBrowsePage(w http.ResponseWriter, r *http.Request, f *fileRequest, q filesReadQuery, route string) error {
	if err := f.RequireRead(q.Path); err != nil {
		return err
	}
	binding := s.filesCursorBinding(f, q, route)
	previous, err := s.filesDecodeCursor(q.Cursor, binding)
	if err != nil {
		return err
	}
	if previous != nil && previous.BindingID != "" && previous.BindingID != q.ResolvedID {
		return fc.NewProblem(409, "cursor_stale", "The selected folder changed after pagination began.")
	}
	parent, err := s.filesIndexedMetadata(r.Context(), f, q.Path)
	if err != nil {
		return err
	}
	if q.SelectorID != "" && parent.ID != q.SelectorID {
		return fc.NewProblem(404, "not_found", "The selected folder changed location.")
	}
	if route == "list" && parent.Kind != "folder" {
		return fc.NewProblem(409, "not_a_folder", "The selected path is not a folder.")
	}
	query := nodes.FileBrowseQuery{ExpectedID: parent.ID, Path: q.Path.Relative(), List: route == "list", Limit: q.Limit + 1, After: previous, HiddenRoots: filesHiddenRoots(f)}
	if route == "search" {
		query.Keyword, query.Kind, query.MediaType = q.Keyword, q.Kind, q.MediaType
	}
	records, err := s.filesIndex.FileBrowsePage(r.Context(), f.Principal.User.ID, f.ContentNamespace, query)
	if err != nil {
		return err
	}
	hasMore := len(records) > q.Limit
	if hasMore {
		records = records[:q.Limit]
	}
	items := make([]fc.Metadata, 0, len(records))
	for _, record := range records {
		metadata, err := nodes.IndexedMetadata(f.ContentNamespace, record)
		if err != nil {
			return err
		}
		if err := f.RequireRead(metadata.Path); err != nil {
			return err
		}
		if !fc.WithinScope(q.Path, metadata.Path, filesPathKey(f)) || (route == "list" && (!filesEqualPath(f, metadata.ParentPath, q.Path) || filesEqualPath(f, metadata.Path, q.Path))) {
			return fc.NewProblem(500, "internal_error", "The index returned a path outside the requested directory.")
		}
		items = append(items, filesRedactParent(f, metadata))
	}
	var next *string
	if hasMore {
		last := items[len(items)-1]
		kind := 1
		if last.Kind == "folder" {
			kind = 0
		}
		key := filesSortKey{BindingID: q.ResolvedID, Kind: kind, Name: norm.NFC.String(last.Name), Path: norm.NFC.String(last.Path.Relative())}
		cursor, err := s.filesEncodeCursor(key, binding)
		if err != nil {
			return err
		}
		next = &cursor
	}
	return writeFilesJSON(w, 200, fc.NewCollection(items, next))
}

func (s *Server) handleFilesList(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	q, err := s.filesReadSelectorQuery(r, f, true)
	if err != nil {
		return err
	}
	return s.filesBrowsePage(w, r, f, q, "list")
}

func (s *Server) handleFilesSearch(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	q, err := s.filesReadSelectorQuery(r, f, false)
	if err != nil {
		return err
	}
	return s.filesBrowsePage(w, r, f, q, "search")
}

func (s *Server) handleFilesMe(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if _, err := s.filesIndexedMetadata(r.Context(), f, f.Scope); err != nil {
		return err
	}
	bytes, files, err := s.filesIndex.FileBrowseUsage(r.Context(), f.Principal.User.ID, f.Scope.Relative(), filesHiddenRoots(f)...)
	if err != nil {
		return err
	}
	if bytes < 0 || bytes > filesMaxJSONBytes || files < 0 || files > filesMaxJSONBytes {
		return fc.NewProblem(503, "resource_limit", "Indexed usage exceeds the supported representation limit.")
	}
	kind := "session"
	if f.Principal.Token != nil {
		kind = "token"
	}
	ready, accounting, err := s.filesWriteReadiness(r.Context(), f)
	if err != nil {
		return err
	}
	quota, reservations := filesUsageFields(accounting, f.Scope == "/")
	result := map[string]any{"principal": map[string]any{"kind": kind}, "scope": map[string]any{"path": f.Scope}, "read": true, "write": ready, "usage": map[string]any{"bytes": bytes, "files": files, "indexed": true}, "quota": quota, "reservations": reservations}
	result["changes"] = f.Principal.Token == nil || f.Principal.Token.CanChanges
	result["subscribe"] = f.Principal.Token == nil || (f.Principal.Token.CanChanges && f.Principal.Token.CanSubscribe)
	if f.CSRFToken != "" {
		result["csrfToken"] = f.CSRFToken
	}
	return writeFilesJSON(w, 200, result)
}

func (s *Server) handleFilesCapabilities(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	q, err := s.filesReadSelectorQuery(r, f, false)
	if err != nil {
		return err
	}
	metadata, err := s.filesIndexedMetadata(r.Context(), f, q.Path)
	if err != nil {
		return err
	}
	if q.SelectorID != "" && metadata.ID != q.SelectorID {
		return fc.NewProblem(404, "not_found", "The selected file changed location.")
	}
	actual := metadata.Path
	d := f.Driver.Describe()
	var maxFile *int64
	if d.MaxFileBytes > 0 {
		n := d.MaxFileBytes
		if n > filesMaxJSONBytes {
			n = filesMaxJSONBytes
		}
		maxFile = &n
	}
	ready, usage, err := s.filesWriteReadiness(r.Context(), f)
	if err != nil {
		return err
	}
	class := s.filesTransferClass()
	downloadsReady := class == storage.TransferRelay
	if class == storage.TransferPresignedParts {
		_, downloadsReady = filesUnderlyingDriver(f.Driver).(storage.ReadGranter)
	}
	var singlePut int64
	checksums := []string{}
	if ready && class == storage.TransferRelay {
		singlePut = s.cfg.Storage.MaxSinglePutBytes
		if singlePut == 0 {
			singlePut = 32 << 20
		}
		checksums = []string{"sha-256"}
	}
	var retention any
	if ready {
		retention = 86400
	}
	uploadsReady := ready
	if class == storage.TransferPresignedParts {
		if _, ok := filesUnderlyingDriver(f.Driver).(storage.PlannedUploadWriter); !ok {
			uploadsReady = false
		}
	} else if numbered, ok := f.Driver.(storage.NumberedUploadWriter); ok && numbered.UploadMaxFileBytes() == 0 {
		uploadsReady = false
	}
	var partBytes, idleExpiry, maxLifetime any
	if uploadsReady {
		partBytes, idleExpiry, maxLifetime = filesUploadPartSize, 86400, 604800
		if native, ok := filesUnderlyingDriver(f.Driver).(fileops.NativeActions); ok {
			n := filesUploadMaxSize(native)
			maxFile = &n
		}
	}
	quota, reservations := filesUsageFields(usage, f.Scope == "/" && actual == "/")
	return writeFilesJSON(w, 200, map[string]any{
		"identity":       map[string]any{"id": true, "path": true, "uniquePaths": true},
		"changeTracking": map[string]any{"available": true, "read": f.Principal.Token == nil || f.Principal.Token.CanChanges, "subscribe": f.Principal.Token == nil || (f.Principal.Token.CanChanges && f.Principal.Token.CanSubscribe), "delivery": []string{"sse"}, "retentionSeconds": int64(90 * 24 * 60 * 60), "subscriptionLifetimeSeconds": int64(7 * 24 * 60 * 60), "snapshotLifetimeSeconds": int64(24 * 60 * 60), "snapshotMaxEntries": 100000, "snapshotMaxBytes": int64(64 << 20), "observation": "indexed", "externalChanges": "explicit-reconcile"},
		"path":           actual, "read": true, "write": ready, "range": d.RangeRead, "pinnedRead": d.PinnedRead, "strongContentValidators": d.PinnedRead,
		"transferClass": s.filesTransferClassLabel(), "transferKind": s.filesTransferKind(), "transferClasses": d.TransferClasses,
		"atomicCreate": d.AtomicCreate, "downloads": downloadsReady,
		"thumbnails": false, "uploads": uploadsReady, "checksums": checksums, "sourceModifiedAt": ready && s.cfg.BackendName() == "local",
		"maxFileBytes": maxFile, "maxSinglePutBytes": singlePut, "maxPartBytes": partBytes,
		"pagination":           map[string]any{"defaultLimit": filesDefaultLimit, "maxLimit": filesMaxLimit},
		"names":                map[string]any{"caseSensitive": d.Names.CaseSensitive, "maxSegmentBytes": d.Names.MaxSegmentBytes, "maxPathBytes": d.Names.MaxPathBytes, "maxDepth": d.Names.MaxDepth},
		"sessionExpirySeconds": idleExpiry, "sessionMaxLifetimeSeconds": maxLifetime, "maxUploadParts": filesUploadMaxParts, "terminalRetentionSeconds": retention, "quota": quota, "reservations": reservations,
	})
}

// Cursors encrypt their keyset and HMAC-authenticate the ciphertext and complete
// query binding. They expose no principal/root references and survive process
// restart with AppSecret. Neither timestamps nor global mutation counters occur.
func (s *Server) filesCursorBinding(f *fileRequest, q filesReadQuery, route string) []byte {
	boundPath := string(q.Path)
	if q.SelectorID != "" {
		boundPath = ""
	}
	payload, _ := json.Marshal(struct{ Namespace, Principal, Scope, Route, Path, Keyword, Kind, MediaType, Sort, SelectorID string }{f.Namespace, filePrincipalID(f.Principal), string(f.Scope), route, boundPath, q.Keyword, q.Kind, q.MediaType, filesSort, q.SelectorID})
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
