// Command param-count lists functions that take more parameters than the limit, one per line as
// "dir::Name count", where Name is "(*T).Method" for methods.
//
// A function with many inputs hides its call sites: `Register(group, nil, tx, nil, nil, nil, nil)` cannot be
// read or checked by the compiler. Take a context and the required arguments, and pass the rest as one named
// options struct. The receiver is not counted; ctx is; a variadic parameter counts as one.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	limit := flag.Int("max", 5, "largest number of parameters a function may take")
	flag.Parse()
	roots := flag.Args()
	if len(roots) == 0 {
		roots = []string{"internal", "contract", "cmd"}
	}
	var found []string
	for _, root := range roots {
		lines, err := scan(root, *limit)
		if err != nil {
			fmt.Fprintln(os.Stderr, "param-count:", err)
			os.Exit(1)
		}
		found = append(found, lines...)
	}
	sort.Strings(found)
	for _, line := range found {
		fmt.Println(line)
	}
}

// scan returns "dir::name count" for every function under root over the limit. An unreadable or
// unparsable file is an error: a lint tool must not skip what it cannot read.
func scan(root string, limit int) ([]string, error) {
	var out []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !isScanned(path) {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if n := countParams(fn.Type); n > limit {
				out = append(out, fmt.Sprintf("%s::%s %d", filepath.ToSlash(filepath.Dir(path)), funcName(fn), n))
			}
		}
		return nil
	})
	return out, err
}

// isScanned skips tests and generated code.
func isScanned(path string) bool {
	slash := filepath.ToSlash(path)
	return strings.HasSuffix(slash, ".go") && !strings.HasSuffix(slash, "_test.go") &&
		!strings.Contains(slash, "/generated/")
}

// countParams counts declared parameters: `a, b string` is two, an unnamed parameter is one.
func countParams(t *ast.FuncType) int {
	n := 0
	for _, field := range t.Params.List {
		n += max(1, len(field.Names))
	}
	return n
}

// funcName returns "Name" for functions and "(*T).Name" or "T.Name" for methods.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	star := ""
	if p, ok := recv.(*ast.StarExpr); ok {
		star, recv = "*", p.X
	}
	switch r := recv.(type) {
	case *ast.IndexExpr:
		recv = r.X
	case *ast.IndexListExpr:
		recv = r.X
	}
	name := "?"
	if id, ok := recv.(*ast.Ident); ok {
		name = id.Name
	}
	if star != "" {
		return "(*" + name + ")." + fn.Name.Name
	}
	return name + "." + fn.Name.Name
}
