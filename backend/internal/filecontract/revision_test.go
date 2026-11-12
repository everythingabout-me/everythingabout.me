package filecontract_test

import (
	"net/http"
	"testing"

	fc "everythingabout.me/backend/internal/filecontract"
)

// Overwrite is not a header form: it arrives as a JSON condition, and it names no revision,
// so there is nothing to compare. Both destination states are accepted — that is what makes a
// re-import one admission per file instead of a read, a refusal and a second admission.
func TestOverwriteConditionAcceptsAnyDestination(t *testing.T) {
	existing := "cr_observed"
	for _, test := range []struct {
		name    string
		current *string
	}{{"absent", nil}, {"existing", &existing}} {
		t.Run(test.name, func(t *testing.T) {
			if err := fc.CheckPutCondition(fc.PutCondition{Mode: fc.PutOverwrite}, test.current); err != nil {
				t.Fatalf("an overwrite was refused: %v", err)
			}
		})
	}
	for _, incomplete := range []fc.PutCondition{
		{Mode: fc.PutOverwrite, ContentRevision: "cr_x"},
		{Mode: fc.PutOverwrite, ExplicitCreate: true},
	} {
		assertProblem(t, fc.CheckPutCondition(incomplete, &existing), 400, "invalid_request")
	}
}

func TestPutConditions(t *testing.T) {
	current := "cr_observed"
	tests := []struct {
		name                     string
		header                   http.Header
		current                  *string
		mode                     fc.PutMode
		explicit                 bool
		revision                 string
		parseStatus, checkStatus int
		code                     string
	}{
		{name: "default create absent", mode: fc.PutCreate},
		{name: "default create existing", current: &current, mode: fc.PutCreate, checkStatus: 409, code: "name_conflict"},
		{name: "explicit create absent", header: http.Header{"If-None-Match": {"*"}}, mode: fc.PutCreate, explicit: true},
		{name: "explicit create conflict", header: http.Header{"If-None-Match": {"*"}}, current: &current, mode: fc.PutCreate, explicit: true, checkStatus: 412, code: "precondition_failed"},
		{name: "replace matching", header: http.Header{"If-Match": {`"cr_observed"`}}, current: &current, mode: fc.PutReplace, revision: current},
		{name: "opaque revision no prefix", header: http.Header{"If-Match": {`"opaque/value"`}}, current: ptr("opaque/value"), mode: fc.PutReplace, revision: "opaque/value"},
		{name: "replace stale", header: http.Header{"If-Match": {`"cr_old"`}}, current: &current, mode: fc.PutReplace, revision: "cr_old", checkStatus: 412, code: "precondition_failed"},
		{name: "replace absent", header: http.Header{"If-Match": {`"cr_observed"`}}, mode: fc.PutReplace, revision: current, checkStatus: 412, code: "precondition_failed"},
		{name: "metadata revision does not match content", header: http.Header{"If-Match": {`"nr_observed"`}}, current: &current, mode: fc.PutReplace, revision: "nr_observed", checkStatus: 412, code: "precondition_failed"},
		{name: "weak", header: http.Header{"If-Match": {`W/"cr_observed"`}}, parseStatus: 400},
		{name: "wildcard replacement", header: http.Header{"If-Match": {"*"}}, parseStatus: 400},
		{name: "unquoted", header: http.Header{"If-Match": {"cr_observed"}}, parseStatus: 400},
		{name: "empty tag", header: http.Header{"If-Match": {`""`}}, parseStatus: 400},
		{name: "empty header", header: http.Header{"If-Match": {""}}, parseStatus: 400},
		{name: "header without values", header: http.Header{"If-Match": nil}, parseStatus: 400},
		{name: "validator list", header: http.Header{"If-Match": {`"a", "b"`}}, parseStatus: 400},
		{name: "repeated validators", header: http.Header{"If-Match": {`"a"`, `"b"`}}, parseStatus: 400},
		{name: "both headers", header: http.Header{"If-Match": {`"a"`}, "If-None-Match": {"*"}}, parseStatus: 400},
		{name: "noncreate none-match", header: http.Header{"If-None-Match": {`"a"`}}, parseStatus: 400},
		{name: "repeated create", header: http.Header{"If-None-Match": {"*", "*"}}, parseStatus: 400},
		{name: "tag contains newline", header: http.Header{"If-Match": {"\"a\nb\""}}, parseStatus: 400},
		{name: "tag contains space", header: http.Header{"If-Match": {`"a b"`}}, parseStatus: 400},
		{name: "OWS", header: http.Header{"if-match": {" \t\"cr_observed\"\t"}}, current: &current, mode: fc.PutReplace, revision: current},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			condition, err := fc.ParsePutCondition(test.header)
			parseCode := ""
			if test.parseStatus != 0 {
				parseCode = "invalid_request"
			}
			assertProblem(t, err, test.parseStatus, parseCode)
			if err != nil {
				return
			}
			if condition.Mode != test.mode || condition.ExplicitCreate != test.explicit || condition.ContentRevision != test.revision {
				t.Fatalf("condition = %+v", condition)
			}
			assertProblem(t, fc.CheckPutCondition(condition, test.current), test.checkStatus, test.code)
		})
	}
	for _, condition := range []fc.PutCondition{{}, {Mode: fc.PutReplace}, {Mode: fc.PutReplace, ContentRevision: "cr_x", ExplicitCreate: true}, {Mode: fc.PutCreate, ContentRevision: "cr_x"}, {Mode: fc.PutOverwrite, ContentRevision: "cr_x"}, {Mode: fc.PutOverwrite, ExplicitCreate: true}} {
		assertProblem(t, fc.CheckPutCondition(condition, nil), 400, "invalid_request")
	}
}

