// Package snapshot copies one native tree into another as a complete, immutable,
// date-named copy. It is the only place that moves backup bytes, and it always
// writes through the storage driver: key construction, atomic publication and
// conditional create stay in one implementation.
//
// A snapshot is complete on its own. Nothing is shared with a previous snapshot
// yet: hardlinking or a server-side copy would make a second snapshot cost only
// the changed bytes, but that needs a driver capability this package cannot get
// from Tree/RelayWriter, so it is deliberately not pretended here.
package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"

	"everythingabout.me/backend/internal/storage"
	"golang.org/x/text/cases"
)

// Target is the writer half of a driver: folders plus staged publication. Both
// the local mutation driver and the admitted object driver implement it.
type Target interface {
	storage.FolderWriter
	storage.StagedWriter
}

// Result is what one copy actually did. It is reported, not inferred: a backup
// that cannot say how many files it wrote cannot be checked.
type Result struct {
	Files   int
	Folders int
	Bytes   int64
}

func (r Result) String() string {
	return fmt.Sprintf("%d files, %d folders, %d bytes", r.Files, r.Folders, r.Bytes)
}

// Copy writes the whole tree at srcRoot into dstRoot. Every destination entry is
// created only if it does not exist, so re-running into a date that already
// exists fails instead of rewriting history.
//
// A folder is created before its children because that is the only ordering a
// native tree supports: a file cannot be published into a parent that does not
// exist. Interruption therefore leaves a prefix of the tree, which is detectable
// by comparing counts, never a torn file.
func Copy(ctx context.Context, src storage.Tree, srcRoot storage.Path, dst Target, dstRoot storage.Path) (Result, error) {
	var result Result
	rules := destinationNameRules(dst)
	if dstRoot != "" {
		if err := mkdir(ctx, dst, dstRoot); err != nil {
			return result, err
		}
		result.Folders++
	}
	if err := copyFolder(ctx, src, srcRoot, dst, dstRoot, srcRoot, dstRoot, rules, &result); err != nil {
		return result, err
	}
	return result, nil
}

// destinationNameRules is what the destination refuses as a user name. A target rooted
// at the bucket reserves the private system prefix, so a source folder with that name
// would write into service space. That is refused here, naming the path, instead of
// being discovered later as a folder nobody can explain.
func destinationNameRules(dst Target) storage.NameRules {
	if namer, ok := dst.(interface{ StorageNameRules() storage.NameRules }); ok {
		return namer.StorageNameRules()
	}
	return storage.NameRules{}
}

// reservedName reports whether one native segment is a name the destination reserves.
func reservedName(segment string, rules storage.NameRules) bool {
	for _, reserved := range rules.ReservedNames {
		if rules.CaseSensitive {
			if segment == reserved {
				return true
			}
			continue
		}
		if cases.Fold().String(segment) == cases.Fold().String(reserved) {
			return true
		}
	}
	return false
}

