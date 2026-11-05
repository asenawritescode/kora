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

func mustJSON(t *testing.T, s string) any {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
