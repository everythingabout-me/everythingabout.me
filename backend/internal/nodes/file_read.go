package nodes

import (
	"context"

	"golang.org/x/text/unicode/norm"
)

// FileReadRecords returns a bounded, owner-scoped index candidate set. Prefix is a
// native root-relative path, not a SQL pattern. NFC comparisons admit equivalent
// native spellings without changing the spelling stored in the index. The caller
// requests one extra record and fails explicitly if its resource bound is exceeded.
// Native storage must still verify existence, kind and visible spelling.
func (s *Service) FileReadRecords(ctx context.Context, owner, prefix string, limit int) ([]Row, error) {
	rows, err := s.db.Pool().Query(ctx, `select `+nodeColumns+`
 from nodes
 where owner_id = $1::uuid
   and ($2 = '' or normalize(path, NFC) = normalize($2, NFC)
        or normalize(path, NFC) like $3)
 order by path collate "C"
 limit $4`, owner, prefix, EscapeLike(norm.NFC.String(prefix))+"/%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]Row, 0)
	for rows.Next() {
		row, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, *row)
	}
	return records, rows.Err()
}
