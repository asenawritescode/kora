// Command free-funcs lists package-level functions that validate, test or reshape a bare primitive,
// for example validateMemberEmail(email string) error. Such a function is a method of a missing type:
// name the thing (type memberEmail string) and give it the method (memberEmail(s).validate()).
//
// Constructors such as parseX(string) (T, error) are idiomatic and are not reported.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// reportedVerbs are the function-name prefixes that mark a method in disguise.
var reportedVerbs = []string{"validate", "normalize", "sanitize", "format", "is", "has"}

var primitives = map[string]bool{
	"string": true, "int": true, "int64": true, "float64": true, "bool": true, "[]string": true, "[]byte": true,
}

func main() {
	roots := os.Args[1:]
	if len(roots) == 0 {
		roots = []string{"internal", "contract"}
	}
	var found []string
	for _, root := range roots {
		names, err := scan(root)
		if err != nil {
			fmt.Fprintln(os.Stderr, "free-funcs:", err)
			os.Exit(1)
		}
		found = append(found, names...)
	}
	sort.Strings(found)
	for _, line := range found {
		fmt.Println(line)
	}
}

// scan returns "dir::name" for every reported function under root. An unreadable or unparsable file is
// an error: a lint tool must not skip what it cannot read.
func scan(root string) ([]string, error) {
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
			if fn, ok := decl.(*ast.FuncDecl); ok && isPrimitiveHelper(fn) {
				out = append(out, filepath.ToSlash(filepath.Dir(path))+"::"+fn.Name.Name)
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
		!strings.Contains(slash, "/generated/") && !strings.Contains(slash, "/gen/")
}

// isPrimitiveHelper reports a package-level function whose name starts with a reported verb followed
// by a noun and whose only parameter is a bare primitive.
func isPrimitiveHelper(fn *ast.FuncDecl) bool {
	if fn.Recv != nil || !hasVerbAndNoun(fn.Name.Name) {
		return false
	}
	params := fn.Type.Params.List
	if len(params) != 1 || len(params[0].Names) > 1 {
		return false
	}
	return primitives[typeName(params[0].Type)]
}

// hasVerbAndNoun reports whether name is a reported verb followed by a capitalised word, so isolated
// words such as "is" or "formatter" do not match.
func hasVerbAndNoun(name string) bool {
	for _, verb := range reportedVerbs {
		rest, ok := strings.CutPrefix(name, verb)
		if ok && rest != "" && unicode.IsUpper(rune(rest[0])) {
			return true
		}
	}
	return false
}

func typeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.ArrayType:
		return "[]" + typeName(t.Elt)
	}
	return ""
}
