package kernel

import (
	"strings"
	"testing"
)

const computeCmd = `
name: sell
namespace: shop
version: 1
input:
  record: Sale
compute:
  subtotal: {sum: $input.lines, expr: "qty * rate"}
  line_count: {count: $input.lines}
transaction:
  - create:
      record: Sale
      values:
        total: $computed.subtotal
        lines: $computed.line_count
`

func TestComputeSumIsDecimalExact(t *testing.T) {
	cmd, err := ParseCommandResource([]byte(computeCmd))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		lines []any
		want  map[string]any
	}{
		{"three tenths", []any{
			map[string]any{"qty": 1.0, "rate": 0.1},
			map[string]any{"qty": 1.0, "rate": 0.1},
			map[string]any{"qty": 1.0, "rate": 0.1},
		}, map[string]any{"subtotal": "0.3", "line_count": "3"}},
		{"mixed", []any{
			map[string]any{"qty": 2.0, "rate": "19.99"},
			map[string]any{"qty": 3.0, "rate": 5.0},
		}, map[string]any{"subtotal": "54.98", "line_count": "2"}},
		{"empty", []any{}, map[string]any{"subtotal": "0", "line_count": "0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := evalCompute(cmd.Compute, map[string]any{"lines": tt.lines})
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("%s = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func TestComputeFailsOnBadValues(t *testing.T) {
	cmd, _ := ParseCommandResource([]byte(computeCmd))
	_, err := evalCompute(cmd.Compute, map[string]any{"lines": []any{map[string]any{"qty": "x", "rate": 1.0}}})
	if err == nil || !strings.Contains(err.Error(), `compute "subtotal"`) {
		t.Fatalf("want compute error naming key, got %v", err)
	}
}

func TestComputeValidation(t *testing.T) {
	tests := []struct{ name, compute, step, want string }{
		{"unknown ref", "a: {count: $input.lines}", "$computed.zzz", "must name a compute key"},
		{"both sum and count", "a: {count: $input.lines, sum: $input.lines, expr: qty}", "$computed.a", "exactly one"},
		{"not input list", "a: {count: lines}", "$computed.a", "$input list"},
		{"bad expr", "a: {sum: $input.lines, expr: \"qty *\"}", "$computed.a", "expr invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "name: s\nnamespace: n\nversion: 1\ninput: {record: Sale}\ncompute:\n  " + tt.compute +
				"\ntransaction:\n  - create: {record: Sale, values: {x: \"" + tt.step + "\"}}\n"
			if _, err := ParseCommandResource([]byte(src)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want %q, got %v", tt.want, err)
			}
		})
	}
}
