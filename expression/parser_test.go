package expression

import (
	"context"
	"testing"
)

type parserScope map[string]Value

func (s parserScope) Lookup(path string) (Value, bool) { value, ok := s[path]; return value, ok }
func (parserScope) Aggregate(string, string, string) (Value, error) {
	return Value{}, &ErrUnknownFunction{Name: "aggregate must not run"}
}

func TestParseAndEvaluatePureExpression(t *testing.T) {
	parsed, err := Parse(`ROUND(total * 1.2, 2) >= 12 AND approved = TRUE`, 100, 32)
	if err != nil {
		t.Fatal("parse:", err)
	}
	if len(parsed.Deps) != 2 || parsed.Deps[0] != "total" || parsed.Deps[1] != "approved" {
		t.Fatalf("dependencies = %#v", parsed.Deps)
	}
	value, err := NewTreeEvaluator().Eval(context.Background(), parsed.AST, parserScope{
		"total": NumberValue(10), "approved": BoolValue(true),
	})
	if err != nil {
		t.Fatal("evaluate:", err)
	}
	if value.Kind != ValueBool || !value.Bool {
		t.Fatalf("value = %#v, want true", value)
	}
}

func TestParseRejectsIOMechanismsAndMalformedInput(t *testing.T) {
	for _, source := range []string{"SUM(lines.amount) > 5", "OPEN_URL(\"https://example.invalid\")", "amount +", "a < b < c", "1 / 0"} {
		t.Run(source, func(t *testing.T) {
			parsed, err := Parse(source, 100, 32)
			if err != nil {
				return
			}
			if source == "1 / 0" {
				if _, err := NewTreeEvaluator().Eval(context.Background(), parsed.AST, parserScope{}); err == nil {
					t.Fatal("division by zero evaluated successfully")
				}
				return
			}
			if _, err := NewTreeEvaluator().Eval(context.Background(), parsed.AST, parserScope{}); err == nil {
				t.Fatal("invalid/non-pure expression evaluated successfully")
			}
		})
	}
}

func TestParseEnforcesInputNodeAndDepthBounds(t *testing.T) {
	if _, err := Parse("x", 1, 1); err != nil {
		t.Fatalf("single identifier should fit limits: %v", err)
	}
	if _, err := Parse("x + 1", 2, 8); err == nil {
		t.Fatal("node limit was not enforced")
	}
	if _, err := Parse("((((x))))", 100, 2); err == nil {
		t.Fatal("depth limit was not enforced")
	}
	if _, err := Parse("", 100, 20); err == nil {
		t.Fatal("empty expression was accepted")
	}
}
