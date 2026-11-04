package nodes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/filerevision"
	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

type contentRevisionReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// LookupContentRevision uses a saved generation only while its complete native
// evidence still matches. Out-of-band replacements automatically use a fresh
// native revision without inheriting a prior file's generation.
func (s *Service) LookupContentRevision(ctx context.Context, owner, namespace string, entry storage.Entry) (*string, error) {
	return lookupContentRevision(ctx, s.db.Pool(), owner, namespace, entry)
}
func lookupContentRevision(ctx context.Context, reader contentRevisionReader, owner, namespace string, entry storage.Entry) (*string, error) {
	fallback := filerevision.Content(namespace, entry)
	if fallback == nil {
		return nil, nil
	}
	var revision string
	err := reader.QueryRow(ctx, `select content_revision from file_content_generations where owner_id=$1::uuid and root_namespace=$2
 and (node_id=file_tree_resolve($1::uuid,$3) or (node_id is null and path=$3)) and native_fingerprint=$4`, owner, namespace, string(entry.Path), contentFingerprint(entry)).Scan(&revision)
	if err == pgx.ErrNoRows {
		return fallback, nil
	}
	if err != nil {
		return nil, err
	}
	return &revision, nil
}
func contentFingerprint(entry storage.Entry) []byte {
	h := sha256.New()
	h.Write([]byte("eam/native-content/v1\x00"))
	for _, value := range [][]byte{entry.NativeRef, entry.ContentIdentity, entry.Version} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		h.Write(length[:])
		h.Write(value)
	}
	return h.Sum(nil)
}
func saveContentRevision(ctx context.Context, tx pgx.Tx, root fileops.Root, entry storage.Entry, revision *string) error {
	if entry.Kind != storage.File {
		return nil
	}
	if revision == nil {
		_, err := tx.Exec(ctx, `delete from file_content_generations where owner_id=$1::uuid and root_namespace=$2 and path=$3`, root.OwnerID, fileRootNamespace(root), string(entry.Path))
		return err
	}
	// A moved node may retain an old path cache. Free that advisory unique key
	// without deleting its identity-bound generation before old-path recreation.
	if _, err := tx.Exec(ctx, `update file_content_generations set path=null where owner_id=$1::uuid and root_namespace=$2 and path=$3
 and node_id is not null and node_id is distinct from file_tree_resolve($1::uuid,$3)`, root.OwnerID, fileRootNamespace(root), string(entry.Path)); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `insert into file_content_generations(owner_id,root_namespace,path,node_id,native_fingerprint,content_revision)
 values($1::uuid,$2,$3,file_tree_resolve($1::uuid,$3),$4,$5)
 on conflict(owner_id,root_namespace,node_id) do update set path=excluded.path,native_fingerprint=excluded.native_fingerprint,content_revision=excluded.content_revision`, root.OwnerID, fileRootNamespace(root), string(entry.Path), contentFingerprint(entry), *revision)
	return err
}
func deleteContentRevision(ctx context.Context, tx pgx.Tx, root fileops.Root, source *storage.Entry) error {
	if source == nil || source.Kind != storage.File {
		return nil
	}
	_, err := tx.Exec(ctx, `delete from file_content_generations where owner_id=$1::uuid and root_namespace=$2 and path=$3 and native_fingerprint=$4`, root.OwnerID, fileRootNamespace(root), string(source.Path), contentFingerprint(*source))
	return err
}
func sameObservedEntry(a, b storage.Entry) bool {
	// Comparing the full representation also binds size, timestamps and optional
	// digest/identity fields in the durable final observation.
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && bytes.Equal(left, right)
}
