package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// testClient stands in for cpapi.gen.go: two models, with the tag shapes
// oapi-codegen produces for a required, an optional and a nullable field. `@`
// stands in for a backtick.
const testClient = `package cpapi

type NvmeConnectEntry struct {
	Ip   string @json:"ip"@
	Nqn  string @json:"nqn"@
	NsId *int   @json:"ns-id,omitempty"@
	Tls  *bool  @json:"tls,omitempty"@
}

type VolumeDTO struct {
	Id   string @json:"id"@
	NsId int    @json:"ns_id"@
}
`

// testSpec stands in for shared/openapi.json.
func testSpec() spec {
	var doc spec
	doc.Components.Schemas = map[string]schema{
		"NvmeConnectEntry": {
			Properties: map[string]json.RawMessage{"ip": nil, "nqn": nil, "ns-id": nil, "tls": nil},
			Required:   []string{"ip", "nqn"},
		},
		"VolumeDTO": {
			Properties: map[string]json.RawMessage{"id": nil, "ns_id": nil},
			Required:   []string{"id", "ns_id"},
		},
	}
	doc.Paths = map[string]map[string]json.RawMessage{
		"/api/v2/volumes/{volume_id}/connect": {"get": nil},
	}
	return doc
}

func testStructs(t *testing.T) map[string]*ast.StructType {
	t.Helper()
	src := strings.ReplaceAll(testClient, "@", "`")
	file, err := parser.ParseFile(token.NewFileSet(), "cpapi.gen.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	structs, _ := index(file)
	return structs
}

func TestCompileRejects(t *testing.T) {
	for name, tc := range map[string]struct{ rules, want string }{
		"unknown schema": {
			rules: "NoSuchDTO:\n  id: required\n",
			want:  "not a schema",
		},
		"unknown property": {
			rules: "VolumeDTO:\n  identifier: required\n",
			want:  `has no property "identifier"`,
		},
		"present on an optional property": {
			rules: "NvmeConnectEntry:\n  tls: present\n",
			want:  "is optional in",
		},
		"second block for one schema": {
			rules: "VolumeDTO:\n  id: required\nVolumeDTO:\n  ns_id: present\n",
			want:  "a second VolumeDTO block",
		},
		"variant without a base block": {
			rules: "NvmeConnectEntry@GET /api/v2/volumes/{volume_id}/connect:\n  as: LvolConnectEntry\n  ns-id: required,gt=0\n",
			want:  "has no NvmeConnectEntry block to extend",
		},
		"variant on an unknown path": {
			rules: "NvmeConnectEntry:\n  nqn: required\nNvmeConnectEntry@GET /api/v2/volumes/{volume_id}/attach:\n  as: LvolConnectEntry\n",
			want:  "not a path in",
		},
		"variant on an unknown method": {
			rules: "NvmeConnectEntry:\n  nqn: required\nNvmeConnectEntry@POST /api/v2/volumes/{volume_id}/connect:\n  as: LvolConnectEntry\n",
			want:  "has no POST operation",
		},
		"variant without a name": {
			rules: "NvmeConnectEntry:\n  nqn: required\nNvmeConnectEntry@GET /api/v2/volumes/{volume_id}/connect:\n  ns-id: required,gt=0\n",
			want:  "needs `as:",
		},
		"variant named after a client type": {
			rules: "NvmeConnectEntry:\n  nqn: required\nNvmeConnectEntry@GET /api/v2/volumes/{volume_id}/connect:\n  as: VolumeDTO\n",
			want:  "already a type of the generated client",
		},
		"two variants of one name": {
			rules: "NvmeConnectEntry:\n  nqn: required\n" +
				"NvmeConnectEntry@GET /api/v2/volumes/{volume_id}/connect:\n  as: LvolConnectEntry\n" +
				"NvmeConnectEntry@get /api/v2/volumes/{volume_id}/connect:\n  as: LvolConnectEntry\n",
			want: "already generated for another block",
		},
		"as outside an endpoint-scoped block": {
			rules: "VolumeDTO:\n  as: Nope\n",
			want:  "only for an endpoint-scoped block",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := compile([]byte(tc.rules), testStructs(t), testSpec(), "openapi.json")
			if err == nil {
				t.Fatalf("compiled without error, want one mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestCompileMergesVariant pins the contract a variant rests on: it carries the
// base's rules as well as its own, because rules are registered per Go type and
// a variant is a distinct type that inherits nothing at runtime.
func TestCompileMergesVariant(t *testing.T) {
	const rules = `
NvmeConnectEntry:
  ip: present
  nqn: required,startswith=nqn.
NvmeConnectEntry@GET /api/v2/volumes/{volume_id}/connect:
  as: LvolConnectEntry
  nqn: required,startswith=nqn.2023
  ns-id: required,gt=0
`
	compiled, err := compile([]byte(rules), testStructs(t), testSpec(), "openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled) != 2 {
		t.Fatalf("compiled %d rules, want the base and its variant", len(compiled))
	}

	variant := compiled[1]
	if variant.typ != "LvolConnectEntry" || variant.base != "NvmeConnectEntry" {
		t.Errorf("variant = %+v, want LvolConnectEntry of NvmeConnectEntry", variant)
	}
	if variant.endpoint != "GET /api/v2/volumes/{volume_id}/connect" {
		t.Errorf("endpoint = %q", variant.endpoint)
	}
	// Inherited, overridden and own rules, and the base's required keys.
	if got := variant.tags["Nqn"]; got != "required,startswith=nqn.2023" {
		t.Errorf("Nqn = %q, want the variant's override", got)
	}
	if got := variant.tags["NsId"]; got != "required,gt=0" {
		t.Errorf("NsId = %q, want the variant's own rule", got)
	}
	if len(variant.keys) != 1 || variant.keys[0] != "ip" {
		t.Errorf("keys = %v, want the base's ip", variant.keys)
	}
	// The base keeps its own, looser rules.
	if got := compiled[0].tags["Nqn"]; got != "required,startswith=nqn." {
		t.Errorf("base Nqn = %q, want it untouched by the variant", got)
	}
	if _, ruled := compiled[0].tags["NsId"]; ruled {
		t.Error("the variant's rule leaked onto the base")
	}
}
