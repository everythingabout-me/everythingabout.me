package nodes

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

// CommitStagedFile is the index half of a proven native file publication. It is
// called inside the journal's settlement transaction, while path guards are held.
// It never touches bytes, deletes a file, or claims an index write published data.
func CommitStagedFile(ctx context.Context, tx pgx.Tx, op fileops.Operation, result *storage.MutationResult) (json.RawMessage, error) {
	var intent storage.WriteIntent
	if json.Unmarshal(op.Intent, &intent) != nil || result == nil || result.Entry == nil {
		return nil, fileops.E("invalid_native_result", nil)
	}
	entry := result.Entry
	if entry.Kind != storage.File || entry.Path != intent.Target || entry.Size != intent.Size || storage.ValidatePath(entry.Path) != nil {
		return nil, fileops.E("invalid_native_result", nil)
	}
	p := string(entry.Path)
	// Parents must have been admitted/captured; index settlement never invents a
	// native folder or runs the old EnsureParents side effect.
	parent := paths.ParentOf(p)
	if parent != "" {
		var folder bool
		err := tx.QueryRow(ctx, `select is_dir from nodes where owner_id=$1::uuid and path=$2`, op.Root.OwnerID, parent).Scan(&folder)
		if err != nil || !folder {
			return nil, fileops.E("parent_changed", err)
		}
	}
	var modified *time.Time
	if !entry.ModifiedAt.IsZero() {
		v := entry.ModifiedAt.UTC()
		modified = &v
	}
	var created, updated time.Time
	err := tx.QueryRow(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size,mime,source_modified_at)
 values($1::uuid,$2,$3,$4,$5,false,$6,nullif($7,''),$8)
 on conflict(owner_id,path) do update set key=excluded.key,parent=excluded.parent,name=excluded.name,
 size=excluded.size,mime=excluded.mime,source_modified_at=coalesce(excluded.source_modified_at,nodes.source_modified_at),updated_at=now()
 where nodes.is_dir=false
 returning created_at,updated_at`, op.Root.OwnerID, paths.KeyFor(p, op.Root.OwnerID), p, parent, paths.NameOf(p), entry.Size, entry.MediaType, modified).Scan(&created, &updated)
	if err != nil {
		return nil, fmt.Errorf("settle file index: %w", err)
	}
	apiPath, err := fc.FromRelative(p)
	if err != nil {
		return nil, err
	}
	apiParent, err := fc.FromRelative(parent)
	if err != nil {
		return nil, err
	}
	media := entry.MediaType
	if media == "" {
		media = "application/octet-stream"
	}
	size := entry.Size
	metadata := fc.Metadata{Path: apiPath, ParentPath: apiParent, Name: paths.NameOf(p), Kind: "file", Size: &size, MediaType: &media, SourceModifiedAt: modified, CreatedAt: created.UTC(), UpdatedAt: updated.UTC()}
	if entry.Digest != nil {
		metadata.Checksum = &fc.Checksum{Algorithm: entry.Digest.Algorithm, Value: base64.StdEncoding.EncodeToString(entry.Digest.Value)}
	}
	// This settled response is durable even if the path later moves or disappears.
	// P4's HTTP representation/revision integration will bind mutation preconditions;
	// a private index result is not itself a native conditional or strong ETag.
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(encoded)
	metadata.Revision = "nr_" + hex.EncodeToString(hash[:])
	return json.Marshal(metadata)
}
