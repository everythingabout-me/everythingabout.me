package nodes

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"everythingabout.me/backend/internal/filechanges"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

type capturedFileState struct {
	metadata  *fc.Metadata
	ancestors []filechanges.Ancestor
}
type fileEffectCapture struct {
	key, source, target string
	subtree             bool
	before              capturedFileState
}

func changeID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func captureFileState(ctx context.Context, tx pgx.Tx, owner, namespace, p string) (capturedFileState, error) {
	var state capturedFileState
	record, err := fileBrowseMetadata(ctx, tx, owner, namespace, p, nil)
	if err != nil {
		return state, err
	}
	if record != nil {
		m, err := IndexedMetadata(namespace, *record)
		if err != nil {
			return state, err
		}
		state.metadata = &m
	}
	// Historical ancestry survives deletion and cross-scope moves. This is depth
	// bounded and never enumerates descendants for an atomic native tree move.
	parent := p
	if parent != "" {
		parent = path.Dir(parent)
		if parent == "." {
			parent = ""
		}
	}
	for depth := 0; depth < 1024; depth++ {
		var id *string
		err := tx.QueryRow(ctx, `select file_tree_resolve($1::uuid,$2)::text`, owner, parent).Scan(&id)
		if err != nil && err != pgx.ErrNoRows {
			return state, err
		}
		if id != nil && *id != "" {
			ap, _ := fc.FromRelative(parent)
			state.ancestors = append(state.ancestors, filechanges.Ancestor{ID: *id, Path: ap})
		}
		if parent == "" {
			break
		}
		parent = path.Dir(parent)
		if parent == "." {
			parent = ""
		}
	}
	return state, nil
}
func fileOperationActor(op fileops.Operation) filechanges.Actor {
	a := op.Attribution
	if a.ID == "" {
		a.ID = op.PrincipalID
	}
	kind := a.Kind
	if kind == "" {
		kind = "owner"
		if strings.HasPrefix(a.ID, "token:") {
			kind = "app"
		}
	}
	scope, _ := fc.FromRelative(string(a.Scope))
	return filechanges.Actor{Kind: kind, PrincipalID: a.ID, GrantID: strings.TrimPrefix(a.ID, "token:"), Label: a.Label, Scope: scope, CanRead: a.CanRead, CanWrite: a.CanWrite, CanChanges: a.CanChanges, CanSubscribe: a.CanSubscribe, CanEvents: a.CanEvents, PolicyGeneration: strconv.FormatInt(a.PolicyVersion, 10)}
}
func captureFileEffects(ctx context.Context, tx pgx.Tx, op fileops.Operation) ([]fileEffectCapture, error) {
	namespace := fileRootNamespace(op.Root)
	var effects []fileEffectCapture
	add := func(key, source, target string, subtree bool) error {
		var exists bool
		if err := tx.QueryRow(ctx, `select exists(select 1 from file_change_effects where owner_id=$1::uuid and effect_key=$2)`, op.Root.OwnerID, key).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		selected := source
		if selected == "" {
			selected = target
		}
		state, err := captureFileState(ctx, tx, op.Root.OwnerID, namespace, selected)
		if err != nil {
			return err
		}
		effects = append(effects, fileEffectCapture{key: key, source: source, target: target, subtree: subtree, before: state})
		return nil
	}
	if op.Kind == "upload_commit" {
		var intent storage.WriteIntent
		if json.Unmarshal(op.Intent, &intent) != nil {
			return nil, fileops.E("invalid_intent", nil)
		}
		if err := add(op.ID+":publication", "", string(intent.Target), false); err != nil {
			return nil, err
		}
		return effects, nil
	}
	paired := map[storage.Path]bool{}
	pairedTargets := map[storage.Path]bool{}
	var moveRequest fileops.ActionRequest
	if op.Kind == "move" {
		if json.Unmarshal(op.Intent, &moveRequest) != nil {
			return nil, fileops.E("invalid_intent", nil)
		}
		for _, e := range op.Entries {
			if e.State == "applied" && (e.Action == "copy" || e.Action == "mkdir") {
				original := storage.Path(string(moveRequest.From) + strings.TrimPrefix(string(e.Target), string(moveRequest.To)))
				pairedTargets[original] = true
			}
		}
		for _, e := range op.Entries {
			if e.State == "applied" && e.Action == "delete" {
				paired[e.Source] = true
			}
		}
	}
	for _, e := range op.Entries {
		if e.State != "applied" {
			continue
		}
		source, target := string(e.Source), string(e.Target)
		if e.Action == "copy" || e.Action == "mkdir" {
			if op.Kind == "move" {
				var req fileops.ActionRequest
				_ = json.Unmarshal(op.Intent, &req)
				original := storage.Path(string(req.From) + strings.TrimPrefix(target, string(req.To)))
				if paired[original] {
					source = string(original)
					paired[original] = false
				} else {
					source = ""
				}
			} else {
				source = ""
			}
		}
		if e.Action == "delete" && op.Kind == "move" {
			// If the same source is paired with a proven destination, the one movement
			// fact describes both effects without a duplicate deletion notification.
			if pairedTargets[e.Source] {
				continue
			}
		}
		subtree := (e.Action == "move" || e.Action == "delete") && e.SourceEntry != nil && e.SourceEntry.Kind == storage.Folder
		if err := add(fmt.Sprintf("%s:entry:%d", op.ID, e.Number), source, target, subtree); err != nil {
			return nil, err
		}
	}
	return effects, nil
}
func settleFileChanges(ctx context.Context, tx pgx.Tx, op fileops.Operation, effects []fileEffectCapture) error {
	if len(effects) == 0 {
		return nil
	}
	if _, err := filechanges.EnsureHead(ctx, tx, op.Root.OwnerID); err != nil {
		return err
	}
	var lockedHead int64
	if err := tx.QueryRow(ctx, `select head_position from file_change_heads where owner_id=$1::uuid for update`, op.Root.OwnerID).Scan(&lockedHead); err != nil {
		return err
	}
	// Native/index writes are complete; subsequent bounded after-state reads take
	// no index write locks, and see preceding committed stream effects.
	type pending struct {
		change filechanges.Change
		ids    []string
	}
	items := make([]pending, 0, len(effects))
	at := time.Now().UTC().Truncate(time.Microsecond)
	var previous *time.Time
	if err := tx.QueryRow(ctx, `select (select last_changed_at from file_change_markers where owner_id=$1::uuid and entry_id=(select id from file_identity_roots where owner_id=$1::uuid))`, op.Root.OwnerID).Scan(&previous); err != nil {
		return err
	}
	if previous != nil && previous.After(at) {
		at = previous.UTC()
	}
	for _, effect := range effects {
		after := capturedFileState{}
		if effect.target != "" {
			var err error
			after, err = captureFileState(ctx, tx, op.Root.OwnerID, fileRootNamespace(op.Root), effect.target)
			if err != nil {
				return err
			}
		}
		before := effect.before
		kind := "created"
		switch {
		case after.metadata == nil && before.metadata != nil:
			kind = "deleted"
		case after.metadata != nil && before.metadata != nil && after.metadata.ID == before.metadata.ID:
			kind = "updated"
			if after.metadata.Path != before.metadata.Path {
				kind = "moved"
			}
		case after.metadata == nil && before.metadata == nil:
			continue
		}
		// A stale destination incarnation was retired, not edited in place.
		if kind == "created" {
			if before.metadata != nil && after.metadata != nil && before.metadata.ID != after.metadata.ID {
				retiredID := before.metadata.ID
				coverage := "entry"
				if before.metadata.Kind == "folder" {
					coverage = "subtree"
				}
				retired := filechanges.Change{ID: changeID(), OwnerID: op.Root.OwnerID, EffectKey: effect.key + ":retired", Kind: "deleted", Coverage: coverage, EntryID: &retiredID, Before: before.metadata, BeforeAncestors: before.ancestors, Actor: filechanges.Actor{Kind: "external"}, OperationID: op.ID, RequestID: op.Attribution.RequestID, Origin: "recovery", ObservedAt: at}
				ids := []string{retiredID}
				for _, a := range before.ancestors {
					ids = append(ids, a.ID)
				}
				for _, a := range before.ancestors {
					row, err := fileBrowseMetadata(ctx, tx, op.Root.OwnerID, fileRootNamespace(op.Root), a.Path.Relative(), nil)
					if err != nil {
						return err
					}
					if row == nil || row.Row.ID != a.ID {
						continue
					}
					m, err := IndexedMetadata(fileRootNamespace(op.Root), *row)
					if err != nil {
						return err
					}
					m.LastChangedAt = &at
					m.ChangeToken = retired.ID
					retired.AffectedFolders = append(retired.AffectedFolders, m)
				}
				items = append(items, pending{change: retired, ids: ids})
			}
			before.metadata = nil
			before.ancestors = nil
		}
		id := changeID()
		coverage := "entry"
		if effect.subtree {
			coverage = "subtree"
		}
		var entryID *string
		if after.metadata != nil {
			v := after.metadata.ID
			entryID = &v
		} else if before.metadata != nil {
			v := before.metadata.ID
			entryID = &v
		}
		if entryID == nil || *entryID == "" {
			continue
		}
		ids := []string{*entryID}
		for _, a := range before.ancestors {
			ids = append(ids, a.ID)
		}
		for _, a := range after.ancestors {
			ids = append(ids, a.ID)
		}
		if after.metadata != nil {
			after.metadata.LastChangedAt = &at
			after.metadata.ChangeToken = id
		}
		change := filechanges.Change{ID: id, OwnerID: op.Root.OwnerID, EffectKey: effect.key, Kind: kind, Coverage: coverage, EntryID: entryID, Before: before.metadata, After: after.metadata, BeforeAncestors: before.ancestors, AfterAncestors: after.ancestors, Actor: fileOperationActor(op), OperationID: op.ID, RequestID: op.Attribution.RequestID, Origin: "api", ObservedAt: at}
		seenFolders := map[string]bool{}
		for _, a := range append(append([]filechanges.Ancestor{}, before.ancestors...), after.ancestors...) {
			if seenFolders[a.ID] {
				continue
			}
			seenFolders[a.ID] = true
			row, err := fileBrowseMetadata(ctx, tx, op.Root.OwnerID, fileRootNamespace(op.Root), a.Path.Relative(), nil)
			if err != nil {
				return err
			}
			if row == nil || row.Row.ID != a.ID {
				continue
			}
			m, err := IndexedMetadata(fileRootNamespace(op.Root), *row)
			if err != nil {
				return err
			}
			m.LastChangedAt = &at
			m.ChangeToken = id
			change.AffectedFolders = append(change.AffectedFolders, m)
		}
		items = append(items, pending{change: change, ids: ids})
	}
	// All index state/ancestry reads above precede the short owner-head lock.
	for _, item := range items {
		position, err := filechanges.Append(ctx, tx, item.change)
		if err != nil {
			return err
		}
		if err = filechanges.MarkChangedWithToken(ctx, tx, op.Root.OwnerID, item.ids, position, at, item.change.ID); err != nil {
			return err
		}
		if item.change.Kind == "moved" && item.change.Coverage == "subtree" {
			if _, err = tx.Exec(ctx, `insert into file_change_namespace_markers(owner_id,entry_id,position,change_token,last_changed_at) values($1::uuid,$2::uuid,$3,$4::uuid,$5)
 on conflict(owner_id,entry_id) do update set position=excluded.position,change_token=excluded.change_token,last_changed_at=greatest(file_change_namespace_markers.last_changed_at,excluded.last_changed_at)
 where excluded.position>file_change_namespace_markers.position`, op.Root.OwnerID, item.change.EntryID, position, item.change.ID, at); err != nil {
				return err
			}
		}
		allowed := true
		p := fc.Path("")
		if item.change.After != nil {
			p = item.change.After.Path
		} else if item.change.Before != nil {
			p = item.change.Before.Path
		}
		audit := filechanges.Audit{OwnerID: op.Root.OwnerID, EffectKey: item.change.EffectKey + ":audit", Action: "file." + item.change.Kind, Outcome: "confirmed", Allowed: &allowed, EntryID: item.change.EntryID, Path: &p, Actor: item.change.Actor, OperationID: op.ID, RequestID: item.change.RequestID, Origin: item.change.Origin, ObservedAt: at}
		if err = filechanges.RecordAudit(ctx, tx, &audit); err != nil {
			return err
		}
	}
	return nil
}

