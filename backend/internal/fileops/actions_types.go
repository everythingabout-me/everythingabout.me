package fileops

import (
	"context"
	"encoding/json"

	"everythingabout.me/backend/internal/storage"
)

// NativeActions is an admitted root-bound tree and its actual native operations.
// Public services obtain it through one startup-owned factory, never a handler client.
type NativeActions interface {
	storage.Driver
	storage.BoundStagedWriter
	storage.BoundEffectRecovery
	storage.FolderWriter
	storage.Remover
	storage.NativeMover
	storage.NativeCopier
}
type ActionRequest struct {
	EntryID        string `json:",omitempty"`
	ParentID       string `json:",omitempty"`
	Selection      string `json:",omitempty"`
	Root           Root
	Actor          Actor
	Kind           string
	From           storage.Path
	To             storage.Path
	Parents        bool
	Recursive      bool
	IfRevision     string
	IdempotencyKey string
}
type ActionObservation struct {
	Entry    storage.Entry
	Children []storage.Entry
}

type ActionValidation func(context.Context, NativeActions, ActionRequest) error
type ActionCommit func(context.Context, Operation) (json.RawMessage, error)