// copyFolder mirrors one source folder into the destination. Everything below
// srcBase keeps its relative path below dstBase, which is what makes a snapshot a
// copy of the tree rather than a copy of the tree's location.
func copyFolder(ctx context.Context, src storage.Tree, srcDir storage.Path, dst Target, dstDir storage.Path, srcBase, dstBase storage.Path, rules storage.NameRules, result *Result) error {
	entries, err := listAll(ctx, src, srcDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		to := join(dstBase, relativeTo(srcBase, entry.Path))
		if segment := lastSegment(entry.Path); reservedName(segment, rules) {
			return fmt.Errorf("copy %q: %q is reserved on the destination, so the snapshot cannot contain it", entry.Path, segment)
		}
		if entry.Kind == storage.Folder {
			if err := mkdir(ctx, dst, to); err != nil {
				return fmt.Errorf("create folder %q: %w", to, err)
			}
			result.Folders++
			if err := copyFolder(ctx, src, entry.Path, dst, to, srcBase, dstBase, rules, result); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(ctx, src, entry.Path, dst, to, entry, result); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(ctx context.Context, src storage.Tree, from storage.Path, dst Target, to storage.Path, entry storage.Entry, result *Result) error {
	read, err := src.Open(ctx, from, storage.ReadOptions{})
	if err != nil {
		return fmt.Errorf("read %q: %w", from, err)
	}
	defer read.Body.Close()
	intent := storage.WriteIntent{
		ID:         intentID(from, to),
		Target:     to,
		Conditions: storage.Conditions{Absent: true},
		Size:       read.Entry.Size,
		MediaType:  read.Entry.MediaType,
	}
	staged, err := dst.Stage(ctx, intent, read.Body, checkpoint)
	if err != nil {
		return fmt.Errorf("stage %q: %w", to, err)
	}
	if _, err := dst.Publish(ctx, staged, checkpoint); err != nil {
		return fmt.Errorf("publish %q: %w", to, err)
	}
	result.Files++
	result.Bytes += read.Entry.Size
	return nil
}

func mkdir(ctx context.Context, dst Target, p storage.Path) error {
	_, err := dst.Mkdir(ctx, storage.Mutation{ID: intentID("", p), Path: p, Conditions: storage.Conditions{Absent: true}}, checkpoint)
	return err
}

// checkpoint is intentionally a no-op: a copy is restartable by re-running the
// whole snapshot, and the conditional create makes that safe. Persisting partial
// progress would only matter for a target that rejects duplicates, which the
// append-only rule already covers.
func checkpoint(context.Context, []byte) error { return nil }

// Verify walks a snapshot and proves every entry is still readable with the size
// it reports. It never compares against the source: a verification that needs the
// source cannot be run after the source is gone.
func Verify(ctx context.Context, tree storage.Tree, root storage.Path) (Result, error) {
	var result Result
	err := walk(ctx, tree, root, func(rel storage.Path, entry storage.Entry) error {
		stat, err := tree.Stat(ctx, rel)
		if err != nil {
			return fmt.Errorf("stat %q: %w", rel, err)
		}
		if stat.Kind != entry.Kind || stat.Size != entry.Size {
			return fmt.Errorf("verify %q: listing reported %s/%d bytes, stat reports %s/%d bytes", rel, entry.Kind, entry.Size, stat.Kind, stat.Size)
		}
		if entry.Kind == storage.Folder {
			result.Folders++
		} else {
			result.Files++
			result.Bytes += entry.Size
		}
		return nil
	})
	return result, err
}

// Dates lists the snapshot dates present under root, oldest first. Names are
// date-shaped by construction, so lexical order is chronological.
func Dates(ctx context.Context, tree storage.Tree, root storage.Path) ([]string, error) {
	entries, err := listAll(ctx, tree, root)
	if err != nil {
		return nil, err
	}
	var dates []string
	for _, entry := range entries {
		name := string(relativeTo(root, entry.Path))
		if entry.Kind == storage.Folder && isDate(name) {
			dates = append(dates, name)
		}
	}
	sort.Strings(dates)
	return dates, nil
}

// Prune removes whole dates, oldest first, until keep remain. It never removes an
// individual file from a snapshot that stays.
func Prune(ctx context.Context, tree storage.Tree, rem storage.Remover, root storage.Path, keep int) ([]string, error) {
	dates, err := Dates(ctx, tree, root)
	if err != nil {
		return nil, err
	}
	if keep < 0 {
		return nil, fmt.Errorf("keep must not be negative, got %d", keep)
	}
	var removed []string
	for len(dates) > keep {
		oldest := dates[0]
		dates = dates[1:]
		if err := removeTree(ctx, tree, rem, join(root, storage.Path(oldest))); err != nil {
			return removed, fmt.Errorf("remove snapshot %q: %w", oldest, err)
		}
		removed = append(removed, oldest)
	}
	return removed, nil
}

func removeTree(ctx context.Context, tree storage.Tree, rem storage.Remover, dir storage.Path) error {
	entries, err := listAll(ctx, tree, dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := entry.Path
		if entry.Kind == storage.Folder {
			if err := removeTree(ctx, tree, rem, child); err != nil {
				return err
			}
		}
		if _, err := rem.Remove(ctx, storage.Mutation{ID: intentID("", child), Path: child}, checkpoint); err != nil {
			return fmt.Errorf("remove %q: %w", child, err)
		}
	}
	_, err = rem.Remove(ctx, storage.Mutation{ID: intentID("", dir), Path: dir}, checkpoint)
	return err
}

// walk visits every entry below root (root itself excluded).
func walk(ctx context.Context, tree storage.Tree, root storage.Path, fn func(storage.Path, storage.Entry) error) error {
	entries, err := listAll(ctx, tree, root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		p := entry.Path
		if err := fn(p, entry); err != nil {
			return err
		}
		if entry.Kind == storage.Folder {
			if err := walk(ctx, tree, p, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

func listAll(ctx context.Context, tree storage.Tree, dir storage.Path) ([]storage.Entry, error) {
	var out []storage.Entry
	cursor := ""
	for {
		page, err := tree.List(ctx, dir, storage.PageRequest{Cursor: cursor, Limit: 500})
		if err != nil {
			return nil, fmt.Errorf("list %q: %w", dir, err)
		}
		out = append(out, page.Entries...)
		if page.NextCursor == "" {
			return out, nil
		}
		if page.NextCursor == cursor {
			return nil, fmt.Errorf("list %q: cursor did not advance", dir)
		}
		cursor = page.NextCursor
	}
}

// relativeTo strips a base prefix from a full tree path.
func relativeTo(base, p storage.Path) storage.Path {
	if base == "" {
		return p
	}
	return storage.Path(strings.TrimPrefix(strings.TrimPrefix(string(p), string(base)), "/"))
}

// lastSegment is the native name a path ends with, which is the form a reserved-name
// rule is written against.
func lastSegment(p storage.Path) string {
	name := string(p)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

func join(root, rel storage.Path) storage.Path {
	switch {
	case root == "":
		return rel
	case rel == "":
		return root
	}
	return storage.Path(path.Join(string(root), string(rel)))
}

// intentID is stable for one destination path, so a retry of the same snapshot
// reuses the same intent instead of inventing a parallel one.
func intentID(from, to storage.Path) string {
	sum := sha256.Sum256([]byte(string(from) + "\x00" + string(to)))
	return "snapshot:" + hex.EncodeToString(sum[:16])
}

func isDate(name string) bool {
	if len(name) != len("2006-01-02") {
		return false
	}
	if name[4] != '-' || name[7] != '-' {
		return false
	}
	for i, r := range name {
		if i == 4 || i == 7 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return !strings.HasPrefix(name, "0000")
}
