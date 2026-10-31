package treescan

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"everythingabout.me/backend/internal/storage"
)

// Inventory reads a scoped, unescaped-key CSV with an explicit header. Key and
// metadata cells are never trimmed or URL-decoded. Supported headers are key,
// size, lastmodifieddate, etag, storageclass (common underscore aliases work).
// Unknown columns are allowed, but duplicate aliases are rejected. A complete
// file proves only that its records were read; inventory claims never allow prune.
// prefix must be the exact owner content prefix, including its final slash.
func Inventory(ctx context.Context, input io.Reader, prefix string, opts Options) (*Result, error) {
	c, err := newCollector(opts)
	if err != nil {
		return nil, err
	}
	if input == nil {
		return nil, issue("invalid_inventory", "", "", "inventory reader is required")
	}
	if prefix == "" || !strings.HasSuffix(prefix, "/") || strings.TrimSuffix(prefix, "/") == "" || storage.ValidatePath(storage.Path(strings.TrimSuffix(prefix, "/"))) != nil {
		return nil, issue("invalid_binding", "", "", "inventory requires an explicit prefix ending in slash")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limited := &io.LimitedReader{R: contextReader{ctx: ctx, reader: input}, N: c.opts.MaxInventoryBytes + 1}
	reader := csv.NewReader(limited)
	header, err := reader.Read()
	if limited.N == 0 {
		return nil, issue("limit_exceeded", "", "", "inventory byte limit reached")
	}
	if err != nil {
		return nil, fmt.Errorf("inventory header: %w", err)
	}
	columns := map[string]int{}
	for i, h := range header {
		name := strings.ToLower(h)
		switch name {
		case "objectkey", "object_key":
			name = "key"
		case "last_modified", "lastmodified", "last_modified_date":
			name = "lastmodifieddate"
		case "storage_class":
			name = "storageclass"
		}
		if _, exists := columns[name]; exists {
			return nil, issue("invalid_inventory", "", "", "duplicate or ambiguous header: "+name)
		}
		columns[name] = i
	}
	for _, required := range []string{"key", "size"} {
		if _, ok := columns[required]; !ok {
			return nil, issue("invalid_inventory", "", "", "explicit key and size header columns are required")
		}
	}
	cell := func(row []string, name string) string {
		if i, ok := columns[name]; ok {
			return row[i]
		}
		return ""
	}
	seenKeys := map[string]bool{}
	for line := 2; ; line++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row, err := reader.Read()
		if limited.N == 0 {
			return nil, issue("limit_exceeded", "", "", "inventory byte limit reached")
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("inventory row %d: %w", line, err)
		}
		key := cell(row, "key")
		if !strings.HasPrefix(key, prefix) {
			return nil, issue("outside_root", storage.Path(key), "", "inventory key is outside the selected owner's prefix")
		}
		if seenKeys[key] {
			return nil, issue("duplicate_path", storage.Path(key), "", "inventory repeated a key")
		}
		seenKeys[key] = true
		// Bound even repeated root markers and rows that do not produce index entries.
		if len(seenKeys) > c.opts.MaxEntries {
			return nil, issue("limit_exceeded", storage.Path(key), "", "inventory row limit reached")
		}
		size, err := strconv.ParseInt(cell(row, "size"), 10, 64)
		if err != nil || size < 0 {
			return nil, issue("invalid_size", storage.Path(key), "", "inventory size must be a nonnegative integer")
		}
		var modified time.Time
		if value := cell(row, "lastmodifieddate"); value != "" {
			modified, err = inventoryTime(value)
			if err != nil {
				return nil, issue("invalid_time", storage.Path(key), "", err.Error())
			}
		}
		relative := strings.TrimPrefix(key, prefix)
		kind := storage.File
		if strings.HasSuffix(relative, "/") {
			relative = strings.TrimSuffix(relative, "/")
			kind = storage.Folder
		}
		if relative == "" {
			if key != prefix {
				return nil, issue("invalid_name", storage.Path(key), "", "only the exact prefix can be a root marker")
			}
			if size != 0 {
				return nil, issue("invalid_size", "", "", "root marker must be empty")
			}
			continue
		}
		record := Record{Entry: storage.Entry{Path: storage.Path(relative), Kind: kind, Size: size, ModifiedAt: modified}}
		if v := cell(row, "etag"); v != "" {
			record.ETag = &v
		}
		if v := cell(row, "storageclass"); v != "" {
			record.StorageClass = &v
		}
		if err := c.add(record); err != nil {
			return nil, err
		}
		for p := parent(record.Entry.Path); p != ""; p = parent(p) {
			if err := c.add(Record{Entry: storage.Entry{Path: p, Kind: storage.Folder}, Implicit: true}); err != nil {
				return nil, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.finish(), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	return n, err
}

func inventoryTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid inventory timestamp %q", value)
}
