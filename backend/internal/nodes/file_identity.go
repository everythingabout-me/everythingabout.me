package nodes

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// FileResolveID resolves an opaque public UUID within the authenticated owner.
// Roots have an empty relative path. Invalid, deleted and foreign IDs are absent.
func (s *Service) FileResolveID(ctx context.Context, owner, id string) (string, bool, error) {
	return resolveFileID(ctx, s.db.Pool(), owner, id)
}

// resolveFileID is FileResolveID on a caller-chosen connection, so a transaction can ask
// the same question about the state it is about to act on.
func resolveFileID(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, owner, id string) (string, bool, error) {
	var parsed pgtype.UUID
	if err := parsed.Scan(id); err != nil || !parsed.Valid {
		return "", false, nil
	}
	var path string
	err := q.QueryRow(ctx, `select path from file_entries where owner_id=$1::uuid and id=$2::uuid
 union all select '' from file_identity_roots where owner_id=$1::uuid and id=$2::uuid`, owner, parsed).Scan(&path)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return path, err == nil, err
}

// FileIdentity returns the stored identity and parent identity without inspecting
// native storage. The root has no parent. A missing path returns empty IDs.
func (s *Service) FileIdentity(ctx context.Context, owner, path string) (id, parentID string, err error) {
	if path == "" {
		err = s.db.Pool().QueryRow(ctx, `select file_identity_root($1::uuid)::text`, owner).Scan(&id)
		return
	}
	err = s.db.Pool().QueryRow(ctx, `select id::text,parent_id::text from nodes
 where owner_id=$1::uuid and id=file_tree_resolve($1::uuid,$2)`, owner, path).Scan(&id, &parentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return
}

// FileIdentityBusy reports a durable path guard covering the selected location.
// Guards remain authoritative through worker expiry and native outcome recovery;
// their existence, rather than a worker lease, determines whether reads can race.
//
// An upload session takes its guard when it commits, so a pending replacement does not
// make the file it will replace unreadable. A guard row under a still-open session is
// the leftover of one opened before that rule and is ignored here exactly as the
// mutation conflict scan ignores it (fileops guardHolder).
func (s *Service) FileIdentityBusy(ctx context.Context, owner, path string) (string, error) {
	var operationID string
	err := s.db.Pool().QueryRow(ctx, `select guard.operation_id::text from file_operation_guards guard
 join file_operations operation on operation.id=guard.operation_id and operation.owner_id=guard.owner_id
 cross join lateral (select file_identity_comparison(guard.native_path,
 case when operation.root_case_sensitive then 'nfc-sensitive-v1' else 'nfc-fold-v1' end) as guarded,
 file_identity_comparison($2,
 case when operation.root_case_sensitive then 'nfc-sensitive-v1' else 'nfc-fold-v1' end) as selected) comparison
 where guard.owner_id=$1::uuid and (comparison.guarded=comparison.selected
 or (guard.subtree and (comparison.guarded='' or starts_with(comparison.selected,comparison.guarded||'/'))))
 and not exists(select 1 from file_uploads upload where upload.operation_id=guard.operation_id
 and upload.owner_id=guard.owner_id and upload.state='open')
 order by guard.operation_id,guard.ordinal limit 1`, owner, path).Scan(&operationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return operationID, err
}

const configureNameComparisonSQL = `select file_identity_configure_rules($1::uuid,$2)`

func nameComparisonVersion(caseSensitive bool) string {
	if caseSensitive {
		return "nfc-sensitive-v1"
	}
	return "nfc-fold-v1"
}

// FileConfigureNameComparison opts a root into its explicit native name rules.
// Existing collisions reject the entire change. Reading/browsing never guesses
// native rules, and unconfigured owners always use NFC case-sensitive comparison.
func (s *Service) FileConfigureNameComparison(ctx context.Context, owner string, caseSensitive bool) error {
	_, err := s.db.Pool().Exec(ctx, configureNameComparisonSQL, owner, nameComparisonVersion(caseSensitive))
	return err
}
