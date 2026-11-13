package nodes

import (
	"context"
	"strings"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/filerevision"
	"everythingabout.me/backend/internal/paths"
	"github.com/jackc/pgx/v5"
	"golang.org/x/text/unicode/norm"
)

// FileBrowseKey is the public folders-first, NFC byte-order keyset position.
type FileBrowseKey struct {
	Kind       int
	Name, Path string
	BindingID  string `json:",omitempty"`
}
type FileBrowseQuery struct {
	Path                     string
	ExpectedID               string
	List                     bool
	Keyword, Kind, MediaType string
	Limit                    int
	After                    *FileBrowseKey
	HiddenRoots              []string
}
type FileBrowseRecord struct {
	Row             Row
	Membership      []string
	ContentRevision *string
	ParentID        *string
	LastChangedAt   *time.Time
	ChangeToken     *string
}
type browseReader interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// IndexedMetadata describes the indexed observation. Native versions remain
// private mutation evidence, independent of the public metadata revision.
func IndexedMetadata(namespace string, record FileBrowseRecord) (fc.Metadata, error) {
	row := record.Row
	if row.Parent != paths.ParentOf(row.Path) || row.Name != paths.NameOf(row.Path) || (!row.IsDir && (row.Size < 0 || row.Size > 9007199254740991)) {
		return fc.Metadata{}, fc.NewProblem(503, "temporarily_unavailable", "The file index contains an invalid entry.")
	}
	p, err := fc.FromRelative(row.Path)
	if err != nil {
		return fc.Metadata{}, err
	}
	parent, err := fc.FromRelative(row.Parent)
	if err != nil {
		return fc.Metadata{}, err
	}
	created, _ := time.Parse(time.RFC3339Nano, row.CreatedAt)
	updated, _ := time.Parse(time.RFC3339Nano, row.UpdatedAt)
	m := fc.Metadata{ID: row.ID, ParentID: record.ParentID, Path: p, ParentPath: parent, Name: row.Name, Kind: "file", CreatedAt: created, UpdatedAt: updated}
	if row.SourceModifiedAt != nil {
		modified, err := time.Parse(time.RFC3339Nano, *row.SourceModifiedAt)
		if err != nil {
			return fc.Metadata{}, err
		}
		m.SourceModifiedAt = &modified
	}
	if row.IsDir {
		m.Kind = "folder"
	} else {
		size := row.Size
		m.Size = &size
		media := "application/octet-stream"
		if row.Mime != nil && *row.Mime != "" {
			media = *row.Mime
		}
		m.MediaType = &media
		m.ContentRevision = record.ContentRevision
	}
	m.Revision = filerevision.Metadata(namespace, m, nil, record.Membership)
	if record.LastChangedAt != nil {
		v := record.LastChangedAt.UTC()
		m.LastChangedAt = &v
	}
	if record.ChangeToken != nil {
		m.ChangeToken = *record.ChangeToken
	}
	return m, nil
}

// Correlated index fields share the row query's snapshot without storage calls
// or per-result database round trips.
const browseVersion = `(select name_comparison_version from file_identity_roots where owner_id=$1::uuid)`
const browseColumns = nodeColumns + `,
 case when is_dir then array['membership:'||membership_revision::text] else array[]::text[] end,
 (select content_revision from file_content_generations generation
 where generation.owner_id=nodes.owner_id and generation.root_namespace=$2 and generation.node_id=nodes.id),
 nodes.parent_id::text,
 (select last_changed_at from file_effective_change(nodes.owner_id,nodes.id)),
 (select change_token::text from file_effective_change(nodes.owner_id,nodes.id))`

// browsePathVisible is the concealment predicate for a read whose candidates it already
// fetches: one entry (metadata) or one folder's members (list). It compares the
// candidate's current path with the pending-delete roots, which is exact and costs one
// ancestry walk per candidate -- bounded by the answer, not by the drive.
const browsePathVisible = `not exists(select 1 from unnest($3::text[]) hidden(root)
 where file_identity_comparison(nodes.path,` + browseVersion + `)=file_identity_comparison(hidden.root,` + browseVersion + `) or starts_with(file_identity_comparison(nodes.path,` + browseVersion + `),file_identity_comparison(hidden.root,` + browseVersion + `)||'/'))`

