package doctype

import (
	"context"
	"log/slog"
	"math"
	"os"
	"strings"
	"testing"
)

// parityCase pairs a legacy infix expression with its s-expression equivalent.
type parityCase struct {
	infixExpr   string
	lispExpr    string
	inputs      map[string]any
	childTables map[string][]map[string]any
}

// TestGoldenZygomysParity ensures the Zygomys sandbox produces identical results
// to the legacy expr-lang evaluator for the same computations.
func TestGoldenZygomysParity(t *testing.T) {
	cases := []parityCase{
		// Simple arithmetic.
		{infixExpr: "qty * unit_price", lispExpr: "(* qty unit_price)",
			inputs: map[string]any{"qty": 5.0, "unit_price": 12.5}},
		{infixExpr: "qty * price - discount", lispExpr: "(- (* qty price) discount)",
			inputs: map[string]any{"qty": 10.0, "price": 5.0, "discount": 2.0}},
		{infixExpr: "total / 3", lispExpr: "(/ total 3)",
			inputs: map[string]any{"total": 100.0}},
		{infixExpr: "a + b + c", lispExpr: "(+ a b c)",
			inputs: map[string]any{"a": 10.0, "b": 20.0, "c": 5.0}},

		// SUM aggregation.
		{infixExpr: "SUM(items.amount)", lispExpr: `(sum "items" "amount")`,
			inputs:      map[string]any{},
			childTables: map[string][]map[string]any{"items": {{"amount": 10.0}, {"amount": 20.0}}}},
		{infixExpr: "SUM(items.line_total)", lispExpr: `(sum "items" "line_total")`,
			inputs:      map[string]any{},
			childTables: map[string][]map[string]any{"items": {{"line_total": 20.0}, {"line_total": 60.0}}}},

		// COUNT aggregation.
		{infixExpr: "COUNT(attendees)", lispExpr: `(count "attendees")`,
			inputs:      map[string]any{},
			childTables: map[string][]map[string]any{"attendees": {{"name": "a"}, {"name": "b"}}}},
	}

	for i, c := range cases {
		// --- Legacy path ---
		dt := &DocType{
			Name:   "TestType",
			Fields: []Field{{Fieldname: "result", Fieldtype: "Float", Computed: c.infixExpr}},
		}

		doc := NewDocument("TestType")
		for k, v := range c.inputs {
			doc.Set(k, v)
		}
		for tableName, rows := range c.childTables {
			children := make([]*Document, len(rows))
			for j, row := range rows {
				child := NewDocument("ChildType")
				for ck, cv := range row {
					child.Set(ck, cv)
				}
				children[j] = child
			}
			doc.SetTable(tableName, children)
		}

		err := ComputeFields(dt, doc)
		if err != nil {
			t.Errorf("case %d: legacy ComputeFields(%q) error: %v", i, c.infixExpr, err)
			continue
		}
		legacyResult := doc.GetFloat("result")

		// --- Zygomys path ---
		s := NewLispSandbox()
		lispResult, err := s.Eval(c.lispExpr, c.inputs, c.childTables)
		s.Close()
		if err != nil {
			t.Errorf("case %d: Zygomys Eval(%q) error: %v", i, c.lispExpr, err)
			continue
		}

		lispFloat, ok := lispResult.(float64)
		if !ok {
			t.Errorf("case %d: Zygomys result is %T(%v), expected float64", i, lispResult, lispResult)
			continue
		}

		// --- Compare ---
		if math.Abs(legacyResult-lispFloat) > 1e-9 {
			t.Errorf("case %d: DIVERGENCE — legacy=%v, zygomys=%v (expr: %q vs %q)",
				i, legacyResult, lispFloat, c.infixExpr, c.lispExpr)
		}
	}
}