func TestMoveAndCopyRequireMetadataRevision(t *testing.T) {
	for _, action := range []string{"move", "copy"} {
		for _, test := range []struct {
			name     string
			from, to fc.Path
			revision string
			status   int
			code     string
		}{
			{"accepted", "/Photos/a", "/Archive/a", "nr_observed", 0, ""},
			{"opaque revision", "/Photos/a", "/Photos/b", "opaque-observed", 0, ""},
			{"missing revision", "/Photos/a", "/Archive/a", "", 428, "precondition_required"},
			{"invalid source", "/Photos/../a", "/Archive/a", "nr_observed", 400, "invalid_path"},
			{"invalid target", "/Photos/a", "/Archive//a", "nr_observed", 400, "invalid_path"},
			{"root source", "/", "/Archive/a", "nr_observed", 400, "invalid_path"},
			{"root target", "/Photos/a", "/", "nr_observed", 400, "invalid_path"},
			{"same path", "/Photos/a", "/Photos/a", "nr_observed", 400, "invalid_path"},
			{"descendant", "/Photos", "/Photos/child", "nr_observed", 400, "invalid_path"},
			{"sibling prefix allowed", "/Photos", "/Photos-old", "nr_observed", 0, ""},
		} {
			t.Run(action+"/"+test.name, func(t *testing.T) {
				var err error
				if action == "move" {
					err = fc.ValidateMove(fc.MoveRequest{From: test.from, To: test.to, IfRevision: test.revision})
				} else {
					err = fc.ValidateCopy(fc.CopyRequest{From: test.from, To: test.to, IfSourceRevision: test.revision})
				}
				assertProblem(t, err, test.status, test.code)
			})
		}
	}
	assertProblem(t, fc.CheckRevision("nr_old", "nr_current"), 412, "precondition_failed")
	assertProblem(t, fc.CheckRevision("nr_current", "nr_current"), 0, "")
	assertProblem(t, fc.CheckRevision("", "nr_current"), 428, "precondition_required")
}

func ptr[T any](value T) *T { return &value }
