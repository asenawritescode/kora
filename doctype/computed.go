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
	"regexp"
	"strconv"
	"strings"

)


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
	needsLisp := true
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

	// Build child tables from Table-type fields AND any expression-referenced tables.
	childTables := make(map[string][]map[string]any)
	for i := range dt.Fields {
		f := &dt.Fields[i]
		if f.Fieldtype == "Table" {
			collectChildTable(doc, f.Fieldname, childTables)
		}
	}
	// Also scan the expression for table names (e.g. (sum "items" "amount"))
	// and include those tables even if not in the doctype fields.
	for _, tableName := range extractTableNames(expr) {
		if _, ok := childTables[tableName]; !ok {
			collectChildTable(doc, tableName, childTables)
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

	// Legacy infix expression — auto-convert to s-expression and evaluate via Lisp.
	if sandbox != nil {
		sExpr := InfixToSExpr(exprStr)
		if sExpr != exprStr {
			return evalComputedLisp(sExpr, dt, doc, sandbox)
		}
	}
	// Last resort: try bare expression in Lisp sandbox.
	if sandbox != nil {
		return evalComputedLisp(exprStr, dt, doc, sandbox)
	}
	return nil, fmt.Errorf("no sandbox available for expression %q", exprStr)
}

// collectChildTable extracts child rows from a document table field.
func collectChildTable(doc *Document, fieldName string, childTables map[string][]map[string]any) {
	children := doc.GetTable(fieldName)
	if len(children) == 0 {
		return
	}
	rows := make([]map[string]any, len(children))
	for j, child := range children {
		rows[j] = child.Fields
	}
	childTables[fieldName] = rows
}

// tableNamePattern matches table names in s-expressions: (sum "table" ...) or (count "table").
var tableNamePattern = regexp.MustCompile(`\((sum|count)\s+"([^"]+)"`)

// extractTableNames extracts table names referenced in a Lisp expression.
func extractTableNames(expr string) []string {
	matches := tableNamePattern.FindAllStringSubmatch(expr, -1)
	var names []string
	seen := make(map[string]bool)
	for _, m := range matches {
		if !seen[m[2]] {
			names = append(names, m[2])
			seen[m[2]] = true
		}
	}
	return names
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