// browseHiddenCTE resolves the pending-delete roots to the index rows they name (one
// file_tree_resolve per root, an index probe per path segment) and walks down the
// canonical parent link to every descendant. $1 is always the owner; roots names the
// parameter position carrying the root-relative root list, and gate is spliced between
// the seed and the walk. The browse queries take the roots at $3; usage takes them at $2
// because its next parameter is the scope path. Both call the same builder so the walk
// cannot drift between layouts.
//
// Why whole-owner reads cannot use browsePathVisible: file_entries.path is derived per
// row by file_tree_path(), a walk up the ancestry, so comparing it cannot use an index
// and pays one (in the `=`/`starts_with` pair above: two) ancestry walks for every entry
// the read scans. On a 50k-entry owner with one 99-entry pending delete, that made
// GET /api/files/me scan 49,500 rows in 1223ms and 1.49M buffer hits; the same read
// against the identity set costs 6ms and 3.2k buffers, because the delete-specific work
// is now the captured subtree (100 rows) and not the drive. The walk uses
// nodes_owner_parent_comparison_uniq(owner_id, parent_id), which already exists, so this
// adds no write cost: no index is added and no row grows.
//
// The semantics are the path predicate's, expressed in identity instead of spelling, so
// the two facts deletes.md settles keep holding. An accepted delete conceals exactly its
// captured subtree: file_tree_resolve applies the owner's name comparison and the parent
// link is the identity the unique sibling index uses, so the walked set is every entry
// whose current path is a root or lies under one, and nothing else. A path re-created
// after acceptance is a new row with a new id, so it is not in the walked set and stays
// visible. A root the index no longer resolves (the delete was accepted and its rows are
// gone) conceals nothing, which is what index absence already meant. An empty root is
// skipped: "" is the relative spelling of the root, whose subtree is the whole drive, and
// callers never pass it (the overlay filters it).
func browseHiddenCTE(roots, gate string) string {
	return `with recursive hidden(id) as (
 select resolve.id from unnest(` + roots + `::text[]) root
  cross join lateral (select file_tree_resolve($1::uuid, root) as id) resolve
 where root<>'' and resolve.id is not null` + gate + `
 union
 select child.id from nodes child join hidden on child.parent_id=hidden.id
  where child.owner_id=$1::uuid) `
}

var browseHidden = browseHiddenCTE("$3", "")

const browseVisible = `nodes.id not in (select id from hidden)`

// A list is anchored to one folder, so the identity walk would charge it for the whole
// pending subtree while its candidates are only that folder's members; it keeps the path
// predicate. A search ($5 false) walks the owner's entries and needs the set. The seed is
// gated the same way so a list builds no hidden set at all.
var browsePageHidden = browseHiddenCTE("$3", " and not $5")

const browsePageVisible = `(($5::bool and ` + browsePathVisible + `) or (not $5 and ` + browseVisible + `))`

// The reads below share the $1=owner layout. They are package-level strings so a test can
// EXPLAIN exactly what production runs, which is what keeps a regression to a per-row
// path predicate visible (see TestPendingDeleteConcealmentIsBoundedByTheDeletedSubtree).
// Metadata and the page keep the pending-delete roots at $3; usage passes them at $2 and
// (for a subfolder scope) resolves the anchor from $3, so its statements are prefixed
// with browseUsageHidden instead.
const browseMetadataQuery = `select ` + browseColumns + ` from file_entries nodes where owner_id=$1::uuid and id=file_tree_resolve($1::uuid,$4) and ` + browsePathVisible + ` limit 2`

