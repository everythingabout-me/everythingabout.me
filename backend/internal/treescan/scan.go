// Package treescan validates read-only native tree enumerations before index import.
// A complete enumeration is not a snapshot or evidence of writer quiescence. P1
// never prunes index rows and never performs native mutations.
package treescan

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"everythingabout.me/backend/internal/storage"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Options bounds work and memory. Zero limits select finite defaults. Negative or
// excessively large values are rejected before any driver calls.
type Options struct {
	PageSize          int
	MaxPages          int
	MaxEntries        int // includes synthesized parents
	MaxDepth          int
	MaxInventoryBytes int64 // zero defaults to 256 MiB; maximum 8 GiB
	Names             storage.NameRules
}

func (o Options) bounded() (Options, error) {
	limits := []struct {
		p        *int
		def, max int
	}{
		{&o.PageSize, 1000, 10000}, {&o.MaxPages, 100000, 1000000},
		{&o.MaxEntries, 1000000, 5000000}, {&o.MaxDepth, 128, 1024},
	}
	for _, l := range limits {
		if *l.p < 0 || *l.p > l.max {
			return o, issue("invalid_options", "", "", "scan limit is out of bounds")
		}
		if *l.p == 0 {
			*l.p = l.def
		}
	}
	if o.MaxInventoryBytes < 0 || o.MaxInventoryBytes > 8<<30 {
		return o, issue("invalid_options", "", "", "inventory byte limit is out of bounds")
	}
	if o.MaxInventoryBytes == 0 {
		o.MaxInventoryBytes = 256 << 20
	}
	if o.Names.MaxDepth < 0 || o.Names.MaxPathBytes < 0 || o.Names.MaxSegmentBytes < 0 {
		return o, issue("invalid_options", "", "", "name limit cannot be negative")
	}
	if o.Names.MaxDepth > 0 && o.Names.MaxDepth < o.MaxDepth {
		o.MaxDepth = o.Names.MaxDepth
	}
	if o.Names.MaxPathBytes == 0 || o.Names.MaxPathBytes > 65536 {
		o.Names.MaxPathBytes = 65536
	}
	if o.Names.MaxSegmentBytes == 0 || o.Names.MaxSegmentBytes > 4096 {
		o.Names.MaxSegmentBytes = 4096
	}
	return o, nil
}

// Error identifies the exact problematic spelling; Other is the conflicting path.
type Error struct {
	Code   string
	Path   storage.Path
	Other  storage.Path
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("scan %s at %q (other %q): %s", e.Code, e.Path, e.Other, e.Detail)
}
func issue(code string, p, other storage.Path, detail string) error {
	return &Error{code, p, other, detail}
}

// Record includes only observed native metadata. Nil optional values mean unknown,
// so import preserves existing metadata rather than pretending it was removed.
type Record struct {
	Entry        storage.Entry
	Implicit     bool
	ETag         *string
	StorageClass *string
}

// Result can only become complete after a successful bounded scan. Its records
// are private and all access returns copies, preventing post-validation changes.
type Result struct {
	records  []Record
	complete bool
	names    storage.NameRules
}

func (r *Result) Records() ([]Record, error) {
	if r == nil || !r.complete {
		return nil, issue("incomplete_scan", "", "", "a successful complete scan is required")
	}
	out := make([]Record, len(r.records))
	for i, v := range r.records {
		out[i] = clone(v)
	}
	return out, nil
}
func (r *Result) NameRules() storage.NameRules {
	if r == nil {
		return storage.NameRules{}
	}
	n := r.names
	n.ReservedNames = append([]string(nil), n.ReservedNames...)
	return n
}
func clone(r Record) Record {
	r.Entry.NativeRef = bytes.Clone(r.Entry.NativeRef)
	r.Entry.Version = bytes.Clone(r.Entry.Version)
	if r.Entry.Digest != nil {
		d := *r.Entry.Digest
		d.Value = bytes.Clone(d.Value)
		r.Entry.Digest = &d
	}
	if r.ETag != nil {
		s := *r.ETag
		r.ETag = &s
	}
	if r.StorageClass != nil {
		s := *r.StorageClass
		r.StorageClass = &s
	}
	return r
}

type collector struct {
	opts       Options
	records    map[storage.Path]Record
	normalized map[string]storage.Path
	folded     map[string]storage.Path
}

