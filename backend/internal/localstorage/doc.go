// Package localstorage reads an existing native filesystem tree without a database.
// It supports macOS, Linux and the BSD platforms with native no-follow/nonblocking
// opening; NewFactory rejects other platforms before advertising capabilities.
// It never creates roots or files. Operators must provision separate absolute content
// and system roots; bindings retain directory handles so path replacement cannot
// redirect a live binding. Open accepts explicit operator-trusted roots, including
// canonicalized root aliases. Multi-owner backends must use OpenOwner, which pins
// globally separated trusted bases and opens the exact owner child without following
// owner symlinks. All symlinks and non-regular/non-directory entries are
// rejected, including links whose targets would otherwise remain within the root.
// os.Root provides escape protection; per-component handles and identity checks
// additionally prevent a symlink swap from redirecting traversal into a sibling.
// Administrators must keep mount topology and hard-link exposure trusted: os.Root
// does not isolate bind mounts or make ordinary hard-linked files immutable.
//
// Names retain their exact native UTF-8 spelling, including spaces and NFD. NFC is
// used for collision detection only. CaseSensitive defaults to false, adding Unicode
// case folding to comparisons; selecting true requires an operator-confirmed
// case-sensitive filesystem, rather than an operating-system guess. Alternate
// spelling never silently resolves to the native spelling. The advertised 255-byte
// segment, 4096-byte relative path and 256-component limits are conservative adapter
// limits; the backing filesystem may impose additional restrictions. Native names
// outside these limits cannot be represented by this adapter and fail explicitly.
//
// Listings are byte-sorted immediate children. Each request validates the whole
// directory, bounded by Options.MaxDirectoryEntries, before returning any entries.
// Cursors are authenticated and bound to owner, connection, generation, canonical
// roots, naming rules and parent. They are process/factory-local and are not snapshots:
// external additions or removals between pages may change the subsequent result.
// Concurrent external writes may also change bytes during an open read. Size, mtime
// and an open descriptor do not prove immutability; PinnedRead is false, Entry.Revision
// is empty, and every requested pinned version is rejected as unsupported.
package localstorage
