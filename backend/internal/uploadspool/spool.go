// Package uploadspool stores private, durable upload chunks. Database receipts remain
// the caller's responsibility. None of these files are committed native content.
package uploadspool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"everythingabout.me/backend/internal/storage"
)

const MaxParts = 10000
const MaxConcurrentReceives = 4

var (
	ErrBusy      = errors.New("upload spool is busy")
	ErrDiscarded = errors.New("upload spool session was discarded")
	ErrIntegrity = errors.New("upload spool integrity failure")
	ErrConflict  = errors.New("upload spool part identity conflict")
	ErrInvalid   = errors.New("invalid upload spool input")
)

// Part is private receipt data, including a bound opaque Handle. Persist all fields
// atomically in accept; never expose Handle as a public/native file locator.
type Part struct {
	Number int
	Size   int64
	SHA256 string
	Handle string
}

type rejected struct{ error }

func (e rejected) Unwrap() error { return e.error }

// Reject marks a definitive database rejection: no receipt for the proposed Part
// was committed. Ordinary errors may mean COMMIT succeeded but its reply was lost;
// their bytes are retained. Never wrap an uncertain database outcome with Reject.
func Reject(err error) error {
	if err == nil {
		err = ErrConflict
	}
	return rejected{err}
}

// Store owns a private namespace below an existing owner-controlled base directory
// without group/other write permissions. The namespace binds owner, connection,
// generation and both configured native roots.
// Callers reserve retained bytes plus four simultaneous temporary part bodies.
type Store struct {
	root     *os.Root
	binding  string
	lifetime context.Context
	cancel   context.CancelFunc
	once     sync.Once
	closeErr error
}

func New(base string, binding storage.Config) (*Store, error) {
	if err := storage.ValidateBinding(binding); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(base) {
		return nil, ErrInvalid
	}
	parent, err := openBase(base)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	ok, err := localFilesystem(parent)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, storage.NewError("unsupported", "", nil)
	}
	binding.Credentials = nil
	encoded, err := json.Marshal(binding)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	identity := hex.EncodeToString(sum[:])
	root, err := childDirectory(parent, "uploadspool-v1-"+identity, true)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Store, error) { root.Close(); return nil, err }
	if err := immutable(root, "binding.json", encoded); err != nil {
		return fail(err)
	}
	if err := syncDirectory(parent); err != nil {
		return fail(err)
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &Store{root: root, binding: identity, lifetime: lifetime, cancel: cancel}, nil
}

// Close cancels active streams and closes this Store's root handle. Other Store
// instances and durable receipts are unaffected. Close is idempotent.
func (s *Store) Close() error {
	s.once.Do(func() { s.cancel(); s.closeErr = s.root.Close() })
	return s.closeErr
}

func (s *Store) operation(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if s.lifetime.Err() != nil {
		return nil, nil, os.ErrClosed
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.lifetime, cancel)
	return ctx, func() { stop(); cancel() }, nil
}

