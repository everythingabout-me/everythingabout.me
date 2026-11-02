// Package browserupload coordinates temporary browser uploads. Only unfinished
// native effects are recovered; successful receipts never poll storage again.
package browserupload

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

type Session struct {
	ID, Binding, OwnerID, Path string
	Size                       int64
	Mime                       string
	ModifiedAt                 int64
	State                      string
	Checkpoint                 json.RawMessage
	Result                     json.RawMessage
	ExpiresAt                  time.Time
	LeaseToken                 string
}

type Inspection struct{ Applied, Terminal bool }

// Stage may receive bytes only into unpublished temporary native state. Each
// allocation identity must be checkpointed before it can need crash cleanup.
// Publish returns true only when native success and quiescence are definitive.
// Inspect never publishes; Terminal asserts no older final request can arrive.
// Discard must remove temporary material only, never the authoritative target.
type Driver interface {
	Stage(context.Context, Session, io.Reader, func(context.Context, json.RawMessage) error) (json.RawMessage, error)
	Publish(context.Context, Session, json.RawMessage) (bool, error)
	Inspect(context.Context, Session) (Inspection, error)
	Discard(context.Context, Session) error
}
type Index func(context.Context, Session) (json.RawMessage, error)

type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string              { return e.Message }
func fail(status int, message string) error { return &Error{status, message} }

// Journal fences every mutation with the current, unexpired random lease token.
// Claim changes no lifecycle state. Workers use Due for bounded candidate lists,
// then Claim to serialize with browser requests and other workers.
type Journal interface {
	Create(context.Context, Session) error
	Get(context.Context, string, string, string) (Session, error)
	Claim(context.Context, string, string, string, time.Duration) (Session, error)
	Renew(context.Context, Session, time.Duration) error
	Save(context.Context, Session, time.Time) error
	Due(context.Context, string, int) ([]Session, error)
	Cleanup(context.Context, string, int) error
	CheckConflict(context.Context, string, string, string, bool) error
}

func validJSON(value json.RawMessage) bool { return len(value) > 0 && json.Valid(value) }
func lifecycleError(state string) error {
	switch state {
	case "aborted":
		return fail(410, "upload expired or was aborted; start a new upload")
	default:
		return fail(409, fmt.Sprintf("upload is %s; retry confirmation", state))
	}
}
