package fileops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"everythingabout.me/backend/internal/storage"
)

// ActionsOptions connects action authorization, public revision validation, and
// transactional index settlement to the native coordinator.
type ActionsOptions struct {
	Authorize Authorize
	Validate  ActionValidation
	Commit    IndexCommit
	LeaseTTL  time.Duration
}
type Actions struct {
	journal *Postgres
	options ActionsOptions
	service *Service
}

func NewActions(journal *Postgres, options ActionsOptions) (*Actions, error) {
	if journal == nil || options.Validate == nil {
		return nil, E("invalid_config", nil)
	}
	service, err := NewService(journal, ServiceOptions{Authorize: options.Authorize, Commit: options.Commit, LeaseTTL: options.LeaseTTL})
	if err != nil {
		return nil, err
	}
	options.LeaseTTL = service.options.LeaseTTL
	return &Actions{journal: journal, options: options, service: service}, nil
}

// actionBinding is frozen once under the admission guard. Recovery never lists
// the tree again. Folder versions must identify the folder, independent of child
// membership, so our own top-down creations and postorder removals preserve them.
type actionBinding struct {
	Anchors  []storage.Entry
	Sources  []storage.Entry
	Existing *storage.Entry
}

const maxActionEntries = 100000
const maxActionPages = 1000

func validateActionRequest(req ActionRequest, native NativeActions) error {
	if err := validateRoot(req.Root); err != nil {
		return err
	}
	if native == nil {
		return E("unsupported", nil)
	}
	if err := validateNativeBinding(req.Root, native.StorageBinding(), native.StorageNameRules()); err != nil {
		return err
	}
	if req.Root.RetainsVersions != native.Describe().RetainsVersions {
		return E("root_conflict", nil)
	}
	if req.Actor.ID == "" || !req.Actor.CanWrite || storage.ValidatePath(req.Actor.Scope) != nil {
		return E("forbidden", nil)
	}
	if storage.ValidatePath(req.From) != nil || storage.ValidatePath(req.To) != nil {
		return E("invalid_path", nil)
	}
	switch req.Kind {
	case "mkdir":
		if req.To == "" || req.From != "" || req.Recursive || req.IfRevision != "" {
			return E("invalid_request", nil)
		}
	case "copy", "move":
		if req.From == "" || req.To == "" || req.Parents || req.Recursive {
			return E("invalid_request", nil)
		}
		if req.IfRevision == "" {
			return E("precondition_required", nil)
		}
		from, to := comparePath(req.Root, req.From), comparePath(req.Root, req.To)
		if within(to, from) || within(from, to) {
			return E("invalid_request", nil)
		}
	case "delete":
		if req.From == "" || req.To != "" || req.Parents {
			return E("invalid_request", nil)
		}
	default:
		return E("invalid_request", nil)
	}
	for _, p := range []storage.Path{req.From, req.To} {
		if p != "" && !within(comparePath(req.Root, p), comparePath(req.Root, req.Actor.Scope)) {
			return E("forbidden", nil)
		}
	}
	return nil
}
func actionClientEqual(a, b ActionRequest) bool {
	return a.Root == b.Root && a.Actor.ID == b.Actor.ID && a.Kind == b.Kind && a.From == b.From && a.To == b.To && a.Parents == b.Parents && a.Recursive == b.Recursive && a.IfRevision == b.IfRevision
}
func actionRequest(op Operation) (ActionRequest, error) {
	var req ActionRequest
	if json.Unmarshal(op.Intent, &req) != nil || req.Root != op.Root || req.Kind != op.Kind || req.Actor.ID != op.PrincipalID {
		return req, E("invalid_intent", nil)
	}
	return req, nil
}

// actionGuards fences exactly the native paths this action observes or mutates.
// An outstanding operation elsewhere in the drive (a slow or abandoned upload, for
// example) keeps its own target guard for its whole session and must not turn an
// unrelated delete, move, copy or mkdir into operation_in_progress.
//
// mkdir with Parents is the exception: its missing ancestors are discovered only
// after the guard is taken, and the frozen manifest must keep them covered, so that
// case retains the actor-scope fence.
func actionGuards(req ActionRequest) []Guard {
	switch req.Kind {
	case "delete":
		return []Guard{{Path: req.From, Subtree: req.Recursive}}
	case "move", "copy":
		return []Guard{{Path: req.From, Subtree: true}, {Path: req.To, Subtree: true}}
	case "mkdir":
		if !req.Parents {
			return []Guard{{Path: req.To, Subtree: true}}
		}
	}
	return []Guard{{Path: req.Actor.Scope, Subtree: true}}
}