func sessionKey(id string) (string, error) {
	if id == "" || len(id) > 1024 || strings.ContainsRune(id, 0) {
		return "", ErrInvalid
	}
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:]), nil
}
func (s *Store) session(id string) (*os.Root, string, error) {
	key, err := sessionKey(id)
	if err != nil {
		return nil, "", err
	}
	root, err := childDirectory(s.root, "s-"+key, true)
	if err != nil {
		return nil, "", err
	}
	if err := immutable(root, "session.json", []byte(key)); err != nil {
		root.Close()
		return nil, "", err
	}
	return root, key, nil
}
func (s *Store) part(key string, number int, size int64, digest string) Part {
	return Part{number, size, digest, "v1:" + s.binding + ":" + key + ":" + partName(number, size, digest)}
}
func partName(number int, size int64, digest string) string {
	return fmt.Sprintf("p-%05d-%d-%s", number, size, digest)
}
func recordName(number int) string { return fmt.Sprintf("part-%05d.json", number) }
func validPart(p Part) bool {
	if p.Number < 1 || p.Number > MaxParts || p.Size < 0 || len(p.SHA256) != 64 {
		return false
	}
	b, err := hex.DecodeString(p.SHA256)
	return err == nil && len(b) == 32 && strings.ToLower(p.SHA256) == p.SHA256
}
func tombstone(root *os.Root) error {
	_, err := root.Lstat("discarded")
	if err == nil {
		return ErrDiscarded
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Receive always reads and hashes the supplied body, including digestless replays.
// It fsyncs the bytes and their immutable identity before calling accept. accept
// runs under a per-session callback gate and must atomically recheck session state
// and receipt identity in the database. Returning success requires the exact Part.
// A differing retained candidate is a conflict even after an uncertain accept.
// Cancellation closes body when it implements io.ReadCloser; a plain Reader must
// itself provide a way to unblock Read. Busy admission never reads the body.
func (s *Store) Receive(ctx context.Context, sessionID string, number int, expectedSize int64, digest *storage.Digest, body io.Reader, accept func(Part) (Part, error)) (Part, error) {
	if number < 1 || number > MaxParts || expectedSize < 0 || expectedSize == math.MaxInt64 || body == nil || accept == nil {
		return Part{}, ErrInvalid
	}
	if digest != nil && (digest.Algorithm != "sha-256" || len(digest.Value) != sha256.Size) {
		return Part{}, ErrInvalid
	}
	ctx, done, err := s.operation(ctx)
	if err != nil {
		return Part{}, err
	}
	defer done()
	if closer, ok := body.(io.ReadCloser); ok {
		stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stop()
	}
	root, key, err := s.session(sessionID)
	if err != nil {
		return Part{}, err
	}
	defer root.Close()
	if err := tombstone(root); err != nil {
		return Part{}, err
	}
	sessionLock, err := lockFile(ctx, root, "session.lock", false, false)
	if err != nil {
		return Part{}, err
	}
	defer sessionLock()
	if err := tombstone(root); err != nil {
		return Part{}, err
	}
	var release func()
	slot := 0
	for ; slot < MaxConcurrentReceives; slot++ {
		release, err = lockFile(ctx, root, fmt.Sprintf("slot-%d.lock", slot), true, false)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrBusy) {
			return Part{}, err
		}
	}
	if err != nil {
		return Part{}, err
	}
	defer release()
	attempt, attemptID, err := newAttempt(root, slot)
	if err != nil {
		return Part{}, err
	}
	defer func() { attempt.Close(); _ = cleanupAttempt(root, attemptID) }()
	file, err := createFile(attempt, "data")
	if err != nil {
		return Part{}, err
	}
	defer file.Close()
	if err := syncDirectory(attempt); err != nil {
		return Part{}, err
	}
	h := sha256.New()
	count, err := io.CopyBuffer(io.MultiWriter(file, h), io.LimitReader(&contextReader{ctx, body}, expectedSize), make([]byte, 64*1024))
	if ctx.Err() != nil {
		return Part{}, ctx.Err()
	}
	if err != nil {
		return Part{}, err
	}
	if count != expectedSize {
		return Part{}, storage.NewError("size_mismatch", "", ErrIntegrity)
	}
	var extra [1]byte
	n, readErr := io.ReadFull(&contextReader{ctx, body}, extra[:])
	if ctx.Err() != nil {
		return Part{}, ctx.Err()
	}
	if n != 0 {
		return Part{}, storage.NewError("size_mismatch", "", ErrIntegrity)
	}
	if readErr != io.EOF {
		return Part{}, readErr
	}
	hashHex := hex.EncodeToString(h.Sum(nil))
	if digest != nil && !equalDigest(digest.Value, h.Sum(nil)) {
		return Part{}, storage.NewError("digest_mismatch", "", ErrIntegrity)
	}
	if err := file.Sync(); err != nil {
		return Part{}, err
	}
	if err := file.Close(); err != nil {
		return Part{}, err
	}
	gate, err := lockFile(ctx, root, "gate.lock", true, true)
	if err != nil {
		return Part{}, err
	}
	defer gate()
	if err := ctx.Err(); err != nil {
		return Part{}, err
	}
	if err := tombstone(root); err != nil {
		return Part{}, err
	}
	proposed := s.part(key, number, count, hashHex)
	record := recordName(number)
	old, err := readPart(root, record)
	created := false
	if errors.Is(err, os.ErrNotExist) {
		data, _ := json.Marshal(proposed)
		if err := immutable(root, record, data); err != nil {
			return Part{}, err
		}
		created = true
	} else if err != nil {
		return Part{}, err
	} else if old != proposed {
		return Part{}, ErrConflict
	}
	canonical := partName(number, count, hashHex)
	if err := linkFile(attempt, "data", root, canonical); err != nil && !errors.Is(err, os.ErrExist) {
		return Part{}, err
	}
	if err := syncDirectory(root); err != nil {
		return Part{}, err
	}
	if err := verifyFile(ctx, root, canonical, proposed); err != nil {
		return Part{}, err
	}
	if err := ctx.Err(); err != nil {
		return Part{}, err
	}
	accepted, err := accept(proposed)
	if err != nil {
		var rejection rejected
		if created && errors.As(err, &rejection) {
			cleanupErr := removeRegular(root, canonical)
			if cleanupErr == nil {
				cleanupErr = syncDirectory(root)
			}
			if cleanupErr == nil {
				cleanupErr = removeRegular(root, record)
			}
			cleanupErr = errors.Join(cleanupErr, syncDirectory(root))
			return Part{}, errors.Join(err, cleanupErr)
		}
		return Part{}, err
	}
	if accepted != proposed {
		return Part{}, ErrIntegrity
	}
	return accepted, nil
}

func equalDigest(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var x byte
	for i := range a {
		x |= a[i] ^ b[i]
	}
	return x == 0
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func newAttempt(root *os.Root, slot int) (*os.Root, string, error) {
	// A persistent slot flock fences every use of this deterministic attempt name.
	// Restart reclaims the old slot's recorded bytes before allocating new bytes.
	id := fmt.Sprintf("a-%032x", slot)
	if err := cleanupAttempt(root, id); err != nil {
		return nil, "", err
	}
	if err := root.Mkdir(id, 0700); err != nil {
		return nil, "", err
	}
	attempt, err := childDirectory(root, id, false)
	if err != nil {
		return nil, "", err
	}
	if err := immutable(attempt, "identity", []byte(id)); err != nil {
		attempt.Close()
		return nil, "", err
	}
	if err := syncDirectory(root); err != nil {
		attempt.Close()
		return nil, "", err
	}
	return attempt, id, nil
}
func readPart(root *os.Root, name string) (Part, error) {
	data, err := readSmall(root, name, 2048)
	if err != nil {
		return Part{}, err
	}
	var p Part
	if json.Unmarshal(data, &p) != nil || !validPart(p) {
		return Part{}, ErrIntegrity
	}
	canonical, _ := json.Marshal(p)
	if string(canonical) != string(data) {
		return Part{}, ErrIntegrity
	}
	return p, nil
}
func verifyFile(ctx context.Context, root *os.Root, name string, p Part) error {
	file, err := readFile(root, name)
	if err != nil {
		return errors.Join(ErrIntegrity, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != p.Size {
		return ErrIntegrity
	}
	h := sha256.New()
	n, err := io.CopyBuffer(h, &contextReader{ctx, file}, make([]byte, 64*1024))
	if err != nil {
		return err
	}
	if n != p.Size || hex.EncodeToString(h.Sum(nil)) != p.SHA256 {
		return ErrIntegrity
	}
	return nil
}

// Open returns ordered concatenated bytes from at most MaxParts receipts. Parts
// must be contiguous 1..N; an empty list represents an empty upload. It holds the
// session's shared lock until EOF, Close or cancellation. Each part is checked
// against its durable identity and hashed as read. Consume to EOF and honor all
// read errors before publishing; partial bytes are never evidence of integrity.
func (s *Store) Open(ctx context.Context, sessionID string, parts []Part) (io.ReadCloser, error) {
	if len(parts) > MaxParts {
		return nil, ErrInvalid
	}
	ctx, done, err := s.operation(ctx)
	if err != nil {
		return nil, err
	}
	root, key, err := s.session(sessionID)
	if err != nil {
		done()
		return nil, err
	}
	fail := func(err error) (io.ReadCloser, error) { root.Close(); done(); return nil, err }
	for i, p := range parts {
		if !validPart(p) || p.Number != i+1 || p != s.part(key, p.Number, p.Size, p.SHA256) {
			return fail(ErrInvalid)
		}
	}
	unlock, err := lockFile(ctx, root, "session.lock", false, false)
	if err != nil {
		return fail(err)
	}
	if err := tombstone(root); err != nil {
		unlock()
		return fail(err)
	}
	stream := &assembled{ctx: ctx, root: root, parts: append([]Part(nil), parts...), release: func() { unlock(); root.Close(); done() }}
	stream.mu.Lock()
	stream.stop = context.AfterFunc(ctx, func() { _ = stream.Close() })
	stream.mu.Unlock()
	return stream, nil
}

type assembled struct {
	mu          sync.Mutex
	ctx         context.Context
	root        *os.Root
	parts       []Part
	at          int
	file        *os.File
	hash        hash.Hash
	count       int64
	closed      bool
	terminalErr error
	release     func()
	stop        func() bool
}

func (r *assembled) closeLocked() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.terminalErr = r.ctx.Err()
	var err error
	if r.file != nil {
		err = r.file.Close()
		r.file = nil
	}
	if r.stop != nil {
		r.stop()
	}
	r.release()
	return err
}
func (r *assembled) Close() error { r.mu.Lock(); defer r.mu.Unlock(); return r.closeLocked() }
func (r *assembled) Read(buf []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		if r.terminalErr != nil {
			return 0, r.terminalErr
		}
		return 0, io.EOF
	}
	if err := r.ctx.Err(); err != nil {
		r.closeLocked()
		return 0, err
	}
	if len(buf) == 0 {
		return 0, nil
	}
	for {
		if r.at == len(r.parts) {
			r.closeLocked()
			return 0, io.EOF
		}
		part := r.parts[r.at]
		if r.file == nil {
			saved, err := readPart(r.root, recordName(part.Number))
			if err != nil || saved != part {
				r.closeLocked()
				return 0, errors.Join(ErrIntegrity, err)
			}
			file, err := readFile(r.root, partName(part.Number, part.Size, part.SHA256))
			if err != nil {
				r.closeLocked()
				return 0, errors.Join(ErrIntegrity, err)
			}
			info, err := file.Stat()
			if err != nil || info.Size() != part.Size {
				file.Close()
				r.closeLocked()
				return 0, errors.Join(ErrIntegrity, err)
			}
			r.file = file
			r.hash = sha256.New()
			r.count = 0
		}
		n, err := r.file.Read(buf)
		r.hash.Write(buf[:n])
		r.count += int64(n)
		if err == io.EOF {
			closeErr := r.file.Close()
			r.file = nil
			if r.count != part.Size || hex.EncodeToString(r.hash.Sum(nil)) != part.SHA256 {
				r.closeLocked()
				return n, ErrIntegrity
			}
			if closeErr != nil {
				r.closeLocked()
				return n, closeErr
			}
			r.at++
			if n > 0 {
				return n, nil
			}
			continue
		}
		if err != nil {
			r.closeLocked()
		}
		return n, err
	}
}

// Discard first durably tombstones the session, then tries exclusive cleanup.
// ErrBusy retains physical liability: retry after active receives/readers close.
// Locks and tombstone are permanent, preventing split locks and late resurrection.
// Only valid recorded attempt/part identities are cleaned; unknown data is left.
func (s *Store) Discard(ctx context.Context, sessionID string) error {
	ctx, done, err := s.operation(ctx)
	if err != nil {
		return err
	}
	defer done()
	root, key, err := s.session(sessionID)
	if err != nil {
		return err
	}
	defer root.Close()
	gate, err := lockFile(ctx, root, "gate.lock", true, false)
	if err != nil {
		return err
	}
	defer gate()
	if err := immutable(root, "discarded", []byte("discarded-v1")); err != nil {
		return err
	}
	exclusive, err := lockFile(ctx, root, "session.lock", true, false)
	if err != nil {
		return err
	}
	defer exclusive()
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	var result error
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		entries, readErr := dir.ReadDir(256)
		if readErr != nil && readErr != io.EOF {
			return errors.Join(result, readErr)
		}
		for _, entry := range entries {
			name := entry.Name()
			if validAttemptName(name) {
				result = errors.Join(result, cleanupAttempt(root, name))
				continue
			}
			if strings.HasPrefix(name, "part-") && strings.HasSuffix(name, ".json") {
				p, err := readPart(root, name)
				if err != nil {
					result = errors.Join(result, err)
					continue
				}
				if name != recordName(p.Number) || p != s.part(key, p.Number, p.Size, p.SHA256) {
					result = errors.Join(result, ErrIntegrity)
					continue
				}
				canonical := partName(p.Number, p.Size, p.SHA256)
				if err := removeRegular(root, canonical); err != nil {
					result = errors.Join(result, err)
					continue
				}
				if err := syncDirectory(root); err != nil {
					result = errors.Join(result, err)
					continue
				}
				result = errors.Join(result, removeRegular(root, name))
			}
		}
		if readErr == io.EOF {
			break
		}
	}
	return errors.Join(result, syncDirectory(root))
}
func validAttemptName(name string) bool {
	if !strings.HasPrefix(name, "a-") || len(name) != 34 {
		return false
	}
	_, err := hex.DecodeString(name[2:])
	return err == nil && strings.ToLower(name) == name
}
func cleanupAttempt(root *os.Root, id string) error {
	if !validAttemptName(id) {
		return ErrInvalid
	}
	attempt, err := childDirectory(root, id, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer attempt.Close()
	data, err := readSmall(attempt, "identity", 64)
	if err != nil || string(data) != id {
		return errors.Join(ErrIntegrity, err)
	}
	if err := removeRegular(attempt, "data"); err != nil {
		return err
	}
	if err := syncDirectory(attempt); err != nil {
		return err
	}
	dir, err := attempt.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := dir.ReadDir(3)
	dir.Close()
	if readErr != nil && readErr != io.EOF {
		return readErr
	}
	if len(entries) != 1 || entries[0].Name() != "identity" {
		return ErrIntegrity
	}
	if err := removeRegular(attempt, "identity"); err != nil {
		return err
	}
	if err := syncDirectory(attempt); err != nil {
		return err
	}
	if err := root.Remove(id); err != nil {
		return err
	}
	return syncDirectory(root)
}
