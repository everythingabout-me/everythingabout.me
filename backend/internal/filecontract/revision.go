package filecontract

import (
	"net/http"
	"strings"
)

type PutMode string

const (
	PutCreate  PutMode = "create"
	PutReplace PutMode = "replace"
)

// PutCondition distinguishes default create conflicts (409) from explicit
// If-None-Match: * conflicts (412). ContentRevision is unquoted and opaque.
type PutCondition struct {
	Mode            PutMode
	ExplicitCreate  bool
	ContentRevision string
}

// ParsePutCondition accepts the file API's narrow write contract: default create,
// explicit create-only, or one strong content ETag. It intentionally does not
// accept general HTTP validator lists, weak tags, or wildcard replacement.
func ParsePutCondition(header http.Header) (PutCondition, error) {
	matches, hasMatch := headerValues(header, "If-Match")
	none, hasNone := headerValues(header, "If-None-Match")
	invalid := func() (PutCondition, error) {
		return PutCondition{}, problem(http.StatusBadRequest, "invalid_request", "Use either If-None-Match: * or one strong If-Match content revision.")
	}
	if hasMatch && hasNone {
		return invalid()
	}
	if hasNone {
		if len(none) != 1 || strings.Trim(none[0], " \t") != "*" {
			return invalid()
		}
		return PutCondition{Mode: PutCreate, ExplicitCreate: true}, nil
	}
	if hasMatch {
		if len(matches) != 1 {
			return invalid()
		}
		tag := strings.Trim(matches[0], " \t")
		if len(tag) < 3 || tag[0] != '"' || tag[len(tag)-1] != '"' {
			return invalid()
		}
		revision := tag[1 : len(tag)-1]
		for i := 0; i < len(revision); i++ {
			b := revision[i]
			if b < 0x21 || b == '"' || b == 0x7f {
				return invalid()
			}
		}
		return PutCondition{Mode: PutReplace, ContentRevision: revision}, nil
	}
	return PutCondition{Mode: PutCreate}, nil
}

// CheckPutCondition evaluates a proven current generation. A nil revision means
// absent. The caller must enforce this same condition at native publication using
// proven native support or an admitted coordinated writer model. This helper
// cannot turn a Stat-then-write sequence into a safe conditional mutation.
func CheckPutCondition(condition PutCondition, currentRevision *string) error {
	switch condition.Mode {
	case PutCreate:
		if condition.ContentRevision != "" {
			return problem(http.StatusBadRequest, "invalid_request", "A create condition cannot include a content revision.")
		}
		if currentRevision == nil {
			return nil
		}
		if condition.ExplicitCreate {
			return problem(http.StatusPreconditionFailed, "precondition_failed", "The destination already exists.")
		}
		return problem(http.StatusConflict, "name_conflict", "The destination already exists.")
	case PutReplace:
		if condition.ExplicitCreate || condition.ContentRevision == "" {
			return problem(http.StatusBadRequest, "invalid_request", "Replacement requires a content revision.")
		}
		if currentRevision == nil || *currentRevision != condition.ContentRevision {
			return problem(http.StatusPreconditionFailed, "precondition_failed", "The file changed before publication.")
		}
		return nil
	default:
		return problem(http.StatusBadRequest, "invalid_request", "The write condition is invalid.")
	}
}

func ValidateMove(request MoveRequest) error {
	if err := validateNamespacePaths(request.From, request.To); err != nil {
		return err
	}
	return requireRevision(request.IfRevision)
}

func ValidateCopy(request CopyRequest) error {
	if err := validateNamespacePaths(request.From, request.To); err != nil {
		return err
	}
	return requireRevision(request.IfSourceRevision)
}

// Native case/normalization semantics and both-path authorization must additionally
// be checked by the service. This validates exact-spelling ancestry only.
func validateNamespacePaths(from, to Path) error {
	if err := ValidatePath(string(from)); err != nil {
		return err
	}
	if err := ValidatePath(string(to)); err != nil {
		return err
	}
	if from == "/" || to == "/" || WithinScope(from, to, nil) {
		return problem(http.StatusBadRequest, "invalid_path", "Root and same-path or descendant namespace targets are not allowed.")
	}
	return nil
}

func requireRevision(revision string) error {
	if revision == "" {
		return problem(http.StatusPreconditionRequired, "precondition_required", "The observed source metadata revision is required.")
	}
	// Revisions are opaque; do not impose an nr_ prefix or attempt decoding.
	if strings.ContainsAny(revision, "\r\n\x00") {
		return problem(http.StatusBadRequest, "invalid_request", "The source revision is invalid.")
	}
	return nil
}

// CheckRevision checks an opaque metadata revision after a required move/copy
// condition has been parsed. It is separate from content ETag matching.
func CheckRevision(expected, current string) error {
	if err := requireRevision(expected); err != nil {
		return err
	}
	if expected != current {
		return problem(http.StatusPreconditionFailed, "precondition_failed", "The source metadata changed before the operation.")
	}
	return nil
}
