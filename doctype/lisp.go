package doctype

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	zygomys "github.com/glycerine/zygomys/v9/zygo"
)

// LispSandbox wraps a Zygomys Lisp environment for safe expression evaluation.
type LispSandbox struct {
	env         *zygomys.Zlisp
	childTables map[string][]map[string]any
	mu          sync.Mutex
}

// NewLispSandbox creates a new sandboxed Lisp environment with builtin functions.
func NewLispSandbox() *LispSandbox {
	env := zygomys.NewZlispSandbox()
	s := &LispSandbox{env: env}
	s.registerBuiltins()
	return s
}

// Close cleans up the Lisp environment.
func (s *LispSandbox) Close() {
	_ = s.env.Close()
}

// toSexp converts a Go value to a Zygomys Sexp.
func toSexp(v any) zygomys.Sexp {
	if v == nil {
		return zygomys.SexpNull
	}
	switch val := v.(type) {
	case float64:
		return &zygomys.SexpFloat{Val: val}
	case float32:
		return &zygomys.SexpFloat{Val: float64(val)}
	case int:
		return &zygomys.SexpInt{Val: int64(val)}
	case int64:
		return &zygomys.SexpInt{Val: val}
	case int32:
		return &zygomys.SexpInt{Val: int64(val)}
	case string:
		return &zygomys.SexpStr{S: val}
	case bool:
		return &zygomys.SexpBool{Val: val}
	default:
		s := fmt.Sprintf("%v", v)
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return &zygomys.SexpFloat{Val: f}
		}
		return &zygomys.SexpStr{S: s}
	}
}

// fromSexp converts a Zygomys Sexp result back to a Go value.
func fromSexp(s zygomys.Sexp) any {
	if s == nil || s == zygomys.SexpNull {
		return nil
	}
	switch v := s.(type) {
	case *zygomys.SexpFloat:
		return v.Val
	case *zygomys.SexpInt:
		return float64(v.Val)
	case *zygomys.SexpUint64:
		return float64(v.Val)
	case *zygomys.SexpBool:
		if v.Val {
			return 1.0
		}
		return 0.0
	case *zygomys.SexpStr:
		if f, err := strconv.ParseFloat(v.S, 64); err == nil {
			return f
		}
		return v.S
	case *zygomys.SexpChar:
		return float64(v.Val)
	case *zygomys.SexpSentinel:
		return nil
	default:
		return v.SexpString(nil)
	}
}

// Eval evaluates a Lisp expression against document values.
// The expression can reference document fields as global variables and use
// builtin functions: sum, count, round, today, datediff, concat.
func (s *LispSandbox) Eval(expr string, doc map[string]any, childTables map[string][]map[string]any) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.childTables = childTables

	// Reset execution state for fresh evaluation. Keeps global scope and builtins.
	s.env.Clear()

	// Inject document fields as global variables.
	for k, v := range doc {
		s.env.AddGlobal(k, toSexp(v))
	}

	result, err := s.env.EvalString(expr)

	// Clean up child table reference.
	s.childTables = nil

	if err != nil {
		return nil, fmt.Errorf("lisp eval error: %w", err)
	}

	return fromSexp(result), nil
}

