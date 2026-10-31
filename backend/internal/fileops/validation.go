package fileops

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func validateRoot(r Root) error {
	var id pgtype.UUID
	if id.Scan(r.OwnerID) != nil || !id.Valid || r.ConnectionID == "" || r.Generation < 1 || r.ContentRoot == "" || r.SystemRoot == "" || r.ContentRoot == r.SystemRoot {
		return E("invalid_request", nil)
	}
	for _, s := range []string{r.OwnerID, r.ConnectionID, r.ContentRoot, r.SystemRoot} {
		if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
			return E("invalid_request", nil)
		}
	}
	return nil
}
func comparePath(r Root, p storage.Path) string {
	s := norm.NFC.String(string(p))
	if !r.CaseSensitive {
		s = cases.Fold().String(s)
	}
	return s
}
func within(path, base string) bool {
	return base == "" || path == base || strings.HasPrefix(path, base+"/")
}
func guardConflict(r Root, a, b Guard) bool {
	x, y := comparePath(r, a.Path), comparePath(r, b.Path)
	return x == "" || y == "" || x == y || (a.Subtree && within(y, x)) || (b.Subtree && within(x, y))
}
func covered(r Root, guards []Guard, p storage.Path) bool {
	for _, g := range guards {
		if comparePath(r, g.Path) == comparePath(r, p) || (g.Subtree && within(comparePath(r, p), comparePath(r, g.Path))) {
			return true
		}
	}
	return false
}
func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, E("invalid_request", nil)
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, E("invalid_request", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, E("invalid_request", err)
	}
	b, err := json.Marshal(v)
	return b, err
}
func nonemptyEvidence(raw json.RawMessage) bool {
	b, err := canonicalJSON(raw)
	return err == nil && string(b) != "null" && string(b) != "{}" && string(b) != "[]" && string(b) != "\"\"" && string(b) != "false"
}
func canonicalOperation(op *Operation) error {
	fields := []*json.RawMessage{&op.Intent, &op.Result, &op.Problem}
	for i := range op.Entries {
		fields = append(fields, &op.Entries[i].Result)
	}
	for _, field := range fields {
		if len(*field) == 0 || bytes.Equal(bytes.TrimSpace(*field), []byte("null")) {
			*field = nil
			continue
		}
		b, err := canonicalJSON(*field)
		if err != nil {
			return err
		}
		*field = b
	}
	return nil
}
func explicitRemaining(raw json.RawMessage) bool {
	var v struct {
		Remaining []json.RawMessage `json:"remaining"`
	}
	return json.Unmarshal(raw, &v) == nil && len(v.Remaining) > 0
}
func authorizeOperation(op Operation, r Root, a Actor, write bool) error {
	if r != op.Root {
		return E("not_found", nil)
	}
	if a.ID == "" || (!a.CanRead && !a.CanWrite) || (write && !a.CanWrite) {
		return E("forbidden", nil)
	}
	if a.ID != op.PrincipalID && !(a.ManageOwner && a.ID == "owner:"+r.OwnerID && a.CanWrite) {
		return E("forbidden", nil)
	}
	if err := storage.ValidatePath(a.Scope); err != nil {
		return E("forbidden", err)
	}
	for _, g := range op.Guards {
		if !within(comparePath(r, g.Path), comparePath(r, a.Scope)) {
			return E("forbidden", nil)
		}
	}
	return nil
}
func validateBegin(req BeginRequest) (BeginRequest, string, int64, error) {
	fail := func() (BeginRequest, string, int64, error) { return req, "", 0, E("invalid_request", nil) }
	if err := validateRoot(req.Root); err != nil {
		return req, "", 0, err
	}
	if req.Actor.ID == "" || !req.Actor.CanWrite {
		return req, "", 0, E("forbidden", nil)
	}
	if storage.ValidatePath(req.Actor.Scope) != nil {
		return fail()
	}
	switch req.Kind {
	case "mkdir", "move", "copy", "delete", "upload_commit", "thumbnail":
	default:
		return fail()
	}
	if len(req.Guards) == 0 || strings.ContainsRune(req.Actor.ID, 0) || strings.ContainsRune(req.IdempotencyKey, 0) || len(req.IdempotencyKey) > 1024 {
		return fail()
	}
	var err error
	req.Intent, err = canonicalJSON(req.Intent)
	if err != nil {
		return req, "", 0, err
	}
	req.Guards = append([]Guard(nil), req.Guards...)
	for _, g := range req.Guards {
		if storage.ValidatePath(g.Path) != nil {
			return fail()
		}
		if !within(comparePath(req.Root, g.Path), comparePath(req.Root, req.Actor.Scope)) {
			return req, "", 0, E("forbidden", nil)
		}
	}
	sort.Slice(req.Guards, func(i, j int) bool {
		if req.Guards[i].Path == req.Guards[j].Path {
			return !req.Guards[i].Subtree && req.Guards[j].Subtree
		}
		return req.Guards[i].Path < req.Guards[j].Path
	})
	// Manifest identity and captured revisions/conditions never change after Begin.
	req.Manifest = append([]ManifestEntry(nil), req.Manifest...)
	sort.Slice(req.Manifest, func(i, j int) bool { return req.Manifest[i].Number < req.Manifest[j].Number })
	for i, e := range req.Manifest {
		if e.Number < 0 || (i > 0 && req.Manifest[i-1].Number == e.Number) || e.Size < 0 || e.Action == "" || storage.ValidatePath(e.Source) != nil || storage.ValidatePath(e.Target) != nil || e.TargetConditions.Absent && len(e.TargetConditions.ExpectedVersion) > 0 {
			return fail()
		}
		for _, p := range []storage.Path{e.Source, e.Target} {
			if p != "" && (!covered(req.Root, req.Guards, p) || !within(comparePath(req.Root, p), comparePath(req.Root, req.Actor.Scope))) {
				return fail()
			}
		}
	}
	q := req.Reservation
	if q.NewBytes < 0 || q.OldBytes < 0 || q.PhysicalBytes < 0 {
		return fail()
	}
	logical := q.NewBytes
	if q.OldBytes > 0 {
		// P3 only admits replacement credit bound to the typed upload target.
		// Tree/source-size credit requires the P4 finite-manifest accounting model.
		if req.Kind != "upload_commit" {
			return fail()
		}
		if q.CreditPath == "" || len(q.CreditVersion) == 0 || storage.ValidatePath(q.CreditPath) != nil || !covered(req.Root, req.Guards, q.CreditPath) {
			return fail()
		}
		logical = max(q.NewBytes-q.OldBytes, 0)
	} else if q.CreditPath != "" || len(q.CreditVersion) > 0 {
		return fail()
	}
	// A credit is a captured exclusive target revision, never a free-form discount.
	if req.Kind == "upload_commit" {
		var w storage.WriteIntent
		if json.Unmarshal(req.Intent, &w) != nil || w.ID != "" || w.Size != q.NewBytes || w.Size < 0 || w.Target == "" || storage.ValidatePath(w.Target) != nil || !covered(req.Root, req.Guards, w.Target) || w.Conditions.Absent == (len(w.Conditions.ExpectedVersion) > 0) {
			return fail()
		}
		if q.OldBytes > 0 && (w.Target != q.CreditPath || !bytes.Equal(w.Conditions.ExpectedVersion, q.CreditVersion)) {
			return fail()
		}
	}
	if q.OldBytes > 0 && len(req.Manifest) > 0 {
		matched := false
		for _, e := range req.Manifest {
			if e.Target == q.CreditPath && bytes.Equal(e.TargetConditions.ExpectedVersion, q.CreditVersion) || e.Action == "delete" && e.Source == q.CreditPath && bytes.Equal(e.SourceVersion, q.CreditVersion) {
				matched = true
			}
		}
		if !matched {
			return fail()
		}
	}
	fingerprintInput := struct {
		Root            Root
		Principal, Kind string
		Intent          json.RawMessage
		Guards          []Guard
		Manifest        []ManifestEntry
		Reservation     ReservationRequest
	}{req.Root, req.Actor.ID, req.Kind, req.Intent, req.Guards, req.Manifest, q}
	b, err := json.Marshal(fingerprintInput)
	if err != nil {
		return req, "", 0, err
	}
	sum := sha256.Sum256(b)
	return req, hex.EncodeToString(sum[:]), logical, nil
}
func addOK(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}
func budgetFits(used, reserved, add, limit int64) bool {
	n, ok := addOK(used, reserved)
	if !ok {
		return false
	}
	n, ok = addOK(n, add)
	return ok && (add == 0 || limit == 0 || n <= limit)
}
