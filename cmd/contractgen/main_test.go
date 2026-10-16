package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratorDeterministicAndIncludesWireDefinitions(t *testing.T) {
	a, err := generate("../../contract")
	if err != nil {
		t.Fatal(err)
	}
	b, err := generate("../../contract")
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("generation differs across identical inputs")
	}
	if !strings.Contains(string(a), `"EventEnvelope"`) || !strings.Contains(string(a), `"$defs"`) {
		t.Fatal("generated document is missing expected wire definitions")
	}
	if !strings.Contains(string(a), `"x-enums"`) || !strings.Contains(string(a), `"queued"`) {
		t.Fatal("generated document is missing enum values discovered from type markers")
	}
	if !strings.Contains(string(a), `"minimum": 1`) {
		t.Fatal("field validation metadata was not emitted")
	}
	ts, err := generateTypeScript(a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ts), "export interface EventEnvelope") || !strings.Contains(string(ts), "export function decodeContract") || !strings.Contains(string(ts), "unknown") {
		t.Fatal("generated TypeScript types or runtime decoder are missing")
	}
}

func TestGeneratorDiscoversOnlyMarkedRoots(t *testing.T) {
	dir := t.TempDir()
	src := `package contract
//contract:enum
type State string
const ( StateOpen State = "open"; StateClosed State = "closed" )
//contract:wire
type Example struct { State State ` + "`json:\"state\"`" + ` }
type RuntimeOnly struct { Secret string ` + "`json:\"secret\"`" + ` }
`
	if err := os.WriteFile(filepath.Join(dir, "wire.go"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"Example"`) || !strings.Contains(string(got), `"open"`) {
		t.Fatalf("marked contract metadata missing: %s", got)
	}
	if strings.Contains(string(got), `"RuntimeOnly"`) {
		t.Fatalf("unmarked runtime type leaked into schema: %s", got)
	}
}

func TestGeneratorInlinesDefinedScalarTypes(t *testing.T) {
	dir := t.TempDir()
	src := `package contract
type Revision string
//contract:wire
type Subject struct { Revision Revision ` + "`json:\"revision\"`" + ` }
`
	if err := os.WriteFile(filepath.Join(dir, "wire.go"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), `"$ref": "#/$defs/Revision"`) || !strings.Contains(string(got), `"revision": {`) || !strings.Contains(string(got), `"type": "string"`) {
		t.Fatalf("defined scalar was not inlined correctly: %s", got)
	}
}

func TestGeneratorKeepsV1AndV2ArtifactsSeparate(t *testing.T) {
	dir := t.TempDir()
	src := `package contract
//contract:enum:v2
type RequestState string
const RequestPending RequestState = "pending"
//contract:wire
type Legacy struct { Value string ` + "`json:\"value\"`" + ` }
//contract:wire:v2
type Request struct { State RequestState ` + "`json:\"state\"`" + ` }
`
	if err := os.WriteFile(filepath.Join(dir, "wire.go"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	v1, err := generateVersion(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := generateVersion(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(v1), `"Legacy"`) || strings.Contains(string(v1), `"Request"`) {
		t.Fatalf("v1 artifact contains wrong roots: %s", v1)
	}
	if !strings.Contains(string(v2), `"Request"`) || strings.Contains(string(v2), `"Legacy"`) || !strings.Contains(string(v2), `"pending"`) {
		t.Fatalf("v2 artifact contains wrong roots/enums: %s", v2)
	}
}

func TestGeneratorEmitsStringArrayAndByteBounds(t *testing.T) {
	dir := t.TempDir()
	src := `package contract
//contract:wire
type Bounded struct {
  //contract:minLength=1
  //contract:maxLength=160
  Title string ` + "`json:\"title\"`" + `
  //contract:maxBytes=8192
  Message string ` + "`json:\"message\"`" + `
  //contract:maxItems=50
  //contract:itemPattern=^[a-z]+$
  Items []string ` + "`json:\"items\"`" + `
  //contract:pattern=^[a-z]+$
  Slug string ` + "`json:\"slug\"`" + `
  //contract:const=2
  Version uint16 ` + "`json:\"version\"`" + `
}
`
	if err := os.WriteFile(filepath.Join(dir, "wire.go"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"minLength": 1`, `"maxLength": 160`, `"x-max-utf8-bytes": 8192`, `"maxItems": 50`, `"pattern": "^[a-z]+$"`, `"const": 2`} {
		if !strings.Contains(string(got), expected) {
			t.Errorf("schema omitted %s: %s", expected, got)
		}
	}
	if count := strings.Count(string(got), `"pattern": "^[a-z]+$"`); count < 4 {
		t.Errorf("schema should include scalar and array-item patterns in both schema sections, got %d", count)
	}
	ts, err := generateTypeScript(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`schema["x-max-utf8-bytes"]`, `schema.maxLength`, `schema.maxItems`, `schema.pattern`, `new RegExp(schema.pattern)`} {
		if !strings.Contains(string(ts), expected) {
			t.Errorf("runtime validator omitted %s", expected)
		}
	}
}

func TestGeneratorEmitsStrictDiscriminatedUnionAndFiniteMapKeys(t *testing.T) {
	dir := t.TempDir()
	src := `package contract
//contract:enum:v2
type Kind string
const ( KindAction Kind = "action"; KindEnd Kind = "end" )
//contract:enum:v2
type Reply string
const ReplyOK Reply = "ok"
//contract:wire:v2
type Action struct { Name string ` + "`json:\"name\"`" + ` }
//contract:wire:v2
type End struct { Outcome string ` + "`json:\"outcome\"`" + ` }
//contract:wire:v2
//contract:taggedUnion:v2
//contract:discriminator=Kind
//contract:variants=Action,End
type Step struct {
  Kind Kind ` + "`json:\"kind\"`" + `
  Action *Action ` + "`json:\"action,omitempty\"`" + `
  End *End ` + "`json:\"end,omitempty\"`" + `
}
//contract:wire:v2
type Routing struct { Outcomes map[Reply]string ` + "`json:\"outcomes\"`" + ` }
`
	if err := os.WriteFile(filepath.Join(dir, "wire.go"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := generateVersion(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"oneOf"`, `"const": "action"`, `"const": "end"`, `"additionalProperties": false`, `"propertyNames"`} {
		if !strings.Contains(string(got), expected) {
			t.Errorf("schema omitted %s: %s", expected, got)
		}
	}
	ts, err := generateTypeScript(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"kind": "action"`, `"kind": "end"`, `schema.oneOf.filter`, `schema.propertyNames`} {
		if !strings.Contains(string(ts), expected) {
			t.Errorf("TypeScript output omitted %s: %s", expected, ts)
		}
	}
}

func TestGeneratorRejectsAnyAndUnregisteredRawJSON(t *testing.T) {
	for name, src := range map[string]string{
		"any": `package contract
//contract:wire
type Bad struct { Value any ` + "`json:\"value\"`" + ` }
`,
		"raw json without annotation": `package contract
import "encoding/json"
//contract:wire
type Bad struct { Value json.RawMessage ` + "`json:\"value\"`" + ` }
`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "wire.go"), []byte(src), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := generate(dir); err == nil {
				t.Fatal("generation accepted an unbounded field without explicit schema metadata")
			}
		})
	}
}

func TestGeneratorAllowsExplicitRawJSONLists(t *testing.T) {
	dir := t.TempDir()
	src := `package contract
import "encoding/json"
//contract:wire:v2
type Candidate struct {
  //contract:json
  Resources []json.RawMessage ` + "`json:\"resources\"`" + `
}
`
	if err := os.WriteFile(filepath.Join(dir, "wire.go"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := generateVersion(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"resources"`, `"items"`, `"x-open-json-value": true`} {
		if !strings.Contains(string(got), expected) {
			t.Errorf("schema omitted %s: %s", expected, got)
		}
	}
	ts, err := generateTypeScript(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ts), `"resources": Array<unknown>`) {
		t.Errorf("TypeScript did not preserve the JSON list shape: %s", ts)
	}
}

func TestUnsupportedContractTypeFailsInsteadOfEmittingAny(t *testing.T) {
	fset := token.NewFileSet()
	src := "package contract\ntype Bad struct { Value chan string `json:\"value\"` }\n"
	f, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	field := f.Decls[0].(*ast.GenDecl).Specs[0].(*ast.TypeSpec).Type.(*ast.StructType).Fields.List[0].Type
	err = visitType(field, map[string]ast.Expr{}, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "unsupported Go type") {
		t.Fatalf("expected unsupported type error, got %v", err)
	}
}

func TestJSONOptionalAndRequiredTags(t *testing.T) {
	name, omit, skip := parseJSONTag("name,omitempty", "Field")
	if name != "name" || !omit || skip {
		t.Fatalf("unexpected parsed tag: %q %v %v", name, omit, skip)
	}
	name, _, skip = parseJSONTag("-", "Field")
	if name != "" || !skip {
		t.Fatalf("json '-' should be omitted, got %q %v", name, skip)
	}
}
