package connections

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"everythingabout.me/backend/internal/config"
)

// adoptionDeployment is a deployment-shaped pair of roots in a temporary directory:
// the evidence file sits in a state directory and every connection's spool is a child
// of one spool root. Both must already exist, as on a real host.
func adoptionDeployment(t *testing.T) config.Storage {
	t.Helper()
	dir := t.TempDir()
	state, spool := filepath.Join(dir, "state"), filepath.Join(dir, "object-spool")
	for _, p := range []string{state, spool} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return config.Storage{
		ObjectAdmissionFile: filepath.Join(state, "admission.json"),
		ObjectSpoolRoot:     spool,
	}
}

// nameKeyedRow is a row exactly as the release that keyed on the name wrote it: the two
// paths are the name-keyed derivation, not an operator's explicit choice.
func nameKeyedRow(deployment config.Storage, id, name string) Connection {
	return Connection{
		ID: id, Name: name, Backend: "cos", Role: RoleBackup, Enabled: true, Generation: 1,
		Config: Spec{
			ObjectAdmissionFile: filepath.Join(filepath.Dir(deployment.ObjectAdmissionFile), "admission-"+name+".json"),
			ObjectSpoolRoot:     filepath.Join(deployment.ObjectSpoolRoot, name),
		},
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Upgrading must not orphan staged bytes: the whole legacy directory moves, the row is
// repointed to it, and a second start does nothing.
func TestLegacyNameKeyedDirectoryIsAdopted(t *testing.T) {
	deployment := adoptionDeployment(t)
	const id = "0f0e9d5a-3f2b-4b1a-9f8e-2d7c6a5b4c3d"
	row := nameKeyedRow(deployment, id, "nas")
	// A staged part and one non-terminal ledger record, as an upload in flight leaves
	// them.
	staged := filepath.Join(row.Config.ObjectSpoolRoot, "object-mutations-v1-deadbeef", "aa-bb.data")
	writeFile(t, staged, "staged bytes")
	writeFile(t, row.Config.ObjectAdmissionFile, `{"connectionId":"nas:digest"}`)
	store := newFakeStore(row)

	if err := AdoptObjectPaths(context.Background(), store, deployment); err != nil {
		t.Fatalf("adopt the legacy layout: %v", err)
	}

	wantSpool, wantAdmission := filepath.Join(deployment.ObjectSpoolRoot, id), filepath.Join(filepath.Dir(deployment.ObjectAdmissionFile), "admission-"+id+".json")
	if got := store.rows[0].Config.ObjectSpoolRoot; got != wantSpool {
		t.Fatalf("row spool = %q, want %q", got, wantSpool)
	}
	if got := store.rows[0].Config.ObjectAdmissionFile; got != wantAdmission {
		t.Fatalf("row evidence = %q, want %q", got, wantAdmission)
	}
	if data, err := os.ReadFile(filepath.Join(wantSpool, "object-mutations-v1-deadbeef", "aa-bb.data")); err != nil || string(data) != "staged bytes" {
		t.Fatalf("adopted staged bytes = %q, %v", data, err)
	}
	if data, err := os.ReadFile(wantAdmission); err != nil || string(data) != `{"connectionId":"nas:digest"}` {
		t.Fatalf("adopted evidence = %q, %v", data, err)
	}
	if exists(row.Config.ObjectSpoolRoot) || exists(row.Config.ObjectAdmissionFile) {
		t.Fatal("the legacy paths still exist after adoption")
	}

	// Idempotent: a second start finds nothing to move and rewrites nothing.
	before := store.rows[0]
	if err := AdoptObjectPaths(context.Background(), store, deployment); err != nil {
		t.Fatalf("second adoption: %v", err)
	}
	if store.rows[0].Config != before.Config || store.writes != 1 {
		t.Fatalf("the second start rewrote the row: %+v (writes %d)", store.rows[0].Config, store.writes)
	}
}

// A row whose directory is missing on both sides is repointed and gets an empty spool,
// so the driver does not fail on a missing root after the upgrade.
func TestAdoptionRepointsARowWithoutEitherDirectory(t *testing.T) {
	deployment := adoptionDeployment(t)
	const id = "0f0e9d5a-3f2b-4b1a-9f8e-2d7c6a5b4c3d"
	store := newFakeStore(nameKeyedRow(deployment, id, "nas"))
	if err := AdoptObjectPaths(context.Background(), store, deployment); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	want := filepath.Join(deployment.ObjectSpoolRoot, id)
	if got := store.rows[0].Config.ObjectSpoolRoot; got != want {
		t.Fatalf("row spool = %q, want %q", got, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("id-keyed spool directory = %v, %v; want an existing 0700 directory", info, err)
	}
}

// Both layouts can exist after a rollback ran the old binary once more. Disjoint spool
// entries are merged, byte-identical duplicates are dropped, and a genuine conflict
// stops the start instead of discarding a record whose effect may still land.
func TestBothLayoutsMergeAndRefuseToChooseBetweenDifferentContents(t *testing.T) {
	t.Run("merge", func(t *testing.T) {
		deployment := adoptionDeployment(t)
		const id = "0f0e9d5a-3f2b-4b1a-9f8e-2d7c6a5b4c3d"
		row := nameKeyedRow(deployment, id, "nas")
		newSpool := filepath.Join(deployment.ObjectSpoolRoot, id)
		writeFile(t, filepath.Join(row.Config.ObjectSpoolRoot, "namespace", "from-old.json"), "old")
		writeFile(t, filepath.Join(newSpool, "namespace", "from-new.json"), "new")
		writeFile(t, filepath.Join(row.Config.ObjectSpoolRoot, "namespace", "same.json"), "same")
		writeFile(t, filepath.Join(newSpool, "namespace", "same.json"), "same")
		store := newFakeStore(row)

		if err := AdoptObjectPaths(context.Background(), store, deployment); err != nil {
			t.Fatalf("merge the two layouts: %v", err)
		}
		for name, want := range map[string]string{"from-old.json": "old", "from-new.json": "new", "same.json": "same"} {
			if data, err := os.ReadFile(filepath.Join(newSpool, "namespace", name)); err != nil || string(data) != want {
				t.Fatalf("merged %s = %q, %v", name, data, err)
			}
		}
		if exists(row.Config.ObjectSpoolRoot) {
			t.Fatal("the legacy spool was not removed after merging")
		}
	})

	t.Run("conflict", func(t *testing.T) {
		deployment := adoptionDeployment(t)
		const id = "0f0e9d5a-3f2b-4b1a-9f8e-2d7c6a5b4c3d"
		row := nameKeyedRow(deployment, id, "nas")
		newSpool := filepath.Join(deployment.ObjectSpoolRoot, id)
		writeFile(t, filepath.Join(row.Config.ObjectSpoolRoot, "namespace", "record.json"), "old record")
		writeFile(t, filepath.Join(newSpool, "namespace", "record.json"), "new record")
		store := newFakeStore(row)

		err := AdoptObjectPaths(context.Background(), store, deployment)
		if !errors.Is(err, ErrPathConflict) {
			t.Fatalf("conflicting contents gave %v, want ErrPathConflict", err)
		}
		for _, path := range []string{filepath.Join(row.Config.ObjectSpoolRoot, "namespace", "record.json"), filepath.Join(newSpool, "namespace", "record.json")} {
			if !exists(path) {
				t.Fatalf("a conflicting record was removed: %s", path)
			}
		}
		if got := store.rows[0].Config.ObjectSpoolRoot; got != row.Config.ObjectSpoolRoot {
			t.Fatalf("a refused adoption repointed the row to %q", got)
		}
	})
}

// The evidence file is a measurement the probe rewrites, not user state: when both
// copies exist the id-keyed file is the one the layout reads, and the old one is left
// alone rather than deleted under a possibly still-running probe.
func TestAdoptionKeepsTheIDKeyedEvidenceWhenBothExist(t *testing.T) {
	deployment := adoptionDeployment(t)
	const id = "0f0e9d5a-3f2b-4b1a-9f8e-2d7c6a5b4c3d"
	row := nameKeyedRow(deployment, id, "nas")
	want := filepath.Join(filepath.Dir(deployment.ObjectAdmissionFile), "admission-"+id+".json")
	writeFile(t, row.Config.ObjectAdmissionFile, "legacy measurement")
	writeFile(t, want, "current measurement")
	store := newFakeStore(row)

	if err := AdoptObjectPaths(context.Background(), store, deployment); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if data, err := os.ReadFile(want); err != nil || string(data) != "current measurement" {
		t.Fatalf("id-keyed evidence = %q, %v", data, err)
	}
	if data, err := os.ReadFile(row.Config.ObjectAdmissionFile); err != nil || string(data) != "legacy measurement" {
		t.Fatalf("legacy evidence was removed or rewritten: %q, %v", data, err)
	}
	if got := store.rows[0].Config.ObjectAdmissionFile; got != want {
		t.Fatalf("row evidence = %q, want %q", got, want)
	}
}

// 036's whole point is that two owners may name a connection the same thing. The older
// row adopts the one shared directory; the newer row must get its own, never the same
// one, and the shared path is not silently duplicated.
func TestTwoOwnersWithTheSameNameDoNotShareAdoptedState(t *testing.T) {
	deployment := adoptionDeployment(t)
	const (
		older = "11111111-1111-4111-8111-111111111111"
		newer = "22222222-2222-4222-8222-222222222222"
	)
	first := nameKeyedRow(deployment, older, "nas")
	first.OwnerID = "owner-one"
	second := nameKeyedRow(deployment, newer, "nas")
	second.OwnerID = "owner-two"
	writeFile(t, filepath.Join(first.Config.ObjectSpoolRoot, "namespace", "staged.data"), "owner one's bytes")
	store := newFakeStore(first, second)

	if err := AdoptObjectPaths(context.Background(), store, deployment); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if got, want := store.rows[0].Config.ObjectSpoolRoot, filepath.Join(deployment.ObjectSpoolRoot, older); got != want {
		t.Fatalf("the first row adopted %q, want %q", got, want)
	}
	if got, want := store.rows[1].Config.ObjectSpoolRoot, filepath.Join(deployment.ObjectSpoolRoot, newer); got != want {
		t.Fatalf("the second row got %q, want %q", got, want)
	}
	if store.rows[0].Config.ObjectSpoolRoot == store.rows[1].Config.ObjectSpoolRoot {
		t.Fatal("two owners still share one spool directory")
	}
	if !exists(filepath.Join(deployment.ObjectSpoolRoot, older, "namespace", "staged.data")) {
		t.Fatal("the staged bytes did not move with the adopting row")
	}
	if exists(filepath.Join(deployment.ObjectSpoolRoot, newer, "namespace", "staged.data")) {
		t.Fatal("the second owner inherited the first owner's staged bytes")
	}
	if exists(filepath.Join(deployment.ObjectSpoolRoot, "nas")) {
		t.Fatal("the shared name-keyed directory still exists")
	}
}

// An explicit operator path is not a derivation, so adoption must not move it.
func TestAdoptionLeavesExplicitOperatorPathsAlone(t *testing.T) {
	deployment := adoptionDeployment(t)
	const id = "0f0e9d5a-3f2b-4b1a-9f8e-2d7c6a5b4c3d"
	row := nameKeyedRow(deployment, id, "nas")
	explicit := filepath.Join(t.TempDir(), "explicit-spool")
	row.Config.ObjectSpoolRoot = explicit
	row.Config.ObjectAdmissionFile = filepath.Join(t.TempDir(), "explicit.json")
	writeFile(t, filepath.Join(explicit, "namespace", "staged.data"), "explicit bytes")
	store := newFakeStore(row)

	if err := AdoptObjectPaths(context.Background(), store, deployment); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !exists(filepath.Join(explicit, "namespace", "staged.data")) {
		t.Fatal("adoption moved an explicit operator path")
	}
	if store.rows[0].Config.ObjectSpoolRoot != explicit || store.writes != 0 {
		t.Fatalf("adoption rewrote a row with explicit paths: %+v", store.rows[0].Config)
	}
}