// registerBuiltins registers domain-specific Lisp functions for computed field evaluation.
func (s *LispSandbox) registerBuiltins() {
	// sum(tableName, fieldName) — sums a field across rows of a child table.
	s.env.AddFunction("sum", func(env *zygomys.Zlisp, name string, args []zygomys.Sexp) (zygomys.Sexp, error) {
		if len(args) < 2 {
			return zygomys.SexpNull, fmt.Errorf("sum requires 2 arguments: table-name field-name")
		}
		tableName, err := sexpToString(args[0])
		if err != nil {
			return zygomys.SexpNull, fmt.Errorf("sum: first arg must be a string: %w", err)
		}
		fieldName, err := sexpToString(args[1])
		if err != nil {
			return zygomys.SexpNull, fmt.Errorf("sum: second arg must be a string: %w", err)
		}

		rows, ok := s.childTables[tableName]
		if !ok {
			return &zygomys.SexpFloat{Val: 0}, nil
		}

		var total float64
		for _, row := range rows {
			if v, exists := row[fieldName]; exists {
				total += lispToFloat64(v)
			}
		}
		return &zygomys.SexpFloat{Val: total}, nil
	})

	// count(tableName) — counts rows in a child table.
	s.env.AddFunction("count", func(env *zygomys.Zlisp, name string, args []zygomys.Sexp) (zygomys.Sexp, error) {
		if len(args) < 1 {
			return zygomys.SexpNull, fmt.Errorf("count requires 1 argument: table-name")
		}
		tableName, err := sexpToString(args[0])
		if err != nil {
			return zygomys.SexpNull, fmt.Errorf("count: first arg must be a string: %w", err)
		}
		rows, ok := s.childTables[tableName]
		if !ok {
			return &zygomys.SexpInt{Val: 0}, nil
		}
		return &zygomys.SexpInt{Val: int64(len(rows))}, nil
	})

	// round(value, decimals) — rounds a float to N decimal places.
	s.env.AddFunction("round", func(env *zygomys.Zlisp, name string, args []zygomys.Sexp) (zygomys.Sexp, error) {
		if len(args) < 2 {
			return zygomys.SexpNull, fmt.Errorf("round requires 2 arguments: value decimals")
		}
		val := sexpToFloat(args[0])
		places := int(sexpToFloat(args[1]))
		pow := math.Pow(10, float64(places))
		rounded := math.Round(val*pow) / pow
		return &zygomys.SexpFloat{Val: rounded}, nil
	})

	// today() — returns current date as YYYY-MM-DD string.
	s.env.AddFunction("today", func(env *zygomys.Zlisp, name string, args []zygomys.Sexp) (zygomys.Sexp, error) {
		now := time.Now()
		return &zygomys.SexpStr{S: now.Format("2006-01-02")}, nil
	})

	// datediff(a, b) — returns days between two dates as int (a - b).
	s.env.AddFunction("datediff", func(env *zygomys.Zlisp, name string, args []zygomys.Sexp) (zygomys.Sexp, error) {
		if len(args) < 2 {
			return zygomys.SexpNull, fmt.Errorf("datediff requires 2 arguments: date1 date2")
		}
		d1Str, err := sexpToString(args[0])
		if err != nil {
			return zygomys.SexpNull, fmt.Errorf("datediff: first arg must be a date string: %w", err)
		}
		d2Str, err := sexpToString(args[1])
		if err != nil {
			return zygomys.SexpNull, fmt.Errorf("datediff: second arg must be a date string: %w", err)
		}

		d1 := lispParseDate(d1Str)
		d2 := lispParseDate(d2Str)
		days := int64(d2.Sub(d1).Hours() / 24)
		return &zygomys.SexpInt{Val: days}, nil
	})

	// concat(args ...) — concatenates strings.
	s.env.AddFunction("concat", func(env *zygomys.Zlisp, name string, args []zygomys.Sexp) (zygomys.Sexp, error) {
		var parts []string
		for _, arg := range args {
			s, err := sexpToString(arg)
			if err != nil {
				parts = append(parts, fmt.Sprintf("%v", arg.SexpString(nil)))
			} else {
				parts = append(parts, s)
			}
		}
		return &zygomys.SexpStr{S: strings.Join(parts, "")}, nil
	})

	// if(condition, then-val, else-val) — a function version of if for use in any context.
	// Zygomys has 'if' as a special form in infix mode, but the sandbox may not include it.
	// This provides a regular function version.
	s.env.AddFunction("if", func(env *zygomys.Zlisp, name string, args []zygomys.Sexp) (zygomys.Sexp, error) {
		if len(args) < 3 {
			return zygomys.SexpNull, fmt.Errorf("if requires 3 arguments: cond then-val else-val")
		}
		if sexpToFloat(args[0]) != 0 {
			return args[1], nil
		}
		return args[2], nil
	})
}

// sexpToString extracts a string value from a Sexp.
func sexpToString(s zygomys.Sexp) (string, error) {
	if s == nil || s == zygomys.SexpNull {
		return "", fmt.Errorf("nil value")
	}
	switch v := s.(type) {
	case *zygomys.SexpStr:
		return v.S, nil
	case *zygomys.SexpSymbol:
		return v.Name(), nil
	default:
		return s.SexpString(nil), nil
	}
}

// sexpToFloat extracts a float64 value from a Sexp.
func sexpToFloat(s zygomys.Sexp) float64 {
	if s == nil || s == zygomys.SexpNull {
		return 0
	}
	switch v := s.(type) {
	case *zygomys.SexpFloat:
		return v.Val
	case *zygomys.SexpInt:
		return float64(v.Val)
	case *zygomys.SexpUint64:
		return float64(v.Val)
	case *zygomys.SexpStr:
		if f, err := strconv.ParseFloat(v.S, 64); err == nil {
			return f
		}
		return 0
	case *zygomys.SexpBool:
		if v.Val {
			return 1
		}
		return 0
	default:
		return 0
	}
}

// toFloat64 converts any value to float64.
func lispToFloat64(v any) float64 {
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
	default:
		return 0
	}
}

// lispParseDate parses a date string using multiple common formats.
func lispParseDate(s string) time.Time {
	formats := []string{
		"2006-01-02",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	}
	for _, layout := range formats {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
