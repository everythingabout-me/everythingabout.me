package treescan

import "everythingabout.me/backend/internal/storage"

// CheckIndex rejects collisions with retained index rows before import. P1 keeps
// index-only paths, so their spelling and kind must also remain unambiguous.
func (r *Result) CheckIndex(entries []storage.Entry) error {
	records, err := r.Records()
	if err != nil {
		return err
	}
	c, err := newCollector(Options{Names: r.NameRules(), MaxDepth: 1024, MaxEntries: 5000000})
	if err != nil {
		return err
	}
	for _, record := range records {
		if err := c.add(record); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		if entry.Path == "" && entry.Kind == storage.Folder {
			continue
		}
		if old, ok := c.records[entry.Path]; ok {
			if old.Entry.Kind != entry.Kind {
				return issue("type_conflict", entry.Path, old.Entry.Path, "native and retained index kinds differ")
			}
		} else if err := c.add(Record{Entry: entry}); err != nil {
			return err
		}
		for p := parent(entry.Path); p != ""; p = parent(p) {
			if err := c.add(Record{Entry: storage.Entry{Path: p, Kind: storage.Folder}, Implicit: true}); err != nil {
				return err
			}
		}
	}
	return nil
}
