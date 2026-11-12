package api

import (
	"context"
	"strings"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/storage"
	"golang.org/x/text/unicode/norm"
)

// filesDeleteOverlay hides every path whose deletion is pending from reads until its
// native purge finishes. The native tree stays authoritative — deletion is complete
// only once the objects are gone and the index settled — but the user's confirmation
// is final: reads must treat the path as deleted at once instead of exposing the
// internal cleanup that remains.
//
// This is deliberately a read view only. A create or upload destination must resolve
// against the index and the journal's guards, not against concealment: once acceptance
// has committed, the rows are gone and the delete's guard is released, so the name is
// free even while the purge still runs. Resolving such a destination through these
// roots turned that free name into 404 not_found (and hid the real 409 while the delete
// was still being admitted). Reads, ID/path selection, change feeds and the delete
// target keep the concealment.
func (s *Server) filesDeleteOverlay(ctx context.Context, owner, namespace string, driver storage.Driver) (storage.Driver, error) {
	roots, err := s.pendingDeleteRoots(ctx, owner, namespace)
	if err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return driver, nil
	}
	overlay := &pendingDeleteDriver{Driver: driver, roots: roots}
	// Only claim the reference-reader capability when the wrapped driver has it, so a
	// handler never depends on an interface the underlying storage cannot serve.
	if _, ok := driver.(storage.ReferenceReader); ok {
		return &pendingDeleteReferenceDriver{pendingDeleteDriver: overlay}, nil
	}
	return overlay, nil
}