const browsePageQuery = `select ` + browseColumns + ` from file_entries nodes
 where owner_id=$1::uuid and ` + browsePageVisible + `
 and ($15='' or ($4='' and exists(select 1 from file_identity_roots selected_root where selected_root.owner_id=$1::uuid and selected_root.id::text=$15))
 or ($4<>'' and exists(select 1 from file_entries selected where selected.owner_id=$1::uuid and selected.id::text=$15
 and file_identity_comparison(selected.path,` + browseVersion + `)=file_identity_comparison($4,` + browseVersion + `))))
 and (($5::bool and parent_id=file_tree_resolve($1::uuid,$4))
 or (not $5 and ($4='' or file_identity_comparison(path,` + browseVersion + `)=file_identity_comparison($4,` + browseVersion + `) or file_identity_comparison(path,` + browseVersion + `) like file_identity_comparison($6,` + browseVersion + `))))
 and ($7='' or lower(normalize('/'||path,NFC)) like $7)
 and ($8='' or ($8='folder' and is_dir) or ($8='file' and not is_dir))
 and ($9='' or (not is_dir and (($10::bool and starts_with(lower(coalesce(mime,'')),$9)) or (not $10 and lower(coalesce(mime,''))=$9))))
 and ($11::int is null or (case when is_dir then 0 else 1 end,normalize(name,NFC) collate "C",normalize(path,NFC) collate "C") > ($11,$12::text collate "C",$13::text collate "C"))
 order by case when is_dir then 0 else 1 end,normalize(name,NFC) collate "C",normalize(path,NFC) collate "C"
 limit $14`

// Usage takes the pending-delete roots at $2 (its handle queries have no namespace
// parameter), so both statements below are prefixed with this CTE instead of browseHidden.
var browseUsageHidden = browseHiddenCTE("$2", "")

// browseUsageQuery is the whole-owner usage read: "/" means the owner's drive, so this
// read scans every entry by definition and only the pending-delete set narrows it.
const browseUsageQuery = `select coalesce(sum(size),0),count(*) from file_entries nodes where owner_id=$1::uuid and not is_dir
 and ` + browseVisible

// browseUsageScopeQuery sums exactly one resolved subtree, by walking the canonical parent
// link from the scope anchor at $3 and carrying size/is_dir along the walk. It never reads
// the owner's other entries.
//
// Why not keep the path predicate: file_entries.path is derived per row by
// file_tree_path(), so comparing it cannot use an index and pays ancestry walks for every
// entry of the owner. On a seeded 50k-entry owner, GET /api/files/me?path=/f0000 (25
// files) scanned 50,000 rows in 1,620ms and 1,181,208 shared buffer hits; the anchored
// walk visits the 26 rows of the subtree in 0.8ms and 115 buffers. The walk uses
// nodes_pkey for the anchor and nodes_owner_parent_comparison_uniq(owner_id, parent_id)
// for the children, so no index is added and no write pays for it.
//
// The semantics are the path predicate's, expressed in identity: file_tree_resolve
// applies the owner's name comparison, so the anchor is the same row a path match would
// select, and the parent link is the identity the unique sibling index uses. A file scope
// counts that file (the anchor is included and `not is_dir` decides), a missing anchor
// counts nothing, and the pending-delete set is applied per row exactly as for the
// whole-owner read. A row whose canonical ancestry is broken is not in the tree either,
// and the cached path that let the old predicate match it was never authoritative.
const browseUsageScopeQuery = `, scope(id,size,is_dir) as (
 select nodes.id,nodes.size,nodes.is_dir from nodes
  where nodes.owner_id=$1::uuid and nodes.id=file_tree_resolve($1::uuid,$3)
 union all
 select child.id,child.size,child.is_dir from nodes child join scope on child.parent_id=scope.id
  where child.owner_id=$1::uuid)
select coalesce(sum(size),0),count(*) from scope
 where not is_dir and id not in (select id from hidden)`

type browseScanRow struct {
	pgx.Row
	extra []any
}

