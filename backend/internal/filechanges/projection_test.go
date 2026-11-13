package filechanges

import (
	"encoding/json"
	"strings"
	"testing"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/namecompare"
)

const testOwner = "81995470-212d-4f40-934c-8435dba67a0f"
const testEntry = "1cdd02be-64b8-480e-997d-d94cfcb5089b"
const testRoot = "c320d389-e9c3-41b7-b81d-b9b6ee2b1ec0"

func snapshot(id string, path fc.Path, kind string) *fc.Metadata {
	parent := fc.Path("/")
	if cut := strings.LastIndex(string(path), "/"); cut > 0 {
		parent = path[:cut]
	}
	parentID := testRoot
	m := &fc.Metadata{ID: id, ParentID: &parentID, Path: path, ParentPath: parent, Name: string(path)[strings.LastIndex(string(path), "/")+1:], Kind: kind, Revision: "nr_first"}
	if path == "/" {
		m.ParentID = nil
		m.ParentPath = ""
	}
	return m
}

func TestProjectionConcealsMovesAndParents(t *testing.T) {
	before, after := snapshot(testEntry, "/Apps/Notes/a.txt", "file"), snapshot(testEntry, "/Private/secret.txt", "file")
	change := Change{ID: testRoot, Kind: "moved", Coverage: "entry", EntryID: &before.ID, Before: before, After: after,
		Actor: Actor{Kind: "token", PrincipalID: "hidden-principal"}, EffectKey: "private-effect", Position: 42,
		BeforeAncestors: []Ancestor{{ID: testRoot, Path: "/"}}, ImpactPaths: []fc.Path{"/", "/Private"}}
	projected, ok := Project(change, "/Apps/Notes", namecompare.Key(true))
	if !ok || projected.Type != "remove" || projected.Before == nil || projected.After != nil {
		t.Fatalf("departure %+v visible=%v", projected, ok)
	}
	data, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"/Private", "secret.txt", "hidden-principal", "private-effect", "position", "beforeAncestors"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("projection leaked %q: %s", secret, data)
		}
	}
	if before.ParentID == nil {
		t.Fatal("projection changed its retained snapshot")
	}
	arrival, ok := Project(change, "/Private", namecompare.Key(true))
	if !ok || arrival.Type != "upsert" || arrival.Before != nil || arrival.After.Path != after.Path {
		t.Fatalf("arrival %+v %v", arrival, ok)
	}
	both, ok := Project(change, "/", namecompare.Key(true))
	if !ok || both.Type != "moved" || both.Before == nil || both.After == nil {
		t.Fatalf("owner move %+v %v", both, ok)
	}
	atRoot := Change{Kind: "updated", Coverage: "entry", Before: snapshot(testEntry, "/Apps/Notes", "folder"), After: snapshot(testEntry, "/Apps/Notes", "folder")}
	redacted, ok := Project(atRoot, "/Apps/Notes", namecompare.Key(true))
	if !ok || redacted.After.ParentID != nil || redacted.After.ParentPath != "" {
		t.Fatalf("concealed parent %+v", redacted)
	}
}

func TestNativeAwareScopeAndAncestorInvalidation(t *testing.T) {
	key := namecompare.Key(false)
	for _, candidate := range []fc.Path{"/Apps/Straße/cafe\u0301.txt", "/APPS/STRASSE/Café.txt"} {
		if !MatchesScope("/apps/STRASSE", candidate, "entry", key) {
			t.Fatalf("missed native-equivalent path %q", candidate)
		}
	}
	if MatchesScope("/Apps/Notes", "/Apps/Notes-old/a", "entry", key) {
		t.Fatal("raw prefix authorized sibling")
	}
	before, after := snapshot(testEntry, "/Apps", "folder"), snapshot(testEntry, "/Private", "folder")
	change := Change{ID: testRoot, Kind: "moved", Coverage: "subtree", Before: before, After: after}
	p, ok := Project(change, "/Apps/Notes", key)
	if !ok || p.Type != "invalidated" || p.Path != "/Apps/Notes" || p.EntryID != nil || p.Before != nil || p.After != nil {
		t.Fatalf("ancestor invalidation %+v %v", p, ok)
	}
	change.Coverage = "entry"
	if _, ok := Project(change, "/Apps/Notes", key); ok {
		t.Fatal("ordinary ancestor metadata emitted a descendant event")
	}
	change = Change{Kind: "invalidated", Coverage: "subtree", ImpactPaths: []fc.Path{"/Apps"}}
	p, ok = Project(change, "/Apps/Notes", key)
	if !ok || p.Path != "/Apps/Notes" || p.Before != nil || p.After != nil {
		t.Fatalf("bounded invalidation %+v %v", p, ok)
	}
	if _, ok := Project(change, "/Unrelated", key); ok {
		t.Fatal("unrelated invalidation projected")
	}
}

func TestRetainedAncestryMatchesIdentityWithoutLiveNodes(t *testing.T) {
	parent := "81995470-212d-4f40-934c-8435dba67a0f"
	before := snapshot(testEntry, "/Notes/Sub/a.txt", "file")
	before.ParentID = &parent
	change := Change{Kind: "deleted", EntryID: &before.ID, Before: before, BeforeAncestors: []Ancestor{{ID: testRoot, Path: "/"}, {ID: parent, Path: "/Notes/Sub"}}}
	if !TouchesID(change, testEntry, false) || !TouchesID(change, parent, false) || TouchesID(change, testRoot, false) || !TouchesID(change, testRoot, true) {
		t.Fatalf("retained ID matching failed: %+v", change)
	}
}

func TestChangeValidationAndFingerprintRejectFalseContinuity(t *testing.T) {
	base := Change{OwnerID: testOwner, EffectKey: "effect", Kind: "created", After: snapshot(testEntry, "/Notes/a.txt", "file")}
	valid, err := normalizeChange(base)
	if err != nil || valid.EntryID == nil || *valid.EntryID != testEntry {
		t.Fatalf("valid change %+v %v", valid, err)
	}
	for _, edit := range []func(*Change){
		func(c *Change) { c.EffectKey = "" }, func(c *Change) { c.OwnerID = "foreign" }, func(c *Change) { c.Coverage = "everything" },
		func(c *Change) { c.Kind = "moved"; c.Before = c.After }, func(c *Change) { id := testRoot; c.EntryID = &id },
		func(c *Change) { copy := *c.After; copy.Path = "/Notes/../outside"; c.After = &copy },
	} {
		c := base
		edit(&c)
		if _, err := normalizeChange(c); err == nil {
			t.Fatalf("invalid change accepted %+v", c)
		}
	}
	first, err := changeFingerprint(valid)
	if err != nil {
		t.Fatal(err)
	}
	valid.Actor.PrincipalID = "another-principal"
	second, err := changeFingerprint(valid)
	if err != nil || first == second {
		t.Fatal("changed actor did not change immutable effect fingerprint", err)
	}
	encoded, err := json.Marshal(Head{Position: 42, RetainedAfter: 2, OwnerID: testOwner, RootEpoch: testRoot})
	if err != nil || string(encoded) != "{}" {
		t.Fatalf("raw head leaked positions: %s %v", encoded, err)
	}
}
