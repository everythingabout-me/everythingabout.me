package fileops

// Which steps take the owner's advisory lock, and which do not.
//
// The lock is per owner, so a step that takes it makes every other operation of that owner
// wait. A 20k-file import showed what that costs: the publication's five bookkeeping steps
// write nothing but one operation's own row, yet each queued behind the others and behind
// every other file's settlement, and the lock — not the work — bounded the import at about
// ten files a second.
//
// The guard records which side of that line each step is on, because the difference is one
// method name in a diff: `p.leased` and `p.leasedOwnRow` are one word apart, and moving a
// step that touches owner-wide state to the cheaper one would be a correctness bug no
// functional test would catch until two operations raced.

import (
	"os"
	"strings"
	"testing"
)

func readFileopsSource(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func TestOwnerLockScopeMatchesTheStateEachStepWrites(t *testing.T) {
	transitions := readFileopsSource(t, "postgres_transitions.go")
	postgres := readFileopsSource(t, "postgres.go")
	cleanup := readFileopsSource(t, "postgres_cleanup.go")

	// One operation's own row (its checkpoints and entries included): the row lock
	// readOperation takes is the whole exclusion, so the owner lock is pure queueing.
	for _, name := range []string{
		"SaveCheckpoint", "SaveStage", "StartEffect", "SaveApplied", "MarkUnknown", "BeginEntry", "SaveEntry",
	} {
		body := functionBody(t, transitions, "func (p *Postgres) "+name+"(")
		if !strings.Contains(body, "p.leasedOwnRow(") {
			t.Fatalf("%s no longer uses leasedOwnRow: a step that writes only its own operation must not queue the owner's other files", name)
		}
		if strings.Contains(body, "p.leased(") {
			t.Fatalf("%s takes the owner lock again; its writes are one operation's own row", name)
		}
	}

	// Owner-wide state: the lock is the invariant (budget, change feed head, guards).
	for _, name := range []string{"Settle", "FinishWithoutEffect"} {
		body := functionBody(t, transitions, "func (p *Postgres) "+name+"(")
		if !strings.Contains(body, "p.leased(") {
			t.Fatalf("%s must take the owner lock: it writes state two operations share", name)
		}
	}

	// Cleanup writes cleaned_at on one row, so it is the own-row shape too — spelled
	// directly on the transaction rather than through a lease.
	cleaned := functionBody(t, cleanup, "func (p *Postgres) MarkCleaned(")
	if !strings.Contains(cleaned, "p.transactionOwnRow(") {
		t.Fatal("MarkCleaned takes the owner lock again; it writes one operation's cleaned_at")
	}

	// And the two shapes stay exactly two: the ordered one takes the advisory lock, the
	// other one must not.
	if !strings.Contains(functionBody(t, postgres, "func (p *Postgres) transaction("), "ownerLock(") {
		t.Fatal("transaction no longer takes the owner lock")
	}
	if strings.Contains(functionBody(t, postgres, "func (p *Postgres) transactionOwnRow("), "ownerLock(") {
		t.Fatal("transactionOwnRow takes the owner lock; it is the unordered shape")
	}
}