// Prepare acquires an exclusion guard before any native observation, validates
// public revision conditions, then freezes a finite manifest and exact liability.
// Idempotent retries consult immutable client intent before inspecting live paths.
func (a *Actions) Prepare(ctx context.Context, req ActionRequest, native NativeActions) (Operation, bool, error) {
	if err := validateActionRequest(req, native); err != nil {
		return Operation{}, false, err
	}
	if op, found, err := a.journal.ReplayAction(ctx, req); err != nil || found {
		if err == nil {
			err = a.options.Authorize(ctx, op)
		}
		return op, found, err
	}
	intent, err := json.Marshal(req)
	if err != nil {
		return Operation{}, false, err
	}
	op, replay, err := a.journal.Begin(ctx, BeginRequest{Root: req.Root, Actor: req.Actor, Kind: req.Kind, IdempotencyKey: req.IdempotencyKey, Intent: intent, Guards: actionGuards(req)})
	if err != nil {
		return op, replay, err
	}
	if replay {
		err = a.options.Authorize(ctx, op)
		return op, true, err
	}
	op, err = a.journal.Claim(ctx, req.Root, req.Actor, op.ID, a.options.LeaseTTL)
	if err != nil {
		return op, false, err
	}
	work, stop := a.service.keepLease(ctx, op.Lease)
	defer stop()
	fail := func(cause error) (Operation, bool, error) {
		persist, cancel := durableContext(ctx)
		defer cancel()
		code := actionErrorCode(cause)
		if finishErr := a.journal.FinishWithoutEffect(persist, op.Lease, Failed, problem(code, cause)); finishErr != nil {
			return op, false, errors.Join(cause, finishErr)
		}
		saved, readErr := a.journal.Get(persist, req.Root, req.Actor, op.ID)
		return saved, false, errors.Join(cause, readErr)
	}
	if err = a.options.Authorize(work, op); err != nil {
		return fail(err)
	}
	if err = a.options.Validate(work, native, req); err != nil {
		return fail(err)
	}
	binding, manifest, reservation, err := captureAction(work, req, native)
	if err != nil {
		return fail(err)
	}
	// Re-observe every captured identity and every listing while the application
	// guard remains held. This catches outside edits during a paginated observation.
	if err = validateCapture(work, req, native, binding, manifest); err != nil {
		return fail(err)
	}
	if err = a.options.Validate(work, native, req); err != nil {
		return fail(err)
	}
	if err = a.journal.FreezeAction(work, op.Lease, binding, manifest, reservation); err != nil {
		return fail(err)
	}
	op, err = a.journal.Get(work, req.Root, req.Actor, op.ID)
	return op, false, err
}
func validateActionName(p storage.Path, rules storage.NameRules) error {
	if storage.ValidatePath(p) != nil {
		return E("invalid_name", nil)
	}
	parts := strings.Split(string(p), "/")
	if rules.MaxPathBytes > 0 && len(p) > rules.MaxPathBytes || rules.MaxDepth > 0 && len(parts) > rules.MaxDepth {
		return E("invalid_name", nil)
	}
	root := Root{CaseSensitive: rules.CaseSensitive}
	for _, part := range parts {
		if rules.MaxSegmentBytes > 0 && len(part) > rules.MaxSegmentBytes {
			return E("invalid_name", nil)
		}
		for _, reserved := range rules.ReservedNames {
			if comparePath(root, storage.Path(part)) == comparePath(root, storage.Path(reserved)) {
				return E("invalid_name", nil)
			}
		}
	}
	return nil
}
func parentPath(p storage.Path) storage.Path {
	if i := strings.LastIndexByte(string(p), '/'); i >= 0 {
		return p[:i]
	}
	return ""
}
func requireVersion(e storage.Entry) error {
	// Prefix folders without a native marker have no revision; they exist only
	// through their children and are never the subject of a conditional effect.
	if storage.ValidatePath(e.Path) != nil || len(e.Version) == 0 && e.Kind != storage.Folder || e.Size < 0 || e.Size > 9007199254740991 || e.Kind != storage.File && e.Kind != storage.Folder || e.Kind == storage.Folder && e.Size != 0 {
		return E("unsupported", nil)
	}
	return nil
}
func cloneActionEntry(e storage.Entry) storage.Entry {
	e.NativeRef = bytes.Clone(e.NativeRef)
	e.Version = bytes.Clone(e.Version)
	e.ContentIdentity = bytes.Clone(e.ContentIdentity)
	if e.Digest != nil {
		digest := *e.Digest
		digest.Value = bytes.Clone(digest.Value)
		e.Digest = &digest
	}
	return e
}

