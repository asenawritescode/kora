// Package doctype — computed field expression evaluation.
//
// Deprecation notice (Phase 1 of RFC-0001):
// The expr-lang/expr evaluator and regex preprocessing in this file will be
// replaced by the Zygomys Lisp sandbox (lisp.go) in a future release.
// New computed field functions should be added to lisp.go, not here.
// See docs/rfc-0001-lisp-embedding.md for the full migration plan.
package doctype

import (
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

// DualEvalMode controls how computed expressions are evaluated during the
// transition from expr-lang/expr to Zygomys Lisp.
// "legacy" — use expr-lang/expr only (original behavior)
// "lisp"   — use Zygomys Lisp only
// "dual"   — evaluate both, log mismatches, use legacy result (safe transition)
var DualEvalMode = "dual"

// computedCache holds compiled programs for computed field expressions.
var computedCache = make(map[string]*vm.Program)

// computedScriptHook is set by the ORM before ComputeFields runs.
// It bridges script-based computed fields (@script:name) to the JS runtime.
var computedScriptHook func(doctypeName, scriptName string, doc *Document) (any, error)

// SetComputedScriptHook sets the script hook for computed field evaluation.
// Called by the ORM before Insert/Save to enable script-based computed fields.
func SetComputedScriptHook(hook func(doctypeName, scriptName string, doc *Document) (any, error)) {
	computedScriptHook = hook
}

// sumPattern matches SUM(field.column) — e.g., SUM(items.line_total).
var sumPattern = regexp.MustCompile(`SUM\(\s*(\w+)\.(\w+)\s*\)`)

// roundPattern matches ROUND(expr, N).
var roundPattern = regexp.MustCompile(`ROUND\(\s*(.+?)\s*,\s*(\d+)\s*\)`)

// countPattern matches COUNT(table_field) — e.g., COUNT(attendees).
var countPattern = regexp.MustCompile(`COUNT\(\s*(\w+)\s*\)`)

// datediffPattern matches DATEDIFF(a, b) — e.g., DATEDIFF(due_date, today()).
var datediffPattern = regexp.MustCompile(`DATEDIFF\(\s*([^,]+?)\s*,\s*([^)]+?)\s*\)`)

// cfInfo holds metadata about a computed field for dependency ordering.
type cfInfo struct {
	field        *Field
	expr         string
	hasSum       bool
	hasRound     bool
	hasCount     bool
	hasDateDiff  bool
}

// ComputeFields evaluates all computed fields on a document and sets their values.
func ComputeFields(dt *DocType, doc *Document) error {
	if doc == nil || dt == nil {
		return nil
	}

	var computed []cfInfo
	for i := range dt.Fields {
		f := &dt.Fields[i]
		if f.Computed != "" && f.Fieldtype != "Table" {
			cf := cfInfo{field: f, expr: f.Computed}
			cf.hasSum = sumPattern.MatchString(f.Computed)
			cf.hasRound = roundPattern.MatchString(f.Computed)
			cf.hasCount = countPattern.MatchString(f.Computed)
			cf.hasDateDiff = datediffPattern.MatchString(f.Computed)
			computed = append(computed, cf)
		}
	}

	if len(computed) == 0 {
		return nil
	}

	// Evaluate script-based computed fields (@script:script_name).
	// These are handled by the ComputedHooks function on the Registry.
	for _, cf := range computed {
		if strings.HasPrefix(cf.expr, "@script:") {
			scriptName := strings.TrimPrefix(cf.expr, "@script:")
			if computedScriptHook != nil {
				val, err := computedScriptHook(dt.Name, scriptName, doc)
				if err != nil {
					slog.Warn("script computed field failed", "field", cf.field.Fieldname, "script", scriptName, "error", err)
					continue
				}
				doc.Set(cf.field.Fieldname, val)
			}
		}
	}

	// Filter to expression-based computed fields only.
	exprOnly := filterCF(computed, func(cf cfInfo) bool { return !strings.HasPrefix(cf.expr, "@script:") })
	if len(exprOnly) == 0 {
		return nil
	}

	// Determine whether a Lisp sandbox is needed.
	needsLisp := DualEvalMode != "legacy"
	if !needsLisp {
		for _, cf := range exprOnly {
			if strings.HasPrefix(cf.expr, "(") {
				needsLisp = true
				break
			}
		}
	}

	// Create a reusable sandbox for this ComputeFields call.
	var sandbox *LispSandbox
	if needsLisp {
		sandbox = NewLispSandbox()
		defer sandbox.Close()
	}

	// Evaluate expression-based computed fields in dependency order:
	// 1. SUM/COUNT fields first (aggregate child data)
	// 2. DATEDIFF/ROUND fields (depend on other fields)
	// 3. Simple arithmetic fields last
	passes := [][]cfInfo{
		filterCF(exprOnly, func(cf cfInfo) bool { return (cf.hasSum || cf.hasCount) && !cf.hasRound && !cf.hasDateDiff }),
		filterCF(exprOnly, func(cf cfInfo) bool { return cf.hasRound || cf.hasDateDiff }),
		filterCF(exprOnly, func(cf cfInfo) bool { return !cf.hasSum && !cf.hasCount && !cf.hasRound && !cf.hasDateDiff }),
	}

	for _, pass := range passes {
		for _, cf := range pass {
			val, err := evalComputedWithCtx(cf.expr, dt, doc, sandbox)
			if err != nil {
				slog.Warn("computed field evaluation failed", "field", cf.field.Fieldname, "expr", cf.expr, "error", err)
				continue
			}
			doc.Set(cf.field.Fieldname, val)
		}
	}

	return nil
}

func filterCF(items []cfInfo, fn func(cfInfo) bool) []cfInfo {
	var result []cfInfo
	for _, item := range items {
		if fn(item) {
			result = append(result, item)
		}
	}
	return result
}

// evalComputed evaluates a single computed field expression.
func evalComputed(exprStr string, dt *DocType, doc *Document) (any, error) {
	resolved := exprStr

	// Step 1: Resolve COUNT() calls.
	for _, m := range countPattern.FindAllStringSubmatch(exprStr, -1) {
		tableField := strings.TrimSpace(m[1])
		children := doc.GetTable(tableField)
		resolved = strings.Replace(resolved, m[0], strconv.Itoa(len(children)), 1)
	}

	// Step 2: Resolve SUM() calls.
	for _, m := range sumPattern.FindAllStringSubmatch(exprStr, -1) {
		tableField := m[1]
		column := m[2]
		var sum float64
		children := doc.GetTable(tableField)
		for _, child := range children {
			sum += asFloat64(child.Get(column))
		}
		resolved = strings.Replace(resolved, m[0], strconv.FormatFloat(sum, 'f', -1, 64), 1)
	}

	// Step 3: Resolve DATEDIFF() calls.
	for _, m := range datediffPattern.FindAllStringSubmatch(resolved, -1) {
		left := strings.TrimSpace(m[1])
		right := strings.TrimSpace(m[2])
		leftDate := resolveDate(left, doc)
		rightDate := resolveDate(right, doc)
		days := int(rightDate.Sub(leftDate).Hours() / 24)
		resolved = strings.Replace(resolved, m[0], strconv.Itoa(days), 1)
	}

	// Step 4: Build evaluation environment.
	env := make(map[string]any)
	for k, v := range doc.Fields {
		// Keep table fields as-is (for len()), convert others to float64.
		if doc.GetTable(k) != nil {
			env[k] = v
		} else {
			env[k] = asFloat64(v)
		}
	}
	env["name"] = doc.Name
	env["doc_status"] = float64(doc.DocStatus)

	// Step 5: Handle ROUND().
	for _, m := range roundPattern.FindAllStringSubmatch(resolved, -1) {
		inner := strings.TrimSpace(m[1])
		decimals, _ := strconv.Atoi(m[2])
		innerResult, err := evalSimpleExpr(inner, env)
		if err != nil {
			return nil, fmt.Errorf("evaluating ROUND inner %q: %w", inner, err)
		}
		rounded := roundTo(asFloat64(innerResult), decimals)
		resolved = strings.Replace(resolved, m[0], strconv.FormatFloat(rounded, 'f', -1, 64), 1)
	}

	// Step 6: Final evaluation.
	result, err := evalSimpleExpr(resolved, env)
	if err != nil {
		return nil, fmt.Errorf("evaluating %q: %w", resolved, err)
	}

	return result, nil
}

// resolveDate resolves a date value: field name, today(), or string literal.
func resolveDate(s string, doc *Document) time.Time {
	s = strings.TrimSpace(s)

	// today() function.
	if s == "today()" {
		now := time.Now()
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	}

	// String literal: '2026-01-15' or "2026-01-15".
	if (strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'")) ||
		(strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"")) {
		s = s[1 : len(s)-1]
	}

	// Try parsing as date.
	for _, layout := range []string{"2006-01-02", "2006-01-02T15:04:05Z", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}

	// Field name: look up in document.
	if val := doc.Get(s); val != nil {
		switch v := val.(type) {
		case string:
			for _, layout := range []string{"2006-01-02", "2006-01-02T15:04:05Z", "2006-01-02T15:04:05"} {
				if t, err := time.Parse(layout, v); err == nil {
					return t
				}
			}
		case time.Time:
			return v
		}
		// Try float64 (JSON number as unix timestamp days).
		return time.Time{}
	}

	return time.Time{}
}

// evalSimpleExpr compiles and evaluates a simple arithmetic expression.
func evalSimpleExpr(exprStr string, env map[string]any) (any, error) {
	program, err := compileForComputed(exprStr)
	if err != nil {
		return nil, err
	}
	return expr.Run(program, env)
}

// roundTo rounds a float to N decimal places.
func roundTo(val float64, decimals int) float64 {
	pow := 1.0
	for i := 0; i < decimals; i++ {
		pow *= 10
	}
	return float64(int(val*pow+0.5)) / pow
}

// compileForComputed compiles an expression for computed field evaluation.
func compileForComputed(exprStr string) (*vm.Program, error) {
	if cached, ok := computedCache[exprStr]; ok {
		return cached, nil
	}

	program, err := expr.Compile(exprStr, expr.AsAny())
	if err != nil {
		return nil, err
	}

	computedCache[exprStr] = program
	return program, nil
}

// evalComputedLisp evaluates a computed field expression using the Zygomys Lisp sandbox.
// The sandbox is reused across expressions in the same ComputeFields call.
// expr is the Lisp s-expression or an infix expression (Zygomys supports both).
// dt and doc provide the evaluation context (field values + child tables).
func evalComputedLisp(expr string, dt *DocType, doc *Document, sandbox *LispSandbox) (any, error) {
	if sandbox == nil {
		return nil, fmt.Errorf("no Lisp sandbox available")
	}

	// Build doc fields map, excluding Table-type fields (passed via childTables).
	docFields := make(map[string]any)
	for k, v := range doc.Fields {
		if doc.GetTable(k) != nil {
			continue
		}
		docFields[k] = v
	}
	docFields["name"] = doc.Name
	docFields["doc_status"] = float64(doc.DocStatus)

	// Build child tables from Table-type fields.
	childTables := make(map[string][]map[string]any)
	for i := range dt.Fields {
		f := &dt.Fields[i]
		if f.Fieldtype == "Table" {
			children := doc.GetTable(f.Fieldname)
			if len(children) > 0 {
				rows := make([]map[string]any, len(children))
				for j, child := range children {
					rows[j] = child.Fields
				}
				childTables[f.Fieldname] = rows
			}
		}
	}

	result, err := sandbox.Eval(expr, docFields, childTables)
	if err != nil {
		return nil, fmt.Errorf("lisp eval of %q: %w", expr, err)
	}
	return asFloat64(result), nil
}

// evalComputedWithCtx dispatches computed field evaluation based on DualEvalMode.
func evalComputedWithCtx(exprStr string, dt *DocType, doc *Document, sandbox *LispSandbox) (any, error) {
	// Lisp-syntax expressions (starting with "(") always route to the Lisp sandbox.
	if strings.HasPrefix(exprStr, "(") {
		return evalComputedLisp(exprStr, dt, doc, sandbox)
	}

	// Legacy infix expression.
	switch DualEvalMode {
	case "legacy":
		return evalComputed(exprStr, dt, doc)

	case "lisp":
		return evalComputedLisp(exprStr, dt, doc, sandbox)

	case "dual":
		result, err := evalComputed(exprStr, dt, doc)
		if err != nil {
			return nil, err
		}
		if sandbox != nil {
			if lispResult, lispErr := evalComputedLisp(exprStr, dt, doc, sandbox); lispErr == nil {
				legacyFloat := asFloat64(result)
				lispFloat := asFloat64(lispResult)
				if legacyFloat != lispFloat && !(math.IsNaN(legacyFloat) && math.IsNaN(lispFloat)) {
					slog.Warn("computed field dual eval mismatch",
						"expr", exprStr,
						"legacy", legacyFloat,
						"lisp", lispFloat)
				}
			}
		}
		return result, nil

	default:
		// Unknown mode — fall back to legacy.
		return evalComputed(exprStr, dt, doc)
	}
}

// asFloat64 converts any value to float64 for arithmetic.
func asFloat64(v any) float64 {
	if v == nil {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	case []byte:
		f, _ := strconv.ParseFloat(string(n), 64)
		return f
	default:
		s := fmt.Sprintf("%v", v)
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
}
