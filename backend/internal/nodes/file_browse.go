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
}
type FileBrowseQuery struct {
	Path                     string
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
	m := fc.Metadata{Path: p, ParentPath: parent, Name: row.Name, Kind: "file", CreatedAt: created, UpdatedAt: updated}
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
	return m, nil
}

// Correlated index fields share the row query's snapshot without storage calls
// or per-result database round trips.
const browseColumns = nodeColumns + `,
 case when is_dir then coalesce((select array_agg((case when child.is_dir then 'folder:' else 'file:' end)||child.path)
 from nodes child where child.owner_id=nodes.owner_id and child.parent=nodes.path
 and not exists(select 1 from unnest($3::text[]) hidden(root)
 where normalize(child.path,NFC)=normalize(hidden.root,NFC) or starts_with(normalize(child.path,NFC),normalize(hidden.root,NFC)||'/'))),array[]::text[]) else array[]::text[] end,
 (select content_revision from file_content_generations generation
 where generation.owner_id=nodes.owner_id and generation.root_namespace=$2 and generation.path=nodes.path)`
const browseVisible = `not exists(select 1 from unnest($3::text[]) hidden(root)
 where normalize(nodes.path,NFC)=normalize(hidden.root,NFC) or starts_with(normalize(nodes.path,NFC),normalize(hidden.root,NFC)||'/'))`

type browseScanRow struct {
	pgx.Row
	extra []any
}

func (r browseScanRow) Scan(dest ...any) error { return r.Row.Scan(append(dest, r.extra...)...) }
func scanBrowseRecord(row pgx.Row) (*FileBrowseRecord, error) {
	record := &FileBrowseRecord{}
	node, err := scanNode(browseScanRow{Row: row, extra: []any{&record.Membership, &record.ContentRevision}})
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
		err := reader.QueryRow(ctx, `select coalesce(array_agg((case when is_dir then 'folder:' else 'file:' end)||path),array[]::text[])
  from nodes where owner_id=$1::uuid and parent='' and `+strings.ReplaceAll(browseVisible, "$3", "$2"), owner, hiddenRoots).Scan(&record.Membership)
		return record, err
	}
	rows, err := reader.Query(ctx, `select `+browseColumns+` from nodes where owner_id=$1::uuid and normalize(path,NFC)=normalize($4,NFC) and `+browseVisible+` limit 2`, owner, namespace, hiddenRoots, path)
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
	rows, err := s.db.Pool().Query(ctx, `select `+browseColumns+` from nodes
 where owner_id=$1::uuid and `+browseVisible+`
 and (($5::bool and normalize(parent,NFC)=normalize($4,NFC))
 or (not $5 and ($4='' or normalize(path,NFC)=normalize($4,NFC) or normalize(path,NFC) like $6)))
 and ($7='' or lower(normalize('/'||path,NFC)) like $7)
 and ($8='' or ($8='folder' and is_dir) or ($8='file' and not is_dir))
 and ($9='' or (not is_dir and (($10::bool and starts_with(lower(coalesce(mime,'')),$9)) or (not $10 and lower(coalesce(mime,''))=$9))))
 and ($11::int is null or (case when is_dir then 0 else 1 end,normalize(name,NFC) collate "C",normalize(path,NFC) collate "C") > ($11,$12::text collate "C",$13::text collate "C"))
 order by case when is_dir then 0 else 1 end,normalize(name,NFC) collate "C",normalize(path,NFC) collate "C"
 limit $14`, owner, namespace, q.HiddenRoots, q.Path, q.List, EscapeLike(norm.NFC.String(q.Path))+"/%", keywordPattern(q.Keyword), q.Kind, media, mediaPrefix, afterKind, afterName, afterPath, q.Limit)
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
	err = s.db.Pool().QueryRow(ctx, `select coalesce(sum(size),0),count(*) from nodes where owner_id=$1::uuid and not is_dir
 and ($2='' or normalize(path,NFC)=normalize($2,NFC) or normalize(path,NFC) like $4)
 and `+browseVisible, owner, path, hiddenRoots, EscapeLike(norm.NFC.String(path))+"/%").Scan(&bytes, &files)
	return
}