func (r browseScanRow) Scan(dest ...any) error { return r.Row.Scan(append(dest, r.extra...)...) }
func scanBrowseRecord(row pgx.Row) (*FileBrowseRecord, error) {
	record := &FileBrowseRecord{}
	node, err := scanNode(browseScanRow{Row: row, extra: []any{&record.Membership, &record.ContentRevision, &record.ParentID, &record.LastChangedAt, &record.ChangeToken}})
	if err != nil || node == nil {
		return nil, err
	}
	record.Row = *node
	return record, nil
}
func (s *Service) FileBrowseMetadata(ctx context.Context, owner, namespace, path string, hiddenRoots ...string) (*FileBrowseRecord, error) {
	return fileBrowseMetadata(ctx, s.db.Pool(), owner, namespace, path, hiddenRoots)
}
func fileBrowseMetadata(ctx context.Context, reader browseReader, owner, namespace, path string, hiddenRoots []string) (*FileBrowseRecord, error) {
	if path == "" {
		record := &FileBrowseRecord{Row: Row{Node: Node{IsDir: true}}}
		err := reader.QueryRow(ctx, `select id::text from file_identity_roots where owner_id=$1::uuid`, owner).Scan(&record.Row.ID)
		if err == pgx.ErrNoRows {
			err = reader.QueryRow(ctx, `select file_identity_root($1::uuid)::text`, owner).Scan(&record.Row.ID)
		}
		if err == nil {
			err = reader.QueryRow(ctx, `select array['membership:'||membership_revision::text],
 (select last_changed_at from file_effective_change(owner_id,id)),
 (select change_token::text from file_effective_change(owner_id,id))
 from file_identity_roots where owner_id=$1::uuid`, owner).Scan(&record.Membership, &record.LastChangedAt, &record.ChangeToken)
		}
		return record, err
	}
	rows, err := reader.Query(ctx, browseMetadataQuery, owner, namespace, hiddenRoots, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var record *FileBrowseRecord
	for rows.Next() {
		if record != nil {
			return nil, fc.NewProblem(409, "name_conflict", "The index contains ambiguous file names.")
		}
		record, err = scanBrowseRecord(rows)
		if err != nil {
			return nil, err
		}
	}
	return record, rows.Err()
}
func (s *Service) FileBrowsePage(ctx context.Context, owner, namespace string, q FileBrowseQuery) ([]FileBrowseRecord, error) {
	var afterKind, afterName, afterPath any
	if q.After != nil {
		afterKind, afterName, afterPath = q.After.Kind, norm.NFC.String(q.After.Name), norm.NFC.String(q.After.Path)
	}
	media := strings.ToLower(q.MediaType)
	mediaPrefix := strings.HasSuffix(media, "/*") || strings.HasSuffix(media, "/")
	media = strings.TrimSuffix(media, "*")
	rows, err := s.db.Pool().Query(ctx, browsePageHidden+browsePageQuery, owner, namespace, q.HiddenRoots, q.Path, q.List, EscapeLike(norm.NFC.String(q.Path))+"/%", keywordPattern(q.Keyword), q.Kind, media, mediaPrefix, afterKind, afterName, afterPath, q.Limit, q.ExpectedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]FileBrowseRecord, 0)
	for rows.Next() {
		record, err := scanBrowseRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, *record)
	}
	return records, rows.Err()
}
func keywordPattern(keyword string) string {
	if keyword == "" {
		return ""
	}
	return "%" + EscapeLike(strings.ToLower(norm.NFC.String(keyword))) + "%"
}
func (s *Service) FileBrowseUsage(ctx context.Context, owner, path string, hiddenRoots ...string) (bytes, files int64, err error) {
	if path == "" {
		err = s.db.Pool().QueryRow(ctx, browseUsageHidden+browseUsageQuery, owner, hiddenRoots).Scan(&bytes, &files)
		return
	}
	err = s.db.Pool().QueryRow(ctx, browseUsageHidden+browseUsageScopeQuery, owner, hiddenRoots, path).Scan(&bytes, &files)
	return
}
