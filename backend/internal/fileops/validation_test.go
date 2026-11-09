package fileops

import (
	"encoding/json"
	"math"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

func testRoot() Root {
	return Root{OwnerID: "11111111-1111-4111-8111-111111111111", ConnectionID: "test", Generation: 1, ContentRoot: "/native/content", SystemRoot: "/native/system", CaseSensitive: true}
}
func testActor(r Root) Actor { return Actor{ID: "owner:" + r.OwnerID, CanRead: true, CanWrite: true} }
func genericRequest(r Root, a Actor, path storage.Path, n int64) BeginRequest {
	return BeginRequest{Root: r, Actor: a, Kind: "copy", Intent: json.RawMessage(`{"source":"original","target":"new"}`), Guards: []Guard{{Path: path}}, Reservation: ReservationRequest{NewBytes: n, PhysicalBytes: n}}
}
func replacementRequest(r Root, a Actor, path storage.Path, newBytes, oldBytes int64, version []byte) BeginRequest {
	req := genericRequest(r, a, path, newBytes)
	req.Kind = "upload_commit"
	req.Reservation.OldBytes = oldBytes
	req.Reservation.CreditPath = path
	req.Reservation.CreditVersion = version
	req.Intent, _ = json.Marshal(storage.WriteIntent{Target: path, Size: newBytes, Conditions: storage.Conditions{ExpectedRevision: version}})
	return req
}
func TestCanonicalFingerprintAndNativePaths(t *testing.T) {
	r := testRoot()
	a := testActor(r)
	x := genericRequest(r, a, "Cafe\u0301 /+%?#", 8)
	x.Intent = json.RawMessage(`{"b":{"z":1,"a":2},"a":3}`)
	y := x
	y.Intent = json.RawMessage("{\n \"a\":3,\"b\":{\"a\":2,\"z\":1}}")
	got, one, _, err := validateBegin(x)
	if err != nil {
		t.Fatal(err)
	}
	_, two, _, err := validateBegin(y)
	if err != nil {
		t.Fatal(err)
	}
	if one != two {
		t.Fatal("JSON key order changed idempotency fingerprint")
	}
	if got.Guards[0].Path != x.Guards[0].Path {
		t.Fatal("native path spelling changed")
	}
	y.Reservation.PhysicalBytes++
	_, three, _, _ := validateBegin(y)
	if three == one {
		t.Fatal("reservation absent from fingerprint")
	}
	y = x
	y.Root.Generation++
	_, three, _, _ = validateBegin(y)
	if three == one {
		t.Fatal("root absent from fingerprint")
	}
}
func TestGuardComparison(t *testing.T) {
	r := testRoot()
	cases := []struct {
		a, b     Guard
		conflict bool
	}{{Guard{"dir", true}, Guard{"dir/file", false}, true}, {Guard{"dir/file", false}, Guard{"dir", true}, true}, {Guard{"dir", false}, Guard{"dir/file", false}, false}, {Guard{"dir", true}, Guard{"directory/file", false}, false}, {Guard{"", true}, Guard{"anything", false}, true}, {Guard{"Cafe\u0301", false}, Guard{"Café", false}, true}, {Guard{"NAME", false}, Guard{"name", false}, false}}
	for _, c := range cases {
		if got := guardConflict(r, c.a, c.b); got != c.conflict {
			t.Errorf("%+v %+v: %v", c.a, c.b, got)
		}
	}
	r.CaseSensitive = false
	if !guardConflict(r, Guard{"Straße", true}, Guard{"STRASSE/file", false}) {
		t.Fatal("Unicode case folding did not guard equivalent path")
	}
}
func TestReservationValidation(t *testing.T) {
	r := testRoot()
	a := testActor(r)
	req := genericRequest(r, a, "file", 10)
	req.Reservation.OldBytes = 7
	if _, _, _, err := validateBegin(req); !IsCode(err, "invalid_request") {
		t.Fatalf("unbound credit: %v", err)
	}
	req.Reservation.CreditPath = "file"
	req.Reservation.CreditVersion = []byte("v1")
	if _, _, _, err := validateBegin(req); !IsCode(err, "invalid_request") {
		t.Fatal("generic intent minted old-size credit")
	}
	req = replacementRequest(r, a, "file", 10, 7, []byte("v1"))
	if _, _, n, err := validateBegin(req); err != nil || n != 3 {
		t.Fatalf("growth %d: %v", n, err)
	}
	req = replacementRequest(r, a, "file", 2, 7, []byte("v1"))
	if _, _, n, err := validateBegin(req); err != nil || n != 0 {
		t.Fatalf("shrink %d: %v", n, err)
	}
	req.Reservation.CreditPath = "other"
	if _, _, _, err := validateBegin(req); !IsCode(err, "invalid_request") {
		t.Fatal("unprotected credit accepted")
	}
	if budgetFits(math.MaxInt64, 1, 0, 0) || budgetFits(1, math.MaxInt64, 1, 0) {
		t.Fatal("integer overflow admitted")
	}
}
func TestScopeAndExplicitOwnerManagement(t *testing.T) {
	r := testRoot()
	a := testActor(r)
	op := Operation{Root: r, PrincipalID: "token:first", Guards: []Guard{{"scope/deleted", true}}}
	a.ID = "token:other"
	a.ManageOwner = true
	if !IsCode(authorizeOperation(op, r, a, false), "forbidden") {
		t.Fatal("another token got owner management")
	}
	a.ID = "owner:" + r.OwnerID
	a.Scope = "scope"
	if err := authorizeOperation(op, r, a, true); err != nil {
		t.Fatal(err)
	}
	a.Scope = "scope/child"
	if !IsCode(authorizeOperation(op, r, a, false), "forbidden") {
		t.Fatal("scope narrowing ignored")
	}
	a.Scope = ""
	a.CanWrite = false
	if !IsCode(authorizeOperation(op, r, a, false), "forbidden") {
		t.Fatal("read-only owner management accepted")
	}
}
func TestUploadIntentBinding(t *testing.T) {
	r := testRoot()
	a := testActor(r)
	req := genericRequest(r, a, "file", 8)
	req.Kind = "upload_commit"
	w := storage.WriteIntent{Target: "file", Size: 8, Conditions: storage.Conditions{Absent: true}}
	req.Intent, _ = json.Marshal(w)
	if _, _, _, err := validateBegin(req); err != nil {
		t.Fatal(err)
	}
	w.Size = 9
	req.Intent, _ = json.Marshal(w)
	if _, _, _, err := validateBegin(req); !IsCode(err, "invalid_request") {
		t.Fatal("stage size not bound to reservation")
	}
	w.Size = 8
	w.ID = "external"
	req.Intent, _ = json.Marshal(w)
	if _, _, _, err := validateBegin(req); !IsCode(err, "invalid_request") {
		t.Fatal("caller supplied operation ID accepted")
	}
	w.ID = ""
	for _, conditions := range []storage.Conditions{{}, {Absent: true, ExpectedRevision: []byte("v1")}} {
		w.Conditions = conditions
		req.Intent, _ = json.Marshal(w)
		if _, _, _, err := validateBegin(req); !IsCode(err, "invalid_request") {
			t.Fatal("missing or conflicting native conditions accepted")
		}
	}
}