// pendingDeleteRoots lists the root-relative paths of deletions that have been accepted
// but are not terminal yet. Pending legacy jobs (fs_jobs) and current actions
// (file_operations) both contribute: in either case the user has already confirmed the
// deletion, so every read treats the path as gone while the native purge runs.
func (s *Server) pendingDeleteRoots(ctx context.Context, owner, namespace string) ([]string, error) {
	if s.db == nil {
		return nil, nil
	}
	var roots []string
	seen := map[string]bool{}
	add := func(root string) {
		root = norm.NFC.String(root)
		if root == "" || root == "/" || seen[root] {
			return
		}
		seen[root] = true
		roots = append(roots, root)
	}
	// Accepted deletions of the retiring legacy job runner keep their own binding.
	if s.cfg != nil && s.cfg.BackendName() == "cos" {
		rows, err := s.db.Pool().Query(ctx, `select from_path from fs_jobs
			where user_id=$1::uuid and op='delete' and status<>'done' and delete_binding=$2`, owner, uploadIndexBinding(s.cfg))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var root string
			if err := rows.Scan(&root); err != nil {
				rows.Close()
				return nil, err
			}
			add(root)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	// A delete action is hidden from its own namespace only: an adopted or restored
	// native root must never inherit a stale hidden path from another generation.
	rows, err := s.db.Pool().Query(ctx, `select distinct g.native_path
		from file_operation_guards g
		join file_operations o on o.id=g.operation_id and o.owner_id=g.owner_id
		where g.owner_id=$1::uuid and o.action='delete'
		  and o.snapshot->>'State' in ('queued','running','outcome_unknown')
		  and concat_ws(':', o.snapshot->'Root'->>'OwnerID', o.snapshot->'Root'->>'ConnectionID',
		      o.snapshot->'Root'->>'Generation', o.snapshot->'Root'->>'ContentRoot',
		      o.snapshot->'Root'->>'SystemRoot')=$2`, owner, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, err
		}
		add(root)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return roots, nil
}

type pendingDeleteDriver struct {
	storage.Driver
	roots []string // normalized comparisons preserve the driver's native spelling
}

func (d *pendingDeleteDriver) hidden(p storage.Path) bool {
	value := norm.NFC.String(string(p))
	for _, root := range d.roots {
		if value == root || strings.HasPrefix(value, root+"/") {
			return true
		}
	}
	return false
}

func (d *pendingDeleteDriver) Stat(ctx context.Context, p storage.Path) (storage.Entry, error) {
	if d.hidden(p) {
		return storage.Entry{}, storage.NewError("not_found", p, nil)
	}
	entry, err := d.Driver.Stat(ctx, p)
	if err == nil && d.hidden(entry.Path) {
		return storage.Entry{}, storage.NewError("not_found", p, nil)
	}
	return entry, err
}

func (d *pendingDeleteDriver) Open(ctx context.Context, p storage.Path, opts storage.ReadOptions) (storage.Read, error) {
	if d.hidden(p) {
		return storage.Read{}, storage.NewError("not_found", p, nil)
	}
	read, err := d.Driver.Open(ctx, p, opts)
	if err == nil && d.hidden(read.Entry.Path) {
		if read.Body != nil {
			_ = read.Body.Close()
		}
		return storage.Read{}, storage.NewError("not_found", p, nil)
	}
	return read, err
}

func (d *pendingDeleteDriver) List(ctx context.Context, p storage.Path, request storage.PageRequest) (storage.Page, error) {
	if d.hidden(p) {
		return storage.Page{}, storage.NewError("not_found", p, nil)
	}
	seen := map[string]bool{request.Cursor: true}
	for count := 0; count < filesNativePageBound; count++ {
		if err := ctx.Err(); err != nil {
			return storage.Page{}, err
		}
		page, err := d.Driver.List(ctx, p, request)
		if err != nil {
			return storage.Page{}, err
		}
		visible := make([]storage.Entry, 0, len(page.Entries))
		for _, entry := range page.Entries {
			if !d.hidden(entry.Path) {
				visible = append(visible, entry)
			}
		}
		// Preserve native cursors. Consume hidden-only pages internally because
		// the public read layer rejects empty nonterminal native pages.
		if len(visible) > 0 || page.NextCursor == "" {
			page.Entries = visible
			return page, nil
		}
		if seen[page.NextCursor] {
			return storage.Page{}, storage.NewError("invalid_cursor", p, nil)
		}
		seen[page.NextCursor] = true
		request.Cursor = page.NextCursor
	}
	return storage.Page{}, storage.NewError("resource_limit", p, nil)
}

// pendingDeleteReferenceDriver keeps hiding accepted deletions on the reference-addressed
// read path. It exists as a separate type so the capability is present only when the
// wrapped driver can actually serve reference reads.
type pendingDeleteReferenceDriver struct{ *pendingDeleteDriver }

func (d *pendingDeleteReferenceDriver) StatReference(ctx context.Context, p storage.Path, ref []byte) (storage.Entry, error) {
	if d.hidden(p) {
		return storage.Entry{}, storage.NewError("not_found", p, nil)
	}
	entry, err := d.Driver.(storage.ReferenceReader).StatReference(ctx, p, ref)
	if err == nil && d.hidden(entry.Path) {
		return storage.Entry{}, storage.NewError("not_found", p, nil)
	}
	return entry, err
}

func (d *pendingDeleteReferenceDriver) OpenReference(ctx context.Context, p storage.Path, ref []byte, opts storage.ReadOptions) (storage.Read, error) {
	if d.hidden(p) {
		return storage.Read{}, storage.NewError("not_found", p, nil)
	}
	read, err := d.Driver.(storage.ReferenceReader).OpenReference(ctx, p, ref, opts)
	if err == nil && d.hidden(read.Entry.Path) {
		if read.Body != nil {
			_ = read.Body.Close()
		}
		return storage.Read{}, storage.NewError("not_found", p, nil)
	}
	return read, err
}

// filesPendingDeleteCovers reports whether p lies inside a delete whose acceptance has
// committed and whose purge is not terminal. It is the write-path trigger for the folder
// marker reassert, not a read view: the guard rows that carry the overlay's roots are
// released by the same acceptance transaction that removed the index rows, so after
// acceptance the only durable record of the still-running purge is the operation's Purge
// path. A normal mkdir pays one bounded, owner-and-namespace-scoped query and no native
// request.
//
// The path comparison happens in Go with the driver's own name rules, like every other
// selection, because the database's lower() is not the comparison the storage layer uses.
// The query is bounded; if an owner somehow has more pending purges than the bound, a mkdir
// is conservatively treated as covered (it then pays one marker reassert), so the bound can
// never turn into a missed fence.
const maxPendingPurgeRoots = 256

func (s *Server) filesPendingDeleteCovers(ctx context.Context, f *fileRequest, p fc.Path) (bool, error) {
	if s.db == nil || p == "/" {
		return false, nil
	}
	rows, err := s.db.Pool().Query(ctx, `select distinct o.snapshot->'Purge'->>'Path'
		from file_operations o
		where o.owner_id=$1::uuid and o.action='delete'
		  and o.snapshot->'Purge' is not null
		  and o.snapshot->>'State' in ('queued','running','outcome_unknown')
		  and concat_ws(':', o.snapshot->'Root'->>'OwnerID', o.snapshot->'Root'->>'ConnectionID',
		      o.snapshot->'Root'->>'Generation', o.snapshot->'Root'->>'ContentRoot',
		      o.snapshot->'Root'->>'SystemRoot')=$2
		limit $3`, f.Principal.User.ID, f.ContentNamespace, maxPendingPurgeRoots+1)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	roots := make([]string, 0, maxPendingPurgeRoots)
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return false, err
		}
		roots = append(roots, root)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(roots) > maxPendingPurgeRoots {
		return true, nil
	}
	for _, root := range roots {
		root = norm.NFC.String(root)
		if root == "" || root == "/" {
			continue
		}
		prefix, err := fc.FromRelative(root)
		if err != nil {
			continue
		}
		if fc.WithinScope(prefix, p, filesPathKey(f)) {
			return true, nil
		}
	}
	return false, nil
}
