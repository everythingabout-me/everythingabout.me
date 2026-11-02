package api

import (
	"context"
	"strings"

	"everythingabout.me/backend/internal/storage"
	"golang.org/x/text/unicode/norm"
)

// filesDeleteOverlay hides accepted legacy deletions until their native purge is
// complete. Index absence alone cannot conceal paths from authoritative reads.
func (s *Server) filesDeleteOverlay(ctx context.Context, owner string, driver storage.Driver) (storage.Driver, error) {
	if s.cfg == nil || s.cfg.BackendName() != "cos" || s.db == nil {
		return driver, nil
	}
	rows, err := s.db.Pool().Query(ctx, `select from_path from fs_jobs
		where user_id=$1::uuid and op='delete' and status<>'done' and delete_binding=$2`, owner, uploadIndexBinding(s.cfg))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roots []string
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, err
		}
		roots = append(roots, norm.NFC.String(root))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return driver, nil
	}
	return &pendingDeleteDriver{Driver: driver, roots: roots}, nil
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
