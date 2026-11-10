package filecontract

import (
	"net/http"
	"time"
)

const Base = "/api/files"

// Problem is the public failure resource. Detail must be a safe English message,
// never a provider response or an internal native locator.
type Problem struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Status      int    `json:"status"`
	Code        string `json:"code"`
	Detail      string `json:"detail"`
	RequestID   string `json:"requestId"`
	OperationID string `json:"operationId,omitempty"`
	// OperationKind names what holds the path (delete, move, copy, mkdir, upload_commit).
	// A client that must decide whether to wait has to tell a deletion it should respect
	// from the folder creation this same upload just raced, and the id alone cannot say.
	OperationKind string `json:"operationKind,omitempty"`
}

func (p *Problem) Error() string { return p.Code + ": " + p.Detail }

// NewProblem builds a public failure resource. Callers supply a stable code and
// safe detail, then attach a request/operation identity when available.
func NewProblem(status int, code, detail string) *Problem {
	return &Problem{Type: "urn:eam:error:" + code, Title: http.StatusText(status), Status: status, Code: code, Detail: detail}
}

func problem(status int, code, detail string) *Problem { return NewProblem(status, code, detail) }

type Checksum struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}

// Metadata exposes service identity independently of native locators and content
// generations. Nullable file-only fields are nil for folders.
type Metadata struct {
	ID               string     `json:"id"`
	ParentID         *string    `json:"parentId"`
	LastChangedAt    *time.Time `json:"lastChangedAt,omitempty"`
	ChangeToken      string     `json:"changeToken,omitempty"`
	Path             Path       `json:"path"`
	ParentPath       Path       `json:"parentPath"`
	Name             string     `json:"name"`
	Kind             string     `json:"kind"`
	Revision         string     `json:"revision"`
	ContentRevision  *string    `json:"contentRevision"`
	Size             *int64     `json:"size"`
	MediaType        *string    `json:"mediaType"`
	SourceModifiedAt *time.Time `json:"sourceModifiedAt"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	Checksum         *Checksum  `json:"checksum"`
}

// Collection uses a non-nil Items slice for empty results. A nil NextCursor is
// encoded explicitly as null. NewCollection guarantees the empty-array shape.
type Collection[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

func NewCollection[T any](items []T, nextCursor *string) Collection[T] {
	if items == nil {
		items = []T{}
	}
	return Collection[T]{Items: items, NextCursor: nextCursor}
}

type CreateFolderRequest struct {
	Kind    string `json:"kind"`
	Path    Path   `json:"path"`
	Parents bool   `json:"parents,omitempty"`
}

type MoveRequest struct {
	From       Path   `json:"from"`
	To         Path   `json:"to"`
	IfRevision string `json:"ifRevision"`
}

type CopyRequest struct {
	From             Path   `json:"from"`
	To               Path   `json:"to"`
	IfSourceRevision string `json:"ifSourceRevision"`
}

type OperationKind string

const (
	OperationMkdir        OperationKind = "mkdir"
	OperationMove         OperationKind = "move"
	OperationCopy         OperationKind = "copy"
	OperationDelete       OperationKind = "delete"
	OperationUploadCommit OperationKind = "upload_commit"
	OperationThumbnail    OperationKind = "thumbnail"
)

type OperationState string

const (
	OperationQueued         OperationState = "queued"
	OperationRunning        OperationState = "running"
	OperationOutcomeUnknown OperationState = "outcome_unknown"
	OperationSucceeded      OperationState = "succeeded"
	OperationFailed         OperationState = "failed"
	OperationCancelled      OperationState = "cancelled"
)

type Progress struct {
	Completed int64  `json:"completed"`
	Total     *int64 `json:"total"`
	Unit      string `json:"unit"`
}

// OperationResult is a closed union of public result shapes. Unexported marker
// methods prevent accidental provider/service structs from entering this field.
type OperationResult interface{ operationResult() }

func (Metadata) operationResult() {}

type DeleteResult struct {
	Deleted bool `json:"deleted"`
}

func (DeleteResult) operationResult() {}

type ThumbnailResult struct {
	ContentURL      string `json:"contentUrl"`
	MediaType       string `json:"mediaType"`
	Size            int64  `json:"size"`
	ContentRevision string `json:"contentRevision"`
}

func (ThumbnailResult) operationResult() {}

// Operation is a representation, not a durable workflow/state machine. Future
// tree services must add truthful per-entry effect/conflict information as needed.
type Operation struct {
	ID              string          `json:"id"`
	Kind            OperationKind   `json:"kind"`
	State           OperationState  `json:"state"`
	Progress        Progress        `json:"progress"`
	Result          OperationResult `json:"result"`
	Error           *Problem        `json:"error"`
	CancelRequested bool            `json:"cancelRequested"`
	CreatedAt       time.Time       `json:"createdAt"`
	FinishedAt      *time.Time      `json:"finishedAt"`
}