// implicitFolderGone reports that a captured marker-less prefix folder vanished
// because its children were removed; that is the expected outcome, not a change.
func implicitFolderGone(captured storage.Entry, err error) bool {
	return captured.Kind == storage.Folder && len(captured.Version) == 0 && storage.IsCode(err, "not_found")
}
func sameNative(a, b storage.Entry) bool {
	return a.Path == b.Path && a.Kind == b.Kind && a.Size == b.Size && bytes.Equal(a.Version, b.Version) && bytes.Equal(a.NativeRef, b.NativeRef)
}
func observe(ctx context.Context, native NativeActions, p storage.Path) (storage.Entry, error) {
	e, err := native.Stat(ctx, p)
	if err != nil {
		return e, err
	}
	if e.Path != p {
		return e, E("target_changed", nil)
	}
	return cloneActionEntry(e), requireVersion(e)
}
func captureAction(ctx context.Context, req ActionRequest, native NativeActions) (actionBinding, []ManifestEntry, ReservationRequest, error) {
	var binding actionBinding
	var manifest []ManifestEntry
	var reserve ReservationRequest
	fail := func(err error) (actionBinding, []ManifestEntry, ReservationRequest, error) {
		return binding, nil, reserve, err
	}
	anchors := map[storage.Path]bool{}
	addAncestors := func(p storage.Path) error {
		for {
			if !anchors[p] {
				e, err := observe(ctx, native, p)
				if err != nil {
					return err
				}
				if e.Kind != storage.Folder {
					return E("not_a_folder", nil)
				}
				binding.Anchors = append(binding.Anchors, e)
				anchors[p] = true
			}
			if p == "" {
				break
			}
			p = parentPath(p)
		}
		return nil
	}
	sourceEvidence := make(map[storage.Path]storage.Entry)
	add := func(action string, source, target storage.Path, version []byte, size int64) {
		entry := ManifestEntry{Number: len(manifest), Action: action, Source: source, Target: target, SourceVersion: bytes.Clone(version), TargetConditions: storage.Conditions{Absent: action != "delete"}, Size: size}
		if source != "" {
			cloned := cloneActionEntry(sourceEvidence[source])
			entry.SourceEntry = &cloned
		}
		manifest = append(manifest, entry)
	}
	if req.Kind == "mkdir" {
		var missing []storage.Path
		p := req.To
		for {
			e, err := observe(ctx, native, p)
			if err == nil {
				if e.Kind != storage.Folder {
					return fail(E("name_conflict", nil))
				}
				if p == req.To {
					binding.Existing = &e
				}
				if err = addAncestors(p); err != nil {
					return fail(err)
				}
				break
			}
			if !storage.IsCode(err, "not_found") {
				return fail(err)
			}
			if p == "" {
				return fail(E("parent_missing", nil))
			}
			missing = append(missing, p)
			if !req.Parents && p != req.To {
				return fail(E("parent_missing", nil))
			}
			p = parentPath(p)
		}
		for i := len(missing) - 1; i >= 0; i-- {
			if err := validateActionName(missing[i], native.StorageNameRules()); err != nil {
				return fail(err)
			}
			add("mkdir", "", missing[i], nil, 0)
		}
		return binding, manifest, reserve, nil
	}
	if err := addAncestors(parentPath(req.From)); err != nil {
		return fail(err)
	}
	sources, err := snapshotActionTree(ctx, native, req.From, req.Kind != "delete" || req.Recursive)
	if err != nil {
		return fail(err)
	}
	binding.Sources = sources
	for _, source := range sources {
		sourceEvidence[source.Path] = source
	}
	if req.Kind != "delete" {
		if err = addAncestors(parentPath(req.To)); err != nil {
			if storage.IsCode(err, "not_found") {
				err = E("parent_missing", err)
			}
			return fail(err)
		}
		if _, err = native.Stat(ctx, req.To); err == nil {
			return fail(E("name_conflict", nil))
		} else if !storage.IsCode(err, "not_found") {
			return fail(err)
		}
		for _, e := range sources {
			target := req.To + storage.Path(strings.TrimPrefix(string(e.Path), string(req.From)))
			if err = validateActionName(target, native.StorageNameRules()); err != nil {
				return fail(err)
			}
			if e.Kind == storage.Folder {
				add("mkdir", "", target, nil, 0)
			} else {
				add("copy", e.Path, target, e.Version, e.Size)
				var ok bool
				reserve.NewBytes, ok = addOK(reserve.NewBytes, e.Size)
				if !ok || reserve.NewBytes > 9007199254740991 {
					return fail(E("request_too_large", nil))
				}
				reserve.PhysicalBytes, err = PhysicalReservation(req.Root, reserve.NewBytes)
				if err != nil {
					return fail(err)
				}
			}
		}
	}
	if req.Kind == "delete" || req.Kind == "move" {
		for i := len(sources) - 1; i >= 0; i-- {
			e := sources[i]
			add("delete", e.Path, "", e.Version, e.Size)
			var ok bool
			reserve.OldBytes, ok = addOK(reserve.OldBytes, e.Size)
			if !ok || reserve.OldBytes > 9007199254740991 {
				return fail(E("request_too_large", nil))
			}
		}
	}
	if reserve.NewBytes > 9007199254740991 {
		return fail(E("request_too_large", nil))
	}
	return binding, manifest, reserve, nil
}

