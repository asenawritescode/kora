package application_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestDomainPackagesDoNotImportOtherDomainInternals protects the application
// package boundary while allowing adapters in the composition layer to join
// domains through their declared ports.
func TestDomainPackagesDoNotImportOtherDomainInternals(t *testing.T) {
	violations, err := domainImportViolations(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range violations {
		t.Error(violation)
	}
}

// TestDomainImportGateRejectsViolationFixture proves the gate itself detects
// a forbidden dependency instead of only passing the current source tree.
func TestDomainImportGateRejectsViolationFixture(t *testing.T) {
	root := t.TempDir()
	for path, source := range map[string]string{
		"sales/order.go":  "package sales\nimport _ \"github.com/asenawritescode/kora/internal/application/finance/private\"\n",
		"finance/book.go": "package finance\n",
	} {
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal("create boundary fixture:", err)
		}
		if err := os.WriteFile(fullPath, []byte(source), 0o600); err != nil {
			t.Fatal("write boundary fixture:", err)
		}
	}
	violations, err := domainImportViolations(root)
	if err != nil {
		t.Fatal("run architecture gate on violation fixture:", err)
	}
	if len(violations) != 1 || !strings.Contains(violations[0], "sales imports private package") || !strings.Contains(violations[0], "/finance/private") {
		t.Fatalf("forbidden dependency fixture produced violations %#v; want one Sales-to-Finance internal edge", violations)
	}
}

func domainImportViolations(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	domains := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != "process" && entry.Name() != "workflow" {
			domains[entry.Name()] = true
		}
	}
	var violations []string
	for domain := range domains {
		err := filepath.WalkDir(filepath.Join(root, domain), func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				spec, ok := node.(*ast.ImportSpec)
				if !ok {
					return true
				}
				importPath := strings.Trim(spec.Path.Value, "\"")
				for other := range domains {
					if other != domain && strings.Contains(importPath, "/internal/application/"+other+"/") {
						violations = append(violations, domain+" imports private package "+importPath)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(violations)
	return violations, nil
}
