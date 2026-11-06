package plugin

import (
	"errors"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestQueryInputErrorFromSchema(t *testing.T) {
	schema := `{"type":"object","properties":{"since":{"type":"string","maxLength":4},"n":{"type":"integer"}},"required":["since"],"additionalProperties":false}`
	tests := []struct {
		name      string
		input     string
		wantField string
		wantMsg   string
	}{
		{"missing required", `{}`, "since", "input since is required"},
		{"wrong type", `{"since":"ok","n":"x"}`, "n", "input n is invalid"},
		{"too long", `{"since":"toolong"}`, "since", "input since is invalid"},
		{"unknown field", `{"since":"ok","zzz":1}`, "zzz", "input zzz is not accepted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			compiler := jsonschema.NewCompiler()
			if err := compiler.AddResource("mem://s.json", mustJSON(t, schema)); err != nil {
				t.Fatal(err)
			}
			compiled, err := compiler.Compile("mem://s.json")
			if err != nil {
				t.Fatal(err)
			}
			cause := compiled.Validate(mustJSON(t, tc.input))
			got := newQueryInputError(cause)
			if got.Field != tc.wantField || !strings.Contains(got.Message, tc.wantMsg) {
				t.Fatalf("got %+v, want field %q msg %q", got, tc.wantField, tc.wantMsg)
			}
			if strings.Contains(got.Message, "mem://") {
				t.Fatalf("validator internals leaked: %q", got.Message)
			}
		})
	}
	var qe *QueryInputError
	if !errors.As(newQueryInputError(errors.New("opaque")), &qe) || qe.Message != "query input is invalid" {
		t.Fatalf("opaque cause: %+v", qe)
	}
}

// #408: an operation call names its subject, and its failures are field-level.
func TestOperationInputErrorFromSchema(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("mem://o.json", mustJSON(t, `{"type":"object","properties":{"register":{"type":"string"},"location":{"type":"string"}},"required":["register","location"],"additionalProperties":false}`)); err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile("mem://o.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, input, field, msg string }{
		{"missing required", `{"register":"R"}`, "location", "input location is required"},
		{"unknown field", `{"register":"R","location":"L","x":1}`, "x", "input x is not accepted by this operation"},
	} {
		got := newInputError(compiled.Validate(mustJSON(t, tc.input)), "operation")
		if got.Field != tc.field || got.Message != tc.msg {
			t.Errorf("%s: got %+v, want %q %q", tc.name, got, tc.field, tc.msg)
		}
	}
	if got := newInputError(errors.New("opaque"), "operation"); got.Message != "operation input is invalid" {
		t.Errorf("opaque cause: %+v", got)
	}
}

func mustJSON(t *testing.T, s string) any {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