func newCollector(opts Options) (*collector, error) {
	o, err := opts.bounded()
	if err != nil {
		return nil, err
	}
	return &collector{o, map[storage.Path]Record{}, map[string]storage.Path{}, map[string]storage.Path{}}, nil
}
func parent(p storage.Path) storage.Path {
	s := string(p)
	i := strings.LastIndexByte(s, '/')
	if i < 0 {
		return ""
	}
	return storage.Path(s[:i])
}
func (c *collector) validate(e storage.Entry) error {
	if e.Path == "" {
		return issue("outside_root", e.Path, "", "root cannot appear as a child")
	}
	if err := storage.ValidatePath(e.Path); err != nil {
		return issue("invalid_name", e.Path, "", err.Error())
	}
	if e.Kind != storage.File && e.Kind != storage.Folder {
		return issue("invalid_kind", e.Path, "", "expected file or folder")
	}
	if e.Size < 0 || (e.Kind == storage.Folder && e.Size != 0) {
		return issue("invalid_size", e.Path, "", "files require nonnegative sizes and folders require zero size")
	}
	parts := strings.Split(string(e.Path), "/")
	if len(parts) > c.opts.MaxDepth || len(e.Path) > c.opts.Names.MaxPathBytes {
		return issue("limit_exceeded", e.Path, "", "path exceeds depth or byte limit")
	}
	for _, part := range parts {
		if len(part) > c.opts.Names.MaxSegmentBytes {
			return issue("limit_exceeded", e.Path, "", "segment exceeds byte limit")
		}
		for _, reserved := range c.opts.Names.ReservedNames {
			if part == reserved || (!c.opts.Names.CaseSensitive && cases.Fold().String(part) == cases.Fold().String(reserved)) {
				return issue("invalid_name", e.Path, "", "reserved native name")
			}
		}
	}
	return nil
}
func (c *collector) add(r Record) error {
	e := r.Entry
	if err := c.validate(e); err != nil {
		return err
	}
	if old, ok := c.records[e.Path]; ok {
		if old.Entry.Kind != e.Kind {
			return issue("type_conflict", e.Path, old.Entry.Path, "a file and folder occupy the same path")
		}
		if old.Implicit && !r.Implicit {
			c.records[e.Path] = clone(r)
			return nil
		}
		if r.Implicit {
			return nil
		}
		return issue("duplicate_path", e.Path, e.Path, "enumeration repeated an entry")
	}
	n := norm.NFC.String(string(e.Path))
	if old, ok := c.normalized[n]; ok && old != e.Path {
		return issue("normalization_conflict", e.Path, old, "NFC-equivalent native spellings")
	}
	if !c.opts.Names.CaseSensitive {
		f := cases.Fold().String(n)
		if old, ok := c.folded[f]; ok && old != e.Path {
			return issue("case_conflict", e.Path, old, "case-equivalent native spellings")
		}
		c.folded[f] = e.Path
	}
	if len(c.records) >= c.opts.MaxEntries {
		return issue("limit_exceeded", e.Path, "", "entry limit reached")
	}
	c.normalized[n] = e.Path
	c.records[e.Path] = clone(r)
	return nil
}
func (c *collector) finish() *Result {
	records := make([]Record, 0, len(c.records))
	for _, r := range c.records {
		records = append(records, r)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Entry.Path < records[j].Entry.Path })
	n := c.opts.Names
	n.ReservedNames = append([]string(nil), n.ReservedNames...)
	return &Result{records: records, complete: true, names: n}
}

// Scan walks paginated immediate children of the bound root. Driver failures,
// malformed pages, changed directory versions and exhausted budgets return no
// result. EOF on all listings proves enumeration only, not a native snapshot.
func Scan(ctx context.Context, tree storage.Tree, opts Options) (*Result, error) {
	c, err := newCollector(opts)
	if err != nil {
		return nil, err
	}
	if tree == nil {
		return nil, issue("invalid_tree", "", "", "tree is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := tree.Stat(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("stat scan root: %w", err)
	}
	if root.Path != "" || root.Kind != storage.Folder || root.Size != 0 {
		return nil, issue("outside_root", root.Path, "", "bound root must be an empty-path folder")
	}
	pending := []storage.Entry{clone(Record{Entry: root}).Entry}
	pages := 0
	for len(pending) > 0 {
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		cursor := ""
		cursors := map[string]bool{"": true}
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if pages >= c.opts.MaxPages {
				return nil, issue("limit_exceeded", dir.Path, "", "page limit reached")
			}
			pages++
			page, err := tree.List(ctx, dir.Path, storage.PageRequest{Cursor: cursor, Limit: c.opts.PageSize})
			if err != nil {
				return nil, fmt.Errorf("list %q: %w", dir.Path, err)
			}
			if len(page.Entries) > c.opts.PageSize {
				return nil, issue("invalid_page", dir.Path, "", "driver exceeded requested page size")
			}
			for _, entry := range page.Entries {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if entry.Path == "" || parent(entry.Path) != dir.Path {
					return nil, issue("outside_root", entry.Path, dir.Path, "listing returned something other than an immediate child")
				}
				if err := c.add(Record{Entry: entry}); err != nil {
					return nil, err
				}
				if entry.Kind == storage.Folder {
					pending = append(pending, clone(Record{Entry: entry}).Entry)
				}
			}
			if len(page.NextCursor) > 65536 {
				return nil, issue("limit_exceeded", dir.Path, "", "cursor exceeds 64 KiB")
			}
			if page.NextCursor == "" {
				break
			}
			if cursors[page.NextCursor] || len(page.Entries) == 0 {
				return nil, issue("nonadvancing_cursor", dir.Path, "", "cursor repeated or advanced without entries")
			}
			cursors[page.NextCursor] = true
			cursor = page.NextCursor
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		after, err := tree.Stat(ctx, dir.Path)
		if err != nil {
			return nil, fmt.Errorf("verify directory %q: %w", dir.Path, err)
		}
		if after.Path != dir.Path || after.Kind != storage.Folder || after.Size != 0 {
			return nil, issue("changed", dir.Path, "", "directory changed during scan")
		}
		if len(dir.Version) > 0 && !bytes.Equal(dir.Version, after.Version) {
			return nil, issue("changed", dir.Path, "", "directory version changed during enumeration")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.finish(), nil
}
