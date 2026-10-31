package localstorage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"everythingabout.me/backend/internal/storage"
)

// OpenOwner opens cfg.OwnerID as an exact immediate child of each trusted base.
// Unlike Open, it never canonicalizes or follows symlinks in the owner component.
// cfg roots must equal filepath.Join(base, cfg.OwnerID). Bases are operator-trusted
// configuration and must be physically disjoint, even if selected owner subtrees
// happen to be disjoint. All returned root handles remain anchored through renames.
func (f *Factory) OpenOwner(ctx context.Context, cfg storage.Config, contentBase, systemBase string) (storage.Driver, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := storage.ValidateBinding(cfg); err != nil {
		return nil, err
	}
	if !validIdentity(cfg.OwnerID) || !validIdentity(cfg.ConnectionID) || validatePath(storage.Path(cfg.OwnerID)) != nil || filepath.Base(cfg.OwnerID) != cfg.OwnerID {
		return nil, storage.NewError("invalid_config", "", nil)
	}
	if !filepath.IsAbs(contentBase) || !filepath.IsAbs(systemBase) || cfg.ContentRoot != filepath.Join(contentBase, cfg.OwnerID) || cfg.SystemRoot != filepath.Join(systemBase, cfg.OwnerID) {
		return nil, storage.NewError("invalid_config", "", nil)
	}
	// Open validates global physical separation and pins both trusted bases. There
	// is deliberately no subsequent absolute-path lookup of an owner directory.
	baseConfig := cfg
	baseConfig.ContentRoot = contentBase
	baseConfig.SystemRoot = systemBase
	opened, err := f.Open(ctx, baseConfig)
	if err != nil {
		return nil, err
	}
	bases := opened.(*driver)
	defer bases.Close()
	release, err := bases.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	content, ci, err := bases.ownerChild(ctx, bases.content, cfg.OwnerID)
	if err != nil {
		return nil, err
	}
	system, si, err := bases.ownerChild(ctx, bases.system, cfg.OwnerID)
	if err != nil {
		content.Close()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		content.Close()
		system.Close()
		return nil, err
	}
	if os.SameFile(ci, si) {
		content.Close()
		system.Close()
		return nil, storage.NewError("invalid_config", "", nil)
	}
	return f.bindHandles(cfg, content, system, filepath.Join(bases.content.Name(), cfg.OwnerID), filepath.Join(bases.system.Name(), cfg.OwnerID), ci, si), nil
}

// ownerChild enumerates names without opening unrelated owners. This allows a
// malformed sibling to be reported only when selected, without exposing it.
func (d *driver) ownerChild(ctx context.Context, base *os.Root, owner string) (*os.Root, os.FileInfo, error) {
	invalid := func(err error) (*os.Root, os.FileInfo, error) {
		return nil, nil, storage.NewError("invalid_config", "", err)
	}
	listing, err := base.Open(".")
	if err != nil {
		return invalid(err)
	}
	defer listing.Close()
	found := false
	seen := make(map[string]bool)
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		entries, readErr := listing.ReadDir(256)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return invalid(readErr)
		}
		for _, e := range entries {
			count++
			if count > d.factory.options.MaxDirectoryEntries {
				return nil, nil, storage.NewError("resource_limit", "", nil)
			}
			key := d.key(e.Name())
			if seen[key] {
				return invalid(nil)
			}
			seen[key] = true
			if e.Name() == owner {
				found = true
			} else if key == d.key(owner) {
				return invalid(nil)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if !found {
		return nil, nil, storage.NewError("not_found", storage.Path(owner), nil)
	}
	before, err := base.Lstat(owner)
	if err != nil {
		return invalid(err)
	}
	if !before.IsDir() {
		return invalid(nil)
	}
	// O_NOFOLLOW pins the actual directory rather than a symlink substituted
	// after Lstat. Holding its FD prevents inode reuse during the Root conversion.
	pinned, err := openReadOnly(base, owner)
	if err != nil {
		return invalid(err)
	}
	defer pinned.Close()
	info, err := pinned.Stat()
	if err != nil || !info.IsDir() || !os.SameFile(before, info) {
		return invalid(err)
	}
	root, err := base.OpenRoot(owner)
	if err != nil {
		return invalid(err)
	}
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(info, actual) {
		root.Close()
		return invalid(err)
	}
	if err := ctx.Err(); err != nil {
		root.Close()
		return nil, nil, err
	}
	return root, actual, nil
}
