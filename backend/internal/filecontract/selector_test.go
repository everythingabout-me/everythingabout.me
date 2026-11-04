package filecontract_test

import (
	"encoding/json"
	fc "everythingabout.me/backend/internal/filecontract"
	"testing"
)

func TestIdentitySelectorsRejectAmbiguousTargets(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	for _, wire := range []string{`{"id":"` + id + `","path":"/a"}`, `{"id":"` + id + `","id":"` + id + `"}`, `{"id":""}`, `{"id":"not-a-uuid"}`, `{"path":""}`, `{"path":null}`, `{}`, `null`, `{"path":"/a","unknown":true}`} {
		var s fc.EntrySelector
		if err := json.Unmarshal([]byte(wire), &s); err == nil {
			t.Fatalf("ambiguous selector accepted: %s", wire)
		}
	}
	for _, wire := range []string{`{"id":"` + id + `"}`, `{"path":"/literal%2F+?#"}`, `"/legacy"`} {
		var s fc.EntrySelector
		if err := json.Unmarshal([]byte(wire), &s); err != nil {
			t.Fatalf("selector %s: %v", wire, err)
		}
	}
	for _, query := range []string{"id=" + id + "&path=%2Fa", "id=" + id + "&id=" + id, "path=", "id=", "", "path=%2Fa&path=%2Fb"} {
		if _, err := fc.ParseSelectorQuery(query, false); err == nil {
			t.Fatalf("query accepted: %s", query)
		}
	}
	if s, err := fc.ParseSelectorQuery("path=%2Fliteral%252F%2B%3F%23", false); err != nil || s.Path != "/literal%2F+?#" {
		t.Fatalf("query rewritten: %+v %v", s, err)
	}
}

func TestIdentityDestinationsPreserveNamesAndRejectMixtures(t *testing.T) {
	for _, wire := range []string{`{"path":"/a","name":"a"}`, `{"parent":{"path":"/"},"name":"../a"}`, `{"parent":{"path":"/"},"name":"a/b"}`, `{"parent":{"path":"/"},"name":""}`, `{"parent":{"path":"/"},"name":"a","name":"b"}`, `{"id":"11111111-1111-4111-8111-111111111111"}`} {
		var d fc.Destination
		if json.Unmarshal([]byte(wire), &d) == nil {
			t.Fatalf("destination accepted: %s", wire)
		}
	}
	var d fc.Destination
	if err := json.Unmarshal([]byte(`{"parent":{"path":"/Notes"},"name":" +%2F?# "}`), &d); err != nil || d.Name != " +%2F?# " {
		t.Fatalf("destination rewritten: %+v %v", d, err)
	}
}
