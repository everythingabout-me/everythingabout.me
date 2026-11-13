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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"

	"everythingabout.me/backend/internal/namecompare"
	"everythingabout.me/backend/internal/storage"
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
	run, err := newRunID()
	if err != nil {
		return result, err
	}
	rules := destinationNameRules(dst)
	if dstRoot != "" {
		if err := mkdir(ctx, dst, dstRoot, run); err != nil {
			return result, err
		}
		result.Folders++
	}
	if err := copyFolder(ctx, src, srcRoot, dst, dstRoot, srcRoot, dstRoot, rules, run, &result); err != nil {
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
	key := namecompare.Key(rules.CaseSensitive)
	for _, reserved := range rules.ReservedNames {
		if key(segment) == key(reserved) {
			return true
		}
	}
	return false
}

// copyFolder mirrors one source folder into the destination. Everything below
// srcBase keeps its relative path below dstBase, which is what makes a snapshot a
// copy of the tree rather than a copy of the tree's location.
func copyFolder(ctx context.Context, src storage.Tree, srcDir storage.Path, dst Target, dstDir storage.Path, srcBase, dstBase storage.Path, rules storage.NameRules, run string, result *Result) error {
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
			if err := mkdir(ctx, dst, to, run); err != nil {
				return fmt.Errorf("create folder %q: %w", to, err)
			}
			result.Folders++
			if err := copyFolder(ctx, src, entry.Path, dst, to, srcBase, dstBase, rules, run, result); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(ctx, src, entry.Path, dst, to, entry, run, result); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(ctx context.Context, src storage.Tree, from storage.Path, dst Target, to storage.Path, entry storage.Entry, run string, result *Result) error {
	read, err := src.Open(ctx, from, storage.ReadOptions{})
	if err != nil {
		return fmt.Errorf("read %q: %w", from, err)
	}
	defer read.Body.Close()
	// The staging bound is the work. A copy stages one entry at a time, so the largest
	// entry it admits is the bound the destination needs; a fixed interactive-request
	// policy instead fails the copy at its first larger file, after part of the tree has
	// been written. The snapshot declares each entry's size before handing it over.
	if bound, ok := dst.(storage.StagingBound); ok {
		if err := bound.AdmitStagedBytes(read.Entry.Size); err != nil {
			return fmt.Errorf("stage %q (%d bytes): %w", to, read.Entry.Size, err)
		}
	}
	intent := storage.WriteIntent{
		ID:         intentID(run, from, to),
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

func mkdir(ctx context.Context, dst Target, p storage.Path, run string) error {
	_, err := dst.Mkdir(ctx, storage.Mutation{ID: intentID(run, "", p), Path: p, Conditions: storage.Conditions{Absent: true}}, checkpoint)
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
	if len(dates) <= keep {
		return nil, nil
	}
	run, err := newRunID()
	if err != nil {
		return nil, err
	}
	var removed []string
	for len(dates) > keep {
		oldest := dates[0]
		dates = dates[1:]
		// The date folder is removed last, so the removal must name the revision observed
		// before its children went. Removing children does not change a folder's native
		// identity, but re-observing it after the walk would be a second observation of
		// state the delete is supposed to be conditional on.
		dir, err := tree.Stat(ctx, join(root, storage.Path(oldest)))
		if err != nil {
			return removed, fmt.Errorf("remove snapshot %q: observe it before deleting: %w", oldest, err)
		}
		if dir.Kind != storage.Folder {
			return removed, fmt.Errorf("remove snapshot %q: it is a %s, not a folder", oldest, dir.Kind)
		}
		if err := removeTree(ctx, tree, rem, dir, run); err != nil {
			return removed, fmt.Errorf("remove snapshot %q: %w", oldest, err)
		}
		removed = append(removed, oldest)
	}
	return removed, nil
}

// removeTree deletes one folder after its children, deepest first. Every call names
// the native state it removes: the revision the listing reported, or a single
// observation when the listing carried none.
func removeTree(ctx context.Context, tree storage.Tree, rem storage.Remover, dir storage.Entry, run string) error {
	entries, err := listAll(ctx, tree, dir.Path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Kind == storage.Folder {
			if err := removeTree(ctx, tree, rem, entry, run); err != nil {
				return err
			}
		}
		if err := removeEntry(ctx, tree, rem, entry, run); err != nil {
			return err
		}
	}
	return removeEntry(ctx, tree, rem, dir, run)
}

// removeEntry deletes one entry with the native state it was observed in.
func removeEntry(ctx context.Context, tree storage.Tree, rem storage.Remover, entry storage.Entry, run string) error {
	revision := entry.Revision
	if len(revision) == 0 {
		// A listing may omit a revision (an object listing has no per-object version, and
		// a driver without listing evidence answers each child itself). One observation is
		// the state this delete must name; guessing here is what made a prune issue a
		// revision-less Remove that settled without deleting anything.
		observed, err := tree.Stat(ctx, entry.Path)
		if err != nil {
			return fmt.Errorf("remove %q: observe its revision: %w", entry.Path, err)
		}
		if observed.Kind != entry.Kind {
			return fmt.Errorf("remove %q: listing reported a %s, stat reports a %s", entry.Path, entry.Kind, observed.Kind)
		}
		revision = observed.Revision
	}
	mutation := storage.Mutation{Path: entry.Path}
	switch {
	case len(revision) > 0:
		mutation.Conditions.ExpectedRevision = revision
		mutation.ID = removalID(run, entry.Path, revision)
	case entry.Kind == storage.Folder:
		// A folder with no revision has no native marker object: its provider makes the
		// folder from its children, so removing them removed it. That is expressed, not
		// inferred by leaving the revision empty, so a driver without marker-less folders
		// refuses it instead of reporting a delete it never performed.
		mutation.Conditions.ImplicitFolder = true
		mutation.ID = removalID(run, entry.Path, nil)
	default:
		return fmt.Errorf("remove %q: the driver reported no revision for a file, so a conditional delete cannot name it", entry.Path)
	}
	if _, err := rem.Remove(ctx, mutation, checkpoint); err != nil {
		return fmt.Errorf("remove %q: %w", entry.Path, err)
	}
	return nil
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

// newRunID names one copy or rotation invocation. Intent ids inside one invocation are
// stable, so a driver call retried under the same run replays its receipt; two runs that
// write or delete the same path must not share one. A path-keyed id made that mistake:
// the driver keeps a settled record per intent for IntentRetention, so a second copy of a
// date whose predecessor had been pruned answered from the first run's create receipt,
// and a second prune of a re-created marker could replay the first run's delete receipt.
func newRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint a snapshot run id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// intentID is stable for one destination path inside one run, so a retry of the same
// snapshot reuses the same intent instead of inventing a parallel one, while a later run
// over the same path is a different intent.
func intentID(run string, from, to storage.Path) string {
	sum := sha256.Sum256([]byte(run + "\x00" + string(from) + "\x00" + string(to)))
	return "snapshot:" + hex.EncodeToString(sum[:16])
}

// removalID names one observed native state of one path inside one rotation. A create
// and a later delete of the same path are different effects and must not share an intent:
// the driver keeps a settled create receipt for IntentRetention, so a delete that reused
// the create's id was refused as an intent conflict before it could run. The revision is
// part of the name because a path removed and re-created differently is a different
// effect; the run is part of it because a provider may spell an identical re-created
// object with the same revision (an empty marker's ETag is the empty-content MD5), and
// replaying a settled delete receipt would leave that re-created object behind.
func removalID(run string, p storage.Path, revision []byte) string {
	sum := sha256.Sum256([]byte("snapshot-remove\x00" + run + "\x00" + string(p) + "\x00" + hex.EncodeToString(revision)))
	return "snapshot-remove:" + hex.EncodeToString(sum[:16])
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