// snapshotActionTree accepts only immediate, unambiguous children and bounded
// pagination. It never follows a driver-provided path out of the selected tree.
func snapshotActionTree(ctx context.Context, native NativeActions, root storage.Path, recursive bool) ([]storage.Entry, error) {
	first, err := observe(ctx, native, root)
	if err != nil {
		return nil, err
	}
	entries := []storage.Entry{first}
	seen := map[string]bool{comparePathFromNative(native, root): true}
	pages := 0
	for i := 0; i < len(entries); i++ {
		folder := entries[i]
		if folder.Kind != storage.Folder {
			continue
		}
		cursor := ""
		cursors := map[string]bool{}
		var children []storage.Entry
		for {
			pages++
			if pages > maxActionPages {
				return nil, E("request_too_large", nil)
			}
			page, err := native.List(ctx, folder.Path, storage.PageRequest{Cursor: cursor, Limit: 1000})
			if err != nil {
				return nil, err
			}
			if len(page.Entries) > 1000 {
				return nil, E("invalid_native_result", nil)
			}
			if !recursive && len(page.Entries) > 0 {
				return nil, E("directory_not_empty", nil)
			}
			for _, e := range page.Entries {
				key := comparePathFromNative(native, e.Path)
				if e.Path == "" || parentPath(e.Path) != folder.Path || seen[key] || requireVersion(e) != nil {
					return nil, E("ambiguous_path", nil)
				}
				seen[key] = true
				children = append(children, cloneActionEntry(e))
				if len(entries)+len(children) > maxActionEntries {
					return nil, E("request_too_large", nil)
				}
			}
			if page.NextCursor == "" {
				break
			}
			if cursors[page.NextCursor] || page.NextCursor == cursor {
				return nil, E("invalid_native_result", nil)
			}
			cursors[page.NextCursor] = true
			cursor = page.NextCursor
		}
		sort.Slice(children, func(i, j int) bool { return children[i].Path < children[j].Path })
		entries = append(entries, children...)
	}
	return entries, nil
}
func comparePathFromNative(native NativeActions, p storage.Path) string {
	return comparePath(Root{CaseSensitive: native.StorageNameRules().CaseSensitive}, p)
}
func validateCapture(ctx context.Context, req ActionRequest, native NativeActions, b actionBinding, manifest []ManifestEntry) error {
	for _, old := range append(append([]storage.Entry(nil), b.Anchors...), b.Sources...) {
		e, err := observe(ctx, native, old.Path)
		if err != nil || !sameNative(old, e) {
			return E("target_changed", err)
		}
	}
	if b.Existing != nil {
		e, err := observe(ctx, native, b.Existing.Path)
		if err != nil || !sameNative(*b.Existing, e) {
			return E("target_changed", err)
		}
	}
	if len(b.Sources) > 0 {
		current, err := snapshotActionTree(ctx, native, req.From, req.Kind != "delete" || req.Recursive)
		if err != nil || len(current) != len(b.Sources) {
			return E("target_changed", err)
		}
		for i := range current {
			if !sameNative(current[i], b.Sources[i]) {
				return E("target_changed", nil)
			}
		}
	}
	for _, entry := range manifest {
		if entry.Target != "" {
			if _, err := native.Stat(ctx, entry.Target); err == nil {
				return E("target_changed", nil)
			} else if !storage.IsCode(err, "not_found") {
				return err
			}
		}
	}
	return nil
}
func actionErrorCode(err error) string {
	var own *Error
	if errors.As(err, &own) {
		return own.Code
	}
	var native *storage.Error
	if errors.As(err, &native) {
		return string(native.Code)
	}
	return "storage_failure"
}

