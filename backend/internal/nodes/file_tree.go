package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

// CommitNativeTreeMove settles one evidence-bound atomic native folder rename.
// Only the root linkage changes; descendants and their content generations retain
// their rows, IDs, native evidence and timestamps.
func CommitNativeTreeMove(ctx context.Context, tx pgx.Tx, op fileops.Operation, native *storage.MutationResult) (json.RawMessage, error) {
	var request fileops.ActionRequest
	if json.Unmarshal(op.Intent, &request) != nil || op.Kind != "move" || request.Kind != "move" || request.Root != op.Root || len(op.Entries) != 1 {
		return nil, fileops.E("invalid_intent", nil)
	}
	effect := op.Entries[0]
	var result storage.MutationResult
	if effect.Action != "move" || effect.State != "applied" || effect.Source != request.From || effect.Target != request.To || effect.SourceEntry == nil || effect.SourceEntry.Kind != storage.Folder || effect.SourceEntry.Path != effect.Source || !bytes.Equal(effect.SourceVersion, effect.SourceEntry.Version) || len(effect.SourceVersion) == 0 || json.Unmarshal(effect.Result, &result) != nil || result.Entry == nil || result.Entry.Kind != storage.Folder || result.Entry.Path != request.To || len(result.Evidence) == 0 || !bytes.Equal(result.Evidence, effect.Evidence) {
		return nil, fileops.E("invalid_native_result", nil)
	}
	entry := *result.Entry
	if !bytes.Equal(entry.Version, effect.SourceEntry.Version) || !bytes.Equal(entry.NativeRef, effect.SourceEntry.NativeRef) || !bytes.Equal(entry.ContentIdentity, effect.SourceEntry.ContentIdentity) {
		return nil, fileops.E("invalid_native_result", nil)
	}
	if native != nil && native.Entry != nil {
		if native.Entry.Path != entry.Path || native.Entry.Kind != storage.Folder || !bytes.Equal(native.Entry.Version, entry.Version) || !bytes.Equal(native.Entry.NativeRef, entry.NativeRef) {
			return nil, fileops.E("invalid_native_result", nil)
		}
		entry = *native.Entry
	}
	var sourceID string
	if err := tx.QueryRow(ctx, `select id::text from nodes where owner_id=$1::uuid and id=file_tree_resolve($1::uuid,$2) and is_dir`, op.Root.OwnerID, string(request.From)).Scan(&sourceID); err != nil {
		return nil, fileops.E("target_changed", err)
	}
	if request.EntryID != "" && request.EntryID != sourceID {
		return nil, fileops.E("target_changed", nil)
	}
	var staleTarget bool
	if err := tx.QueryRow(ctx, `select file_tree_resolve($1::uuid,$2) is not null`, op.Root.OwnerID, string(request.To)).Scan(&staleTarget); err != nil {
		return nil, err
	}
	if staleTarget {
		if err := retireStaleDestination(ctx, tx, op.Root, request.To); err != nil {
			return nil, err
		}
	}
	_, err := tx.Exec(ctx, `update nodes set key=$3,path=$4,parent=$5,name=$6,source_modified_at=$7,updated_at=now()
 where owner_id=$1::uuid and id=$2::uuid`, op.Root.OwnerID, sourceID, actualEntryKey(op.Root, entry), string(entry.Path), paths.ParentOf(string(entry.Path)), paths.NameOf(string(entry.Path)), nativeModified(entry))
	if err != nil {
		return nil, fmt.Errorf("settle canonical native tree move: %w", err)
	}
	if _, err = tx.Exec(ctx, `update file_identity_roots set tree_revision=tree_revision+1 where owner_id=$1::uuid`, op.Root.OwnerID); err != nil {
		return nil, err
	}
	metadata, err := settledMetadata(ctx, tx, op.Root, entry, nil)
	if err != nil {
		return nil, err
	}
	return json.Marshal(metadata)
}
