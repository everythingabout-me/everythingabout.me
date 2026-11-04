package filechanges

import (
	"slices"

	fc "everythingabout.me/backend/internal/filecontract"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// NameKey matches identity's nfc-sensitive-v1 / nfc-fold-v1 comparison. It
// preserves segmentation and never rewrites displayed native spelling.
func NameKey(caseSensitive bool) fc.SegmentKey {
	return func(segment string) string {
		value := norm.NFC.String(segment)
		if !caseSensitive {
			value = norm.NFC.String(cases.Fold().String(value))
		}
		return value
	}
}

// MatchesScope includes descendants, plus ancestor subtree effects which require
// refreshing the visible scope. Plain ancestor metadata edits do not match.
func MatchesScope(scope, candidate fc.Path, coverage string, key fc.SegmentKey) bool {
	return fc.WithinScope(scope, candidate, key) || (coverage == "subtree" && fc.WithinScope(candidate, scope, key))
}

// Project uses retained snapshots only. It never resolves deleted nodes, follows
// former grants, or exposes a concealed move endpoint/parent/actor. The caller
// must still authorize the current principal/scope and retained history access.
func Project(change Change, scope fc.Path, key fc.SegmentKey) (Projection, bool) {
	if fc.ValidatePath(string(scope)) != nil {
		return Projection{}, false
	}
	p := Projection{ID: change.ID, Coverage: change.Coverage, Origin: change.Origin, ObservedAt: change.ObservedAt}
	for _, folder := range change.AffectedFolders {
		if fc.WithinScope(scope, folder.Path, key) {
			p.AffectedFolders = append(p.AffectedFolders, *visibleSnapshot(&folder, scope, key))
		}
	}
	beforeVisible := change.Before != nil && fc.WithinScope(scope, change.Before.Path, key)
	afterVisible := change.After != nil && fc.WithinScope(scope, change.After.Path, key)
	ancestor := false
	for _, snapshot := range []*fc.Metadata{change.Before, change.After} {
		if snapshot != nil && !fc.WithinScope(scope, snapshot.Path, key) && change.Coverage == "subtree" && fc.WithinScope(snapshot.Path, scope, key) {
			ancestor = true
		}
	}
	for _, path := range change.ImpactPaths {
		if fc.WithinScope(scope, path, key) {
			p.ImpactPaths = append(p.ImpactPaths, path)
		}
		if change.Kind == "invalidated" && change.Coverage == "subtree" && !fc.WithinScope(scope, path, key) && fc.WithinScope(path, scope, key) {
			ancestor = true
		}
	}
	if ancestor {
		p.Type, p.Path, p.Coverage = "invalidated", scope, "subtree"
		p.ImpactPaths = []fc.Path{scope}
		return p, true
	}
	if change.Kind == "invalidated" {
		if !beforeVisible && !afterVisible && len(p.ImpactPaths) == 0 {
			return Projection{}, false
		}
		p.Type = "invalidated"
		p.Path = scope
		if beforeVisible {
			p.Path = change.Before.Path
		}
		if afterVisible {
			p.Path = change.After.Path
		}
		if !beforeVisible && !afterVisible && len(p.ImpactPaths) == 1 {
			p.Path = p.ImpactPaths[0]
		}
		// An invalidation never replays a potentially superseded metadata snapshot.
		return p, true
	}
	if !beforeVisible && !afterVisible {
		return Projection{}, false
	}
	if beforeVisible {
		p.Before = visibleSnapshot(change.Before, scope, key)
	}
	if afterVisible {
		p.After = visibleSnapshot(change.After, scope, key)
	}
	switch {
	case beforeVisible && afterVisible && change.Kind == "moved":
		p.Type = "moved"
	case afterVisible:
		p.Type = "upsert"
	default:
		p.Type = "remove"
	}
	if p.After != nil {
		p.EntryID = &p.After.ID
		p.Path = p.After.Path
	} else {
		p.EntryID = &p.Before.ID
		p.Path = p.Before.Path
	}
	slices.Sort(p.ImpactPaths)
	p.ImpactPaths = slices.Compact(p.ImpactPaths)
	return p, true
}

func visibleSnapshot(snapshot *fc.Metadata, scope fc.Path, key fc.SegmentKey) *fc.Metadata {
	copy := *snapshot
	if copy.ParentPath == "" || !fc.WithinScope(scope, copy.ParentPath, key) {
		copy.ParentPath = ""
		copy.ParentID = nil
	}
	return &copy
}

// TouchesID uses retained ancestry, not live nodes. It is a candidate helper for
// later ID-following filters; it supplies no authorization and must be composed
// with Project. A direct-child watch matches only itself and immediate children.
func TouchesID(change Change, id string, recursive bool) bool {
	if change.EntryID != nil && *change.EntryID == id {
		return true
	}
	for _, snapshot := range []*fc.Metadata{change.Before, change.After} {
		if snapshot != nil && snapshot.ParentID != nil && *snapshot.ParentID == id {
			return true
		}
	}
	if recursive {
		for _, ancestors := range [][]Ancestor{change.BeforeAncestors, change.AfterAncestors} {
			for _, ancestor := range ancestors {
				if ancestor.ID == id {
					return true
				}
			}
		}
	}
	return false
}
