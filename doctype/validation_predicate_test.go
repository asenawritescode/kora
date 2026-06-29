package doctype

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// PredicateGoldenVector represents a single predicate validation test case.
type PredicateGoldenVector struct {
	Predicate string         `json:"predicate"`
	Inputs    map[string]any `json:"inputs"`
	Expected  bool           `json:"expected"`
}

// TestGoldenValidationPredicates evaluates all golden predicate vectors against the Lisp sandbox.
func TestGoldenValidationPredicates(t *testing.T) {
	vectors := loadPredicateGoldenVectors(t)

	sandbox := NewLispSandbox()
	defer sandbox.Close()

	for i, v := range vectors {
		t.Run(fmt.Sprintf("vec_%d", i), func(t *testing.T) {
			result, err := sandbox.Eval(v.Predicate, v.Inputs, nil)
			if err != nil {
				t.Fatalf("Eval(%q) returned error: %v", v.Predicate, err)
			}

			got := predicateResult(result)
			if got != v.Expected {
				t.Errorf("predicate %q with inputs %v: got %v, want %v", v.Predicate, v.Inputs, got, v.Expected)
			}
		})
	}
}

// TestValidateDocument_CrossFieldPredicate tests ValidateDocument with cross-field predicates.
func TestValidateDocument_CrossFieldPredicate(t *testing.T) {
	dt := &DocType{
		Name: "TestEvent",
		Fields: []Field{
			{Fieldname: "start_date", Fieldtype: "Data", Label: "Start Date"},
			{Fieldname: "end_date", Fieldtype: "Data", Label: "End Date"},
			{Fieldname: "total", Fieldtype: "Float", Label: "Total"},
			{Fieldname: "customer", Fieldtype: "Data", Label: "Customer"},
		},
		DocConstraints: []DocConstraint{
			{
				Predicate: "(> end_date start_date)",
				Message:   "End date must be after start date.",
			},
			{
				Predicate: "(> total 0)",
				Message:   "Total must be positive.",
			},
		},
	}

	t.Run("end_date after start_date passes", func(t *testing.T) {
		doc := NewDocument("TestEvent")
		doc.Set("start_date", "2026-06-01")
		doc.Set("end_date", "2026-06-15")
		doc.Set("total", 100.0)

		errs := ValidateDocument(dt, doc, nil, nil)
		for _, e := range errs {
			t.Errorf("unexpected validation error: %v", e)
		}
	})

	t.Run("end_date before start_date fails", func(t *testing.T) {
		doc := NewDocument("TestEvent")
		doc.Set("start_date", "2026-06-15")
		doc.Set("end_date", "2026-06-01")
		doc.Set("total", 100.0)

		errs := ValidateDocument(dt, doc, nil, nil)
		if len(errs) == 0 {
			t.Fatal("expected validation errors, got none")
		}
		found := false
		for _, e := range errs {
			if e.Message == "End date must be after start date." {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected message %q not found in errors: %v", "End date must be after start date.", errs)
		}
	})

	t.Run("negative total fails", func(t *testing.T) {
		doc := NewDocument("TestEvent")
		doc.Set("start_date", "2026-06-01")
		doc.Set("end_date", "2026-06-15")
		doc.Set("total", -50.0)

		errs := ValidateDocument(dt, doc, nil, nil)
		if len(errs) == 0 {
			t.Fatal("expected validation errors, got none")
		}
		found := false
		for _, e := range errs {
			if e.Message == "Total must be positive." {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected message %q not found in errors: %v", "Total must be positive.", errs)
		}
	})

	t.Run("both predicates fail", func(t *testing.T) {
		doc := NewDocument("TestEvent")
		doc.Set("start_date", "2026-06-15")
		doc.Set("end_date", "2026-06-01")
		doc.Set("total", -50.0)

		errs := ValidateDocument(dt, doc, nil, nil)
		if len(errs) < 2 {
			t.Fatalf("expected at least 2 validation errors, got %d: %v", len(errs), errs)
		}
	})
}

// TestValidateDocument_PredicateWithCondition tests predicate evaluation with condition gates.
func TestValidateDocument_PredicateWithCondition(t *testing.T) {
	dt := &DocType{
		Name: "TestOrder",
		Fields: []Field{
			{Fieldname: "type", Fieldtype: "Data", Label: "Type"},
			{Fieldname: "discount", Fieldtype: "Float", Label: "Discount"},
		},
		DocConstraints: []DocConstraint{
			{
				Predicate: "(< discount 50)",
				Condition: "doc.type == \"wholesale\"",
				Message:   "Wholesale discount must be under 50.",
			},
		},
	}

	t.Run("wholesale with discount >= 50 fails", func(t *testing.T) {
		doc := NewDocument("TestOrder")
		doc.Set("type", "wholesale")
		doc.Set("discount", 60.0)

		errs := ValidateDocument(dt, doc, nil, nil)
		if len(errs) == 0 {
			t.Fatal("expected validation errors, got none")
		}
		if errs[0].Message != "Wholesale discount must be under 50." {
			t.Errorf("expected message %q, got %q", "Wholesale discount must be under 50.", errs[0].Message)
		}
	})

	t.Run("retail with discount >= 50 passes (condition not met)", func(t *testing.T) {
		doc := NewDocument("TestOrder")
		doc.Set("type", "retail")
		doc.Set("discount", 60.0)

		errs := ValidateDocument(dt, doc, nil, nil)
		for _, e := range errs {
			t.Errorf("unexpected validation error: %v", e)
		}
	})
}

// loadPredicateGoldenVectors reads and decodes the golden JSON file.
func loadPredicateGoldenVectors(t *testing.T) []PredicateGoldenVector {
	t.Helper()

	data, err := os.ReadFile("testdata/validation_golden.json")
	if err != nil {
		t.Fatalf("reading golden vectors: %v", err)
	}

	var vectors []PredicateGoldenVector
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatalf("parsing golden vectors: %v", err)
	}

	return vectors
}
