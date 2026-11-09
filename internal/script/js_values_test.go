package script

import (
	"encoding/json"
	"testing"
)

func TestJSValueConvertsDecimalsToNumbers(t *testing.T) {
	in := map[string]any{
		"total": json.Number("12.5"),
		"items": []any{map[string]any{"qty": json.Number("2")}},
		"rows":  []map[string]any{{"rate": json.Number("0.1")}},
		"name":  "SO-1",
	}
	out := jsValue(in).(map[string]any)
	if out["total"] != 12.5 || out["name"] != "SO-1" {
		t.Fatalf("out = %#v", out)
	}
	if out["items"].([]any)[0].(map[string]any)["qty"] != 2.0 {
		t.Fatalf("nested slice = %#v", out["items"])
	}
	if out["rows"].([]any)[0].(map[string]any)["rate"] != 0.1 {
		t.Fatalf("nested rows = %#v", out["rows"])
	}
	if in["total"] != json.Number("12.5") {
		t.Fatal("input map must not be mutated")
	}
	var nilMap map[string]any
	if jsValue(nilMap).(map[string]any) != nil {
		t.Fatal("nil map must stay nil")
	}
}

func TestScriptArithmeticOnDecimalFields(t *testing.T) {
	runner := NewEmbeddedRunner(EmbeddedConfig{PoolSize: 1})
	defer runner.Close()
	res, err := runner.Execute(t.Context(), ExecuteRequest{
		Script:   `return {result: event.doc.a + event.doc.b};`,
		Document: map[string]any{"a": json.Number("0.5"), "b": json.Number("1.25")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != 1.75 {
		t.Fatalf("result = %#v, want 1.75 (numeric addition, not concatenation)", res.Result)
	}
}