func settledChangeResult(ctx context.Context, tx pgx.Tx, owner string, resource json.RawMessage) (json.RawMessage, error) {
	if len(resource) == 0 {
		return resource, nil
	}
	var m fc.Metadata
	if json.Unmarshal(resource, &m) != nil || m.ID == "" || m.Kind == "" {
		return resource, nil
	}
	var changedAt *time.Time
	var token *string
	err := tx.QueryRow(ctx, `select (select last_changed_at from file_effective_change($1::uuid,$2::uuid)),(select change_token::text from file_effective_change($1::uuid,$2::uuid))`, owner, m.ID).Scan(&changedAt, &token)
	if err != nil {
		return nil, err
	}
	if changedAt != nil {
		v := changedAt.UTC()
		m.LastChangedAt = &v
	}
	if token != nil {
		m.ChangeToken = *token
	}
	return json.Marshal(m)
}

// CommitFileAction and CommitStagedFile keep index, durable changes and recursive
// markers in the journal's existing settlement transaction.
func CommitFileAction(ctx context.Context, tx pgx.Tx, op fileops.Operation, result *storage.MutationResult) (json.RawMessage, error) {
	if _, err := filechanges.BindRoot(ctx, tx, op.Root.OwnerID, fileRootNamespace(op.Root)); err != nil {
		return nil, err
	}
	captured, err := captureFileEffects(ctx, tx, op)
	if err != nil {
		return nil, err
	}
	resource, err := commitFileAction(ctx, tx, op, result)
	if err != nil {
		return nil, err
	}
	if err = settleFileChanges(ctx, tx, op, captured); err != nil {
		return nil, err
	}
	return settledChangeResult(ctx, tx, op.Root.OwnerID, resource)
}
func CommitStagedFile(ctx context.Context, tx pgx.Tx, op fileops.Operation, result *storage.MutationResult) (json.RawMessage, error) {
	if _, err := filechanges.BindRoot(ctx, tx, op.Root.OwnerID, fileRootNamespace(op.Root)); err != nil {
		return nil, err
	}
	captured, err := captureFileEffects(ctx, tx, op)
	if err != nil {
		return nil, err
	}
	resource, err := commitStagedFile(ctx, tx, op, result)
	if err != nil {
		return nil, err
	}
	if err = settleFileChanges(ctx, tx, op, captured); err != nil {
		return nil, err
	}
	return settledChangeResult(ctx, tx, op.Root.OwnerID, resource)
}