// Run resumes only the saved manifest. Each native entry has its own short worker
// lease. An unresolved entry is inspected before further work, even after cancel.
func (a *Actions) Run(ctx context.Context, root Root, actor Actor, id string, native NativeActions) (Operation, error) {
	if native == nil {
		return Operation{}, E("unsupported", nil)
	}
	if err := validateNativeBinding(root, native.StorageBinding(), native.StorageNameRules()); err != nil {
		return Operation{}, err
	}
	if root.RetainsVersions != native.Describe().RetainsVersions {
		return Operation{}, E("root_conflict", nil)
	}
	op, err := a.journal.Get(ctx, root, actor, id)
	if err != nil || op.Terminal() {
		return op, err
	}
	req, err := actionRequest(op)
	if err != nil {
		return op, err
	}
	binding, err := a.journal.ActionBinding(ctx, op)
	if IsCode(err, "manifest_not_frozen") {
		return a.finishInterruptedPrepare(ctx, root, actor, op.ID)
	}
	if err != nil {
		return op, err
	}
	var cause error
	// First repair every uncertain effect. Recovery never depends on creator rights
	// that may have been revoked after its native effect, and never publishes again.
	for _, entry := range op.Entries {
		if entry.State == "started" || entry.State == "outcome_unknown" {
			op, err = a.service.RecoverEntry(ctx, root, actor, id, entry.Number, native)
			if uncertainEntries(&op) {
				return a.accountUnknown(ctx, root, actor, op, err)
			}
			if err != nil {
				if !IsCode(err, "native_not_applied") {
					return op, err
				}
				cause = err
			}
		}
	}
	for _, entry := range op.Entries {
		if entry.State == "not_applied" {
			cause = E("native_not_applied", nil)
		}
	}
	if op.Phase != NativeApplied && cause == nil {
		for _, entry := range op.Entries {
			if entry.State != "pending" {
				continue
			}
			op, err = a.journal.Get(ctx, root, actor, id)
			if err != nil {
				return op, err
			}
			if op.CancelRequested {
				break
			}
			if err = a.options.Authorize(ctx, op); err != nil {
				cause = err
				break
			}
			if err = validateActionStep(ctx, native, req, binding, op, entry); err != nil {
				cause = err
				break
			}
			op, err = a.service.ApplyEntry(ctx, root, actor, id, entry.Number, native, func(work context.Context, intent storage.EffectIntent, save storage.Checkpoint) (storage.MutationResult, error) {
				// Repeat identity checks after the durable entry start and immediately before
				// passing the actual immutable intent to the admitted native executor.
				if checkErr := validateActionStep(work, native, req, binding, op, entry); checkErr != nil {
					return storage.MutationResult{}, checkErr
				}
				return executeAction(work, native, intent, save)
			})
			if uncertainEntries(&op) {
				return a.accountUnknown(ctx, root, actor, op, err)
			}
			if err != nil {
				cause = err
				break
			}
		}
	}
	return a.settle(ctx, root, actor, op, binding, native, cause)
}

