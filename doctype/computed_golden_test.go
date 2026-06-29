package doctype

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// GoldenVector represents a single computed expression test case.
type GoldenVector struct {
	Expr     string         `json:"expr"`
	Inputs   map[string]any `json:"inputs"`
	Expected float64        `json:"expected"`
}

// TestGoldenComputedVectors evaluates all golden vectors against ComputeFields.
func TestGoldenComputedVectors(t *testing.T) {
	// Use dual mode to verify both evaluators agree.
	oldMode := DualEvalMode
	DualEvalMode = "dual"
	defer func() { DualEvalMode = oldMode }()

	vectors := loadGoldenVectors(t)

	for i, v := range vectors {
		t.Run(fmt.Sprintf("vec_%d", i), func(t *testing.T) {
			dt := &DocType{
				Name:   "TestType",
				Fields: []Field{
					{Fieldname: "result", Fieldtype: "Float", Computed: v.Expr},
				},
			}

			doc := buildDocFromInputs(v.Inputs)
			err := ComputeFields(dt, doc)
			if err != nil {
				t.Fatalf("ComputeFields(%q) returned error: %v", v.Expr, err)
			}

			got := doc.GetFloat("result")

			// Handle today() dynamically: compute expected from current time.
			var expected float64
			if strings.Contains(v.Expr, "today()") {
				t.Logf("expr %q contains today(), computing expected dynamically", v.Expr)
				expected = computeTodayExpected(v.Expr, v.Inputs)
				t.Logf("dynamic expected: %v, got: %v", expected, got)
				// Larger tolerance for today() to account for execution time variance.
				if math.Abs(got-expected) > 2.0 {
					t.Errorf("expr %q: got %v, want approx %v", v.Expr, got, expected)
				}
			} else {
				expected = v.Expected
				if math.Abs(got-expected) > 1e-9 {
					t.Errorf("expr %q: got %v, want %v", v.Expr, got, expected)
				}
			}
		})
	}
}

// TestExportGoldenVectors validates the golden JSON file structure.
func TestExportGoldenVectors(t *testing.T) {
	vectors := loadGoldenVectors(t)

	if len(vectors) == 0 {
		t.Fatal("golden vectors file is empty")
	}

	seen := make(map[string]bool)
	for _, v := range vectors {
		if v.Expr == "" {
			t.Error("found vector with empty expr")
		}
		if v.Inputs == nil {
			t.Errorf("vector %q has nil inputs", v.Expr)
		}
		if seen[v.Expr] {
			t.Errorf("duplicate expression: %q", v.Expr)
		}
		seen[v.Expr] = true
	}

	t.Logf("validated %d golden vectors", len(vectors))
}

// loadGoldenVectors reads and decodes the golden JSON file.
func loadGoldenVectors(t *testing.T) []GoldenVector {
	t.Helper()

	data, err := os.ReadFile("testdata/computed_golden.json")
	if err != nil {
		t.Fatalf("reading golden vectors: %v", err)
	}

	var vectors []GoldenVector
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatalf("parsing golden vectors: %v", err)
	}

	return vectors
}

// buildDocFromInputs converts golden JSON inputs into a Document.
func buildDocFromInputs(inputs map[string]any) *Document {
	doc := NewDocument("TestType")

	for k, v := range inputs {
		switch arr := v.(type) {
		case []any:
			// Child table rows.
			children := make([]*Document, len(arr))
			for i, item := range arr {
				child := NewDocument("ChildType")
				if m, ok := item.(map[string]any); ok {
					for ck, cv := range m {
						child.Set(ck, cv)
					}
				}
				children[i] = child
			}
			doc.SetTable(k, children)
		default:
			doc.Set(k, v)
		}
	}

	return doc
}

// computeTodayExpected computes the expected DATEDIFF value for today() expressions.
// Supports DATEDIFF(today(), due_date) and DATEDIFF(due_date, today()).
func computeTodayExpected(expr string, inputs map[string]any) float64 {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	// DATEDIFF(today(), due_date) — due_date is the second argument.
	if strings.Contains(expr, "today(),") {
		if dueDateVal, ok := inputs["due_date"]; ok {
			if dueDateStr, ok := dueDateVal.(string); ok {
				dueDate, err := time.Parse("2006-01-02", dueDateStr)
				if err == nil {
					// DATEDIFF(a, b) = int(b - a) in days
					// a = today(), b = due_date
					days := int(dueDate.Sub(today).Hours() / 24)
					return float64(days)
				}
			}
		}
	}

	// DATEDIFF(due_date, today()) — due_date is in inputs["due_date"].
	if dueDateVal, ok := inputs["due_date"]; ok {
		if dueDateStr, ok := dueDateVal.(string); ok {
			dueDate, err := time.Parse("2006-01-02T15:04:05Z", dueDateStr)
			if err != nil {
				dueDate, err = time.Parse("2006-01-02", dueDateStr)
			}
			if err == nil {
				// DATEDIFF(a, b) = int(b - a) in days
				// a = due_date, b = today()
				days := int(today.Sub(dueDate).Hours() / 24)
				return float64(days)
			}
		}
	}

	return 0
}
