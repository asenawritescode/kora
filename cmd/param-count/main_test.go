package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestCountParamsAndName(t *testing.T) {
	tests := []struct {
		name     string
		decl     string
		wantName string
		wantN    int
	}{
		{"none", "func F() {}", "F", 0},
		{"grouped names count separately", "func F(a, b string, c int) {}", "F", 3},
		{"unnamed parameters count once each", "func F(string, int) {}", "F", 2},
		{"variadic counts as one", "func F(ctx context.Context, opts ...int) {}", "F", 2},
		{"receiver is not counted", "func (s *S) M(a, b int) {}", "(*S).M", 2},
		{"value receiver", "func (s S) M() {}", "S.M", 0},
		{"generic receiver", "func (s *S[K, V]) M(k K) {}", "(*S).M", 1},
		{"unnamed receiver", "func (*S) M(a int) {}", "(*S).M", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\n"+tt.decl, 0)
			if err != nil {
				t.Fatal(err)
			}
			fn := file.Decls[0].(*ast.FuncDecl)
			if got := countParams(fn.Type); got != tt.wantN {
				t.Errorf("countParams = %d, want %d", got, tt.wantN)
			}
			if got := funcName(fn); got != tt.wantName {
				t.Errorf("funcName = %q, want %q", got, tt.wantName)
			}
		})
	}
}

func TestIsScanned(t *testing.T) {
	tests := map[string]bool{
		"internal/api/router.go":         true,
		"internal/api/router_test.go":    false,
		"contract/generated/engine.go":   false,
		"internal/api/testdata/notgo.md": false,
	}
	for path, want := range tests {
		if got := isScanned(path); got != want {
			t.Errorf("isScanned(%q) = %v, want %v", path, got, want)
		}
	}
}