func (a *Actions) accountUnknown(ctx context.Context, root Root, actor Actor, op Operation, cause error) (Operation, error) {
	persist, cancel := durableContext(ctx)
	defer cancel()
	current, err := a.journal.Claim(persist, root, actor, op.ID, a.options.LeaseTTL)
	if err != nil {
		return op, errors.Join(cause, E("outcome_unknown", err))
	}
	defer a.journal.Release(persist, current.Lease)
	lease := current.Lease
	for _, entry := range current.Entries {
		if entry.State == "applied" && entry.Action == "copy" && entry.Size > 0 {
			current, err = a.journal.AccountEntry(persist, lease, EntryCharge{Number: entry.Number, LogicalDelta: entry.Size, PhysicalDelta: entry.Size})
			if err != nil {
				return op, errors.Join(cause, E("outcome_unknown", err))
			}
		}
	}
	return current, errors.Join(cause, E("outcome_unknown", nil))
}

// finishInterruptedPrepare never reconstructs a manifest. Claim fences a stale
// preparer before we recheck binding absence: a freeze that won concurrently must
// be handled by a subsequent Run against that immutable manifest instead.
func (a *Actions) finishInterruptedPrepare(ctx context.Context, root Root, actor Actor, id string) (Operation, error) {
	op, err := a.journal.Claim(ctx, root, actor, id, a.options.LeaseTTL)
	if err != nil {
		return op, err
	}
	persist, cancel := durableContext(ctx)
	defer cancel()
	defer a.journal.Release(persist, op.Lease)
	if _, err = a.journal.ActionBinding(persist, op); err == nil {
		return op, E("recovery_required", nil)
	} else if !IsCode(err, "manifest_not_frozen") {
		return op, err
	}
	if op.Phase != Admitted || len(op.Entries) != 0 || op.NativeResult != nil || op.Stage != nil {
		return op, E("recovery_required", nil)
	}
	state := Failed
	if op.CancelRequested {
		state = Cancelled
	}
	if err = a.journal.FinishWithoutEffect(persist, op.Lease, state, problem("prepare_interrupted", nil)); err != nil {
		return op, err
	}
	return a.journal.Get(persist, root, actor, id)
}