// TestGoldenZygomysParity_DateFunctions verifies date function parity.
func TestGoldenZygomysParity_DateFunctions(t *testing.T) {
	s := NewLispSandbox()
	defer s.Close()

	// ROUND: both should round identically.
	legacyDT := &DocType{
		Name:   "TestType",
		Fields: []Field{{Fieldname: "result", Fieldtype: "Float", Computed: "ROUND(price * qty, 2)"}},
	}
	legacyDoc := NewDocument("TestType")
	legacyDoc.Set("price", 10.567)
	legacyDoc.Set("qty", 3.0)
	if err := ComputeFields(legacyDT, legacyDoc); err != nil {
		t.Fatal(err)
	}
	legacyRounded := legacyDoc.GetFloat("result")

	lispRounded, err := s.Eval("(round (* price qty) 2)", map[string]any{"price": 10.567, "qty": 3.0}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(legacyRounded-lispRounded.(float64)) > 1e-9 {
		t.Errorf("ROUND divergence: legacy=%v, zygomys=%v", legacyRounded, lispRounded)
	}

	// DATEDIFF: both should compute same day difference.
	legacyDT2 := &DocType{
		Name:   "TestType",
		Fields: []Field{{Fieldname: "result", Fieldtype: "Float", Computed: "DATEDIFF(start_date, end_date)"}},
	}
	legacyDoc2 := NewDocument("TestType")
	legacyDoc2.Set("start_date", "2026-06-29")
	legacyDoc2.Set("end_date", "2026-07-01")
	if err := ComputeFields(legacyDT2, legacyDoc2); err != nil {
		t.Fatal(err)
	}
	legacyDiff := legacyDoc2.GetFloat("result")

	lispDiff, err := s.Eval(`(datediff "2026-06-29" "2026-07-01")`, map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(legacyDiff-lispDiff.(float64)) > 1e-9 {
		t.Errorf("DATEDIFF divergence: legacy=%v, zygomys=%v", legacyDiff, lispDiff)
	}
}

// captureWarnHandler intercepts slog Warn-level messages for test assertions.
type captureWarnHandler struct {
	entries []string
	inner   slog.Handler
}

func (h *captureWarnHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *captureWarnHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		h.entries = append(h.entries, r.Message)
	}
	return h.inner.Handle(ctx, r)
}

func (h *captureWarnHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &captureWarnHandler{inner: h.inner.WithAttrs(attrs), entries: h.entries}
}

func (h *captureWarnHandler) WithGroup(name string) slog.Handler {
	return &captureWarnHandler{inner: h.inner.WithGroup(name), entries: h.entries}
}

// TestDualEvalMode_LogsMismatches verifies that dual mode logs mismatches but
// doesn't break on expressions that produce the same result through both paths.
func TestDualEvalMode_LogsMismatches(t *testing.T) {
	oldMode := DualEvalMode
	DualEvalMode = "dual"
	defer func() { DualEvalMode = oldMode }()

	// Capture slog Warn output.
	handler := &captureWarnHandler{
		entries: nil,
		inner:   slog.NewTextHandler(os.Stderr, nil),
	}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(oldLogger)

	// Case 1: Infix arithmetic — legacy gets 62.5, Lisp returns 0 (expected mismatch).
	dt1 := &DocType{
		Name:   "MismatchType",
		Fields: []Field{{Fieldname: "result", Fieldtype: "Float", Computed: "qty * unit_price"}},
	}
	doc1 := NewDocument("MismatchType")
	doc1.Set("qty", 5.0)
	doc1.Set("unit_price", 12.5)

	err := ComputeFields(dt1, doc1)
	if err != nil {
		t.Fatalf("ComputeFields error on mismatching expression: %v", err)
	}
	got1 := doc1.GetFloat("result")
	if math.Abs(got1-62.5) > 1e-9 {
		t.Errorf("mismatching expression: expected 62.5, got %v", got1)
	}
	// Dual mode must produce a mismatch warning for infix expressions since
	// the Lisp sandbox cannot evaluate bare infix (it returns 0).
	if !hasWarnAboutMismatch(handler.entries) {
		t.Errorf("expected a mismatch warning for infix expression, got: %v", handler.entries)
	}

	// Case 2: Lisp expression — routes directly to Lisp sandbox, no dual comparison.
	handler.entries = nil
	dt2 := &DocType{
		Name:   "LispType",
		Fields: []Field{{Fieldname: "result", Fieldtype: "Float", Computed: "(* qty unit_price)"}},
	}
	doc2 := NewDocument("LispType")
	doc2.Set("qty", 5.0)
	doc2.Set("unit_price", 12.5)

	err = ComputeFields(dt2, doc2)
	if err != nil {
		t.Fatalf("ComputeFields error on lisp expression: %v", err)
	}
	got2 := doc2.GetFloat("result")
	if math.Abs(got2-62.5) > 1e-9 {
		t.Errorf("lisp expression: expected 62.5, got %v", got2)
	}
	// Lisp expressions bypass the dual comparison, so no warning should appear.
	if len(handler.entries) > 0 {
		t.Errorf("expected no warnings for lisp expression (no dual comparison), got %d: %v",
			len(handler.entries), handler.entries)
	}

	// Case 3: Dual mode still works in "legacy" mode (no sandbox created).
	handler.entries = nil
	oldMode2 := DualEvalMode
	DualEvalMode = "legacy"
	dt3 := &DocType{
		Name:   "LegacyType",
		Fields: []Field{{Fieldname: "result", Fieldtype: "Float", Computed: "qty * unit_price"}},
	}
	doc3 := NewDocument("LegacyType")
	doc3.Set("qty", 5.0)
	doc3.Set("unit_price", 12.5)

	err = ComputeFields(dt3, doc3)
	if err != nil {
		t.Fatalf("ComputeFields error in legacy mode: %v", err)
	}
	got3 := doc3.GetFloat("result")
	if math.Abs(got3-62.5) > 1e-9 {
		t.Errorf("legacy mode: expected 62.5, got %v", got3)
	}
	if len(handler.entries) > 0 {
		t.Errorf("expected no warnings in legacy mode, got %d: %v",
			len(handler.entries), handler.entries)
	}
	DualEvalMode = oldMode2

	// Case 4: SUM/COUNT expressions still work correctly in dual mode.
	handler.entries = nil
	dt4 := &DocType{
		Name: "SumType",
		Fields: []Field{
			{Fieldname: "items", Fieldtype: "Table"},
			{Fieldname: "total", Fieldtype: "Float", Computed: "SUM(items.amount)"},
		},
	}
	doc4 := NewDocument("SumType")
	doc4.SetTable("items", []*Document{
		{Fields: map[string]any{"amount": 10.0}},
		{Fields: map[string]any{"amount": 20.0}},
	})

	err = ComputeFields(dt4, doc4)
	if err != nil {
		t.Fatalf("ComputeFields error on SUM expression: %v", err)
	}
	got4 := doc4.GetFloat("total")
	if math.Abs(got4-30.0) > 1e-9 {
		t.Errorf("SUM expression: expected 30.0, got %v", got4)
	}
}

func hasWarnAboutMismatch(entries []string) bool {
	for _, e := range entries {
		if strings.Contains(e, "mismatch") {
			return true
		}
	}
	return false
}
