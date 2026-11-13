package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/filechanges"
	fc "everythingabout.me/backend/internal/filecontract"
)

// TestFileAPIContractSchema pins the wire shape of every file-API response the web
// client consumes. web/tests/fileApiContract.test.mjs compares this document with the
// interfaces in web/src/api/client.ts using the TypeScript compiler.
//
// Why this mechanism, rather than a table in docs/api/api.md or a hand-written fixture:
//   - A documentation table is not executable. The drift this guards against (a Go
//     handler field removed or renamed while client.ts still asserted it) is exactly a
//     documentation table that nobody re-reads, so it cannot be the check.
//   - A fixture written by hand on either side is another copy of the same fact. It
//     catches a rename only because a human remembered to edit two places, which is the
//     problem stated again.
//   - This document is *derived* from the Go response types by reflection. A renamed or
//     removed JSON tag changes it, the checked-in copy no longer matches, and the Go test
//     fails before the web suite ever runs. Regenerating the document then leaves
//     client.ts stale, which fails the web test. One fact, two directions, neither side
//     able to move alone.
//
// Regenerate after an intentional API change:
//
//	go test ./internal/api -run TestFileAPIContractSchema -update-file-api-schema
//
// Scope: the resource types listed in fileAPIContractShapes. Request bodies, transfer
// class payloads (an opaque union outside the client executor) and error status codes
// are not part of it; the shared document records field presence, optionality and a
// coarse value kind, not enum members or numeric bounds.

const fileAPIContractPath = "../../../docs/api/file-api.schema.json"

var updateFileAPIContract = flag.Bool("update-file-api-schema", false,
	"rewrite docs/api/file-api.schema.json from the Go response types")

type fileAPIContractField struct {
	Kind     string `json:"kind"`
	Optional bool   `json:"optional"`
}

type fileAPIContractType struct {
	Fields map[string]fileAPIContractField `json:"fields"`
}

type fileAPIContract struct {
	Version int                            `json:"version"`
	Comment string                         `json:"comment"`
	Types   map[string]fileAPIContractType `json:"types"`
}

// fileAPIContractShapes maps each client interface to the Go value whose JSON shape it
// mirrors. Every exported file-API response type the web client declares must be here;
// a new resource type is added to both sides in the same change, and the web test fails
// if the document names a type client.ts does not declare.
var fileAPIContractShapes = []struct {
	Type  string
	Value any
}{
	{"FileAudit", filechanges.Audit{}},
	{"FileAuditActor", filechanges.Actor{}},
	{"FileAuditPage", fc.Collection[filechanges.Audit]{}},
	{"FileCapabilities", filesCapabilitiesResource{}},
	{"FileChange", filesChangeItem{}},
	{"FileChangeFilter", filesChangeFilter{}},
	{"FileChangeSnapshot", filesChangeSnapshot{}},
	{"FileChangeSnapshotPage", filesChangeSnapshotPage{}},
	{"FileChangeTrackingCapabilities", filesChangeTrackingCapabilities{}},
	{"FileChangesPage", filesChangesPage{}},
	{"FileCollection", fc.Collection[fc.Metadata]{}},
	{"FileDownloadGrant", filesDownloadGrantResource{}},
	{"FileDownloadGrantCollection", filesDownloadGrantPage{}},
	{"FileMediaMetadata", filesMediaMetadata{}},
	{"FileOperation", filesOperationResource{}},
	{"FilePrincipal", filesPrincipalResource{}},
	{"FileProblem", fc.Problem{}},
	{"FileProviderPart", filesProviderPart{}},
	{"FileQuota", filesQuota{}},
	{"FileReservations", filesReservations{}},
	{"FileResource", fc.Metadata{}},
	{"FileSignedParts", filesSignedParts{}},
	{"FileSubscription", filesSubscription{}},
	{"FileSubscriptionNotification", filesChangeNotification{}},
	{"FileSyncReport", filesSyncResponse{}},
	{"FileUploadCollection", fc.Collection[*filesUploadSession]{}},
	{"FileUploadCommit", filesUploadCommitResource{}},
	{"FileUploadPartGrant", filesPartGrant{}},
	{"FileUploadPartsPage", filesUploadPartsPage{}},
	{"FileUploadReceipt", filesUploadReceiptResource{}},
	{"FileUploadSession", filesUploadSession{}},
}

var contractTimeType = reflect.TypeOf(time.Time{})

func fileAPIJSONKind(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == contractTimeType {
		// RFC 3339 is a JSON string, not an object.
		return "string"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	case reflect.Interface:
		return "any"
	default:
		return "any"
	}
}

// fileAPIContractShape flattens an embedded struct the way encoding/json does, and keeps
// optionality from the tag's omitempty because that is the one difference a client sees
// as "field may be absent" rather than "field may be null".
func fileAPIContractShape(t reflect.Type) fileAPIContractType {
	fields := map[string]fileAPIContractField{}
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			tag := field.Tag.Get("json")
			if field.Anonymous && tag == "" {
				walk(field.Type)
				continue
			}
			if field.PkgPath != "" || tag == "-" {
				continue
			}
			name, options, _ := strings.Cut(tag, ",")
			if name == "" {
				name = field.Name
			}
			fields[name] = fileAPIContractField{
				Kind:     fileAPIJSONKind(field.Type),
				Optional: strings.Contains(options, "omitempty"),
			}
		}
	}
	walk(t)
	return fileAPIContractType{Fields: fields}
}

func fileAPIContractDocument() fileAPIContract {
	document := fileAPIContract{
		Version: 1,
		Comment: "Generated by backend/internal/api/files_contract_schema_test.go; " +
			"compared with web/src/api/client.ts by web/tests/fileApiContract.test.mjs. " +
			"Do not edit by hand: run go test ./internal/api -run TestFileAPIContractSchema -update-file-api-schema.",
		Types: map[string]fileAPIContractType{},
	}
	for _, shape := range fileAPIContractShapes {
		if _, exists := document.Types[shape.Type]; exists {
			panic("duplicate file API contract shape " + shape.Type)
		}
		document.Types[shape.Type] = fileAPIContractShape(reflect.TypeOf(shape.Value))
	}
	return document
}

func TestFileAPIContractSchema(t *testing.T) {
	document := fileAPIContractDocument()
	for name, shape := range document.Types {
		if len(shape.Fields) == 0 {
			t.Fatalf("%s has no fields; the reflection shape is empty", name)
		}
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	path := filepath.Clean(fileAPIContractPath)
	if *updateFileAPIContract {
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes, %d types)", path, len(encoded), len(document.Types))
		return
	}
	checked, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v; regenerate with -update-file-api-schema", path, err)
	}
	if !bytes.Equal(checked, encoded) {
		t.Fatalf("%s is stale against the Go response types.\n"+
			"A file-API field was renamed, added or removed without updating the web client.\n"+
			"Regenerate with: go test ./internal/api -run TestFileAPIContractSchema -update-file-api-schema\n"+
			"then update web/src/api/client.ts until web/tests/fileApiContract.test.mjs passes.", path)
	}
}