func executeAction(ctx context.Context, native NativeActions, intent storage.EffectIntent, save storage.Checkpoint) (storage.MutationResult, error) {
	switch intent.Action {
	case "mkdir":
		return native.Mkdir(ctx, storage.Mutation{ID: intent.ID, Path: intent.Target, Conditions: intent.Conditions}, save)
	case "delete":
		return native.Remove(ctx, storage.Mutation{ID: intent.ID, Path: intent.Source, Conditions: storage.Conditions{ExpectedVersion: intent.SourceVersion}}, save)
	case "copy":
		return native.Copy(ctx, storage.MoveRequest{ID: intent.ID, Source: intent.Source, Target: intent.Target, SourceVersion: intent.SourceVersion, TargetConditions: intent.Conditions}, save)
	default:
		return storage.MutationResult{}, E("invalid_intent", nil)
	}
}
func validateActionStep(ctx context.Context, native NativeActions, req ActionRequest, b actionBinding, op Operation, entry EntryEffect) error {
	for _, old := range b.Anchors {
		current, err := observe(ctx, native, old.Path)
		if entry.Action == "delete" && implicitFolderGone(old, err) {
			continue
		}
		if err != nil || !sameNative(old, current) {
			return E("target_changed", err)
		}
	}
	// Every destination parent is either an immutable captured ancestor or the
	// result identity of a mkdir already applied by this manifest.
	if entry.Target != "" {
		p := parentPath(entry.Target)
		found := false
		for _, anchor := range b.Anchors {
			if anchor.Path == p {
				found = true
			}
		}
		for _, effect := range op.Entries {
			if effect.Action == "mkdir" && effect.Target == p && effect.State == "applied" {
				var result storage.MutationResult
				if json.Unmarshal(effect.Result, &result) != nil || result.Entry == nil {
					return E("invalid_native_result", nil)
				}
				current, err := observe(ctx, native, p)
				if err != nil || !sameNative(*result.Entry, current) {
					return E("target_changed", err)
				}
				found = true
			}
		}
		if !found {
			return E("target_changed", nil)
		}
	}
	if entry.Source != "" {
		for _, ancestor := range b.Sources {
			if ancestor.Kind != storage.Folder || ancestor.Path == entry.Source || !within(string(entry.Source), string(ancestor.Path)) {
				continue
			}
			current, err := observe(ctx, native, ancestor.Path)
			if implicitFolderGone(ancestor, err) {
				continue
			}
			if err != nil || !sameNative(ancestor, current) {
				return E("target_changed", err)
			}
		}
		current, err := observe(ctx, native, entry.Source)
		if entry.SourceEntry != nil && entry.Action == "delete" && implicitFolderGone(*entry.SourceEntry, err) {
			return nil
		}
		if err != nil || entry.SourceEntry == nil || !sameNative(*entry.SourceEntry, current) || !bytes.Equal(current.Version, entry.SourceVersion) {
			return E("target_changed", err)
		}
	}
	if req.Kind == "move" && entry.Action == "delete" {
		// No captured source may be removed until every captured destination has
		// applied, and the entire destination set still matches the proven result.
		for _, effect := range op.Entries {
			if effect.Action == "delete" {
				continue
			}
			if effect.State != "applied" {
				return E("recovery_required", nil)
			}
			var result storage.MutationResult
			if json.Unmarshal(effect.Result, &result) != nil || result.Entry == nil {
				return E("invalid_native_result", nil)
			}
			current, err := observe(ctx, native, effect.Target)
			if err != nil || !sameNative(*result.Entry, current) {
				return E("target_changed", err)
			}
		}
	}
	return nil
}
func actionDeltas(op Operation) (int64, int64, error) {
	var logical, physical int64
	for _, entry := range op.Entries {
		if entry.State != "applied" {
			continue
		}
		if done, err := recordedEntry(entry); !done || err != nil {
			return 0, 0, E("invalid_native_result", err)
		}
		delta := int64(0)
		switch entry.Action {
		case "copy":
			delta = entry.Size
		case "delete":
			delta = -entry.Size
		case "mkdir":
		default:
			return 0, 0, E("invalid_intent", nil)
		}
		var ok bool
		logical, ok = addOK(logical, delta)
		if !ok {
			return 0, 0, E("reservation_exceeded", nil)
		}
		physicalChange := delta
		if entry.Action == "delete" && op.Root.RetainsVersions {
			physicalChange = 0
		}
		physical, ok = addOK(physical, physicalChange)
		if !ok {
			return 0, 0, E("reservation_exceeded", nil)
		}
	}
	return logical, physical, nil
}
func actionPartialResult(op Operation) json.RawMessage {
	effects := make([]map[string]any, 0, len(op.Entries))
	remaining := make([]map[string]any, 0)
	for _, e := range op.Entries {
		item := map[string]any{"number": e.Number, "action": e.Action, "state": e.State}
		if e.Source != "" {
			item["source"] = "/" + string(e.Source)
		}
		if e.Target != "" {
			item["target"] = "/" + string(e.Target)
		}
		if e.State == "applied" {
			effects = append(effects, item)
		} else {
			remaining = append(remaining, item)
		}
	}
	value, _ := json.Marshal(map[string]any{"effects": effects, "remaining": remaining})
	return value
}
func (a *Actions) settle(ctx context.Context, root Root, actor Actor, op Operation, b actionBinding, native NativeActions, cause error) (Operation, error) {
	persist, cancel := durableContext(ctx)
	defer cancel()
	op, err := a.journal.Claim(persist, root, actor, op.ID, a.options.LeaseTTL)
	if err != nil {
		return op, err
	}
	defer a.journal.Release(persist, op.Lease)
	if uncertainEntries(&op) {
		return op, E("outcome_unknown", cause)
	}
	state := Succeeded
	for _, entry := range op.Entries {
		if entry.State != "applied" {
			state = Failed
		}
	}
	if state == Failed && op.CancelRequested {
		state = Cancelled
	}
	if cause != nil && state == Succeeded {
		state = Failed
	}
	logical, physical, err := actionDeltas(op)
	if err != nil {
		return op, err
	}
	var result, p json.RawMessage
	if state != Succeeded {
		result = actionPartialResult(op)
		code := actionErrorCode(cause)
		if op.CancelRequested {
			code = "cancelled"
		}
		p = problem(code, cause)
	}
	var observation *ActionObservation
	if state == Succeeded && op.Kind != "delete" {
		value, observeErr := observeActionResult(persist, native, op, b)
		if observeErr != nil {
			if markErr := a.journal.MarkActionObservationUnknown(persist, op.Lease); markErr != nil {
				return op, errors.Join(observeErr, markErr)
			}
			latest, readErr := a.journal.Get(persist, root, actor, op.ID)
			return latest, errors.Join(E("outcome_unknown", observeErr), readErr)
		}
		observation = &value
	}
	if anyApplied(&op) || b.Existing != nil {
		if err = a.journal.SaveActionApplied(persist, op.Lease, observation); err != nil {
			return op, err
		}
	}
	out, err := a.journal.Settle(persist, op.Lease, Settlement{State: state, LogicalDelta: logical, PhysicalDelta: physical, Result: result, Problem: p}, a.options.Commit)
	if err != nil {
		return op, err
	}
	return out, nil
}
func observeActionResult(ctx context.Context, native NativeActions, op Operation, b actionBinding) (ActionObservation, error) {
	req, err := actionRequest(op)
	if err != nil {
		return ActionObservation{}, err
	}
	var expected *storage.Entry
	if b.Existing != nil {
		expected = b.Existing
	}
	for _, entry := range op.Entries {
		if entry.Target == req.To && entry.State == "applied" {
			var result storage.MutationResult
			if json.Unmarshal(entry.Result, &result) != nil || result.Entry == nil {
				return ActionObservation{}, E("invalid_native_result", nil)
			}
			expected = result.Entry
		}
	}
	if expected == nil {
		return ActionObservation{}, E("invalid_native_result", nil)
	}
	current, err := observe(ctx, native, req.To)
	if err != nil || !sameNative(*expected, current) {
		return ActionObservation{}, E("target_changed", err)
	}
	observation := ActionObservation{Entry: current}
	if current.Kind == storage.Folder {
		cursors := map[string]bool{}
		seen := map[string]bool{}
		cursor := ""
		for pageNumber := 0; ; pageNumber++ {
			if pageNumber >= maxActionPages {
				return observation, E("request_too_large", nil)
			}
			page, err := native.List(ctx, req.To, storage.PageRequest{Limit: 1000, Cursor: cursor})
			if err != nil {
				return observation, err
			}
			if len(page.Entries) > 1000 {
				return observation, E("invalid_native_result", nil)
			}
			for _, child := range page.Entries {
				key := comparePath(op.Root, child.Path)
				if child.Path == "" || parentPath(child.Path) != req.To || seen[key] || requireVersion(child) != nil {
					return observation, E("invalid_native_result", nil)
				}
				seen[key] = true
				observation.Children = append(observation.Children, child)
				if len(observation.Children) > maxActionEntries {
					return observation, E("request_too_large", nil)
				}
			}
			if page.NextCursor == "" {
				break
			}
			if cursors[page.NextCursor] || cursor == page.NextCursor {
				return observation, E("invalid_native_result", nil)
			}
			cursors[page.NextCursor] = true
			cursor = page.NextCursor
		}
		// Capture parent metadata after membership observation and prove it is still
		// the same folder; child additions are outside the exclusive writer model.
		final, err := observe(ctx, native, req.To)
		if err != nil || !sameNative(current, final) {
			return observation, E("target_changed", err)
		}
		observation.Entry = final
	}
	return observation, nil
}

func (a *Actions) Cancel(ctx context.Context, root Root, actor Actor, id string) (Operation, error) {
	return a.journal.RequestCancel(ctx, root, actor, id)
}

// EffectIdentity provides deterministic native intent IDs for auditing without
// exposing private native references in public operation resources.
func EffectIdentity(id string, number int) string { return fmt.Sprintf("%s/entry/%d", id, number) }
