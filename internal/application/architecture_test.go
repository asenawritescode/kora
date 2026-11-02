package application_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDomainPackagesDoNotImportOtherDomainInternals keeps application domains
// behind their public package contracts. Engine adapters are composition edges.
func TestDomainPackagesDoNotImportOtherDomainInternals(t *testing.T) {
	root := "."
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	domains := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != "process" && entry.Name() != "workflow" {
			domains[entry.Name()] = true
		}
	}
	for domain := range domains {
		files, err := filepath.Glob(filepath.Join(domain, "**", "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		// filepath.Glob does not recurse ** on every platform; enumerate known package files recursively.
		_ = files
		_ = filepath.WalkDir(filepath.Join(root, domain), func(path string, d os.DirEntry, walkErr error) error {
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
					if other != domain && strings.Contains(importPath, "/internal/application/"+other) {
						t.Errorf("domain %s imports private package %s", domain, importPath)
					}
				}
				return true
			})
			return nil
		})
	}
}
