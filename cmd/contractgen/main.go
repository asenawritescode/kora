// Command contractgen produces a deterministic JSON Schema snapshot for the
// stable wire types in contract. Unsupported Go fields fail generation rather
// than being weakened to an unconstrained JSON value.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	schemaOutputPath     = "contract/schema/engine-v1.json"
	typescriptOutputPath = "contract/generated/engine-v1.ts"
	v2SchemaOutputPath   = "contract/schema/engine-v2.json"
	v2TypeScriptPath     = "contract/generated/engine-v2.ts"
)

type schema map[string]any

func main() {
	write := flag.Bool("write", false, "write generated contracts")
	check := flag.Bool("check", false, "check generated contracts are current")
	flag.Parse()
	if *write == *check {
		fatal("choose exactly one of -write or -check")
	}
	v1Data, err := generateVersion("contract", 1)
	if err != nil {
		fatal(err.Error())
	}
	v1Validator, err := generateTypeScript(v1Data)
	if err != nil {
		fatal(err.Error())
	}
	v2Data, err := generateVersion("contract", 2)
	if err != nil {
		fatal(err.Error())
	}
	v2Validator, err := generateTypeScript(v2Data)
	if err != nil {
		fatal(err.Error())
	}
	artifacts := map[string][]byte{
		schemaOutputPath: v1Data, typescriptOutputPath: v1Validator,
		v2SchemaOutputPath: v2Data, v2TypeScriptPath: v2Validator,
	}
	if *write {
		for path, contents := range artifacts {
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				fatal(err.Error())
			}
			if err := os.WriteFile(path, contents, 0644); err != nil {
				fatal(err.Error())
			}
		}
		return
	}
	for path, expected := range artifacts {
		current, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			fatal("generated artifact missing; run go run ./cmd/contractgen -write")
		}
		if !bytes.Equal(current, expected) {
			fatal("generated contract drift in " + path + "; run go run ./cmd/contractgen -write")
		}
	}
}

func fatal(message string) { fmt.Fprintln(os.Stderr, "contractgen:", message); os.Exit(1) }

func generate(dir string) ([]byte, error) {
	return generateVersion(dir, 1)
}

func generateVersion(dir string, version int) ([]byte, error) {
	if version != 1 && version != 2 {
		return nil, fmt.Errorf("unsupported contract version %d", version)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	defs := map[string]ast.Expr{}
	wireRoots := []string{}
	enumRoots := []string{}
	taggedUnions := map[string]taggedUnionSpec{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		for _, decl := range parsed.Decls {
			g, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range g.Specs {
				switch x := spec.(type) {
				case *ast.TypeSpec:
					if x.Assign.IsValid() {
						continue
					}
					defs[x.Name.Name] = x.Type
					doc := ""
					if g.Doc != nil {
						doc += rawComments(g.Doc)
					}
					if x.Doc != nil {
						doc += rawComments(x.Doc)
					}
					if hasVersionMarker(doc, "wire", version) {
						wireRoots = append(wireRoots, x.Name.Name)
					}
					if hasVersionMarker(doc, "enum", version) {
						enumRoots = append(enumRoots, x.Name.Name)
					}
					if hasVersionMarker(doc, "taggedUnion", version) {
						discriminator := commentDirective(doc, "contract:discriminator=")
						variants := strings.Split(commentDirective(doc, "contract:variants="), ",")
						if discriminator == "" || len(variants) < 2 {
							return nil, fmt.Errorf("tagged union %s requires discriminator and at least two variants", x.Name.Name)
						}
						for i := range variants {
							variants[i] = strings.TrimSpace(variants[i])
							if variants[i] == "" {
								return nil, fmt.Errorf("tagged union %s has an empty variant", x.Name.Name)
							}
						}
						taggedUnions[x.Name.Name] = taggedUnionSpec{Discriminator: discriminator, Variants: variants}
					}
				case *ast.ValueSpec:
					for i, name := range x.Names {
						if i < len(x.Values) {
							defs[name.Name] = x.Values[i]
						}
					}
				}
			}
		}
	}
	enums := map[string][]string{}
	sort.Strings(enumRoots)
	for _, root := range enumRoots {
		values, err := enumValues(files, fset, root)
		if err != nil {
			return nil, err
		}
		if len(values) > 0 {
			enums[root] = values
		}
	}
	wireTypes := map[string]bool{}
	var include func(string) error
	include = func(name string) error {
		if wireTypes[name] {
			return nil
		}
		expr, ok := defs[name]
		if !ok {
			return fmt.Errorf("wire type %s is undefined", name)
		}
		wireTypes[name] = true
		st, ok := expr.(*ast.StructType)
		if !ok {
			return nil
		}
		for _, field := range st.Fields.List {
			if isRawJSON(field.Type) {
				if !hasDirective(field, "contract:json") {
					return fmt.Errorf("%s.%s: json.RawMessage requires //contract:json", name, fieldName(field))
				}
				continue
			}
			if err := visitType(field.Type, defs, include); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		return nil
	}
	sort.Strings(wireRoots)
	for _, root := range wireRoots {
		if err := include(root); err != nil {
			return nil, err
		}
	}
	defsJSON := map[string]any{}
	allNames := make([]string, 0, len(wireTypes))
	for n := range wireTypes {
		allNames = append(allNames, n)
	}
	sort.Strings(allNames)
	for _, name := range allNames {
		if _, ok := defs[name].(*ast.StructType); !ok {
			continue
		}
		s, err := typeSchema(defs[name], name, defs, enums)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if union, ok := taggedUnions[name]; ok {
			s, err = taggedUnionSchema(defs[name], name, union, defs, enums)
			if err != nil {
				return nil, fmt.Errorf("%s tagged union: %w", name, err)
			}
		}
		defsJSON[name] = s
	}
	props := map[string]any{}
	for _, n := range wireRoots {
		if v, ok := defsJSON[n]; ok {
			props[n] = v
		}
	}
	doc := schema{"$schema": "https://json-schema.org/draft/2020-12/schema", "$id": fmt.Sprintf("https://kora.dev/schema/engine-v%d.json", version), "title": fmt.Sprintf("Kora Engine v%d wire contracts", version), "type": "object", "properties": props, "additionalProperties": false, "$defs": defsJSON, "x-enums": enums}
	return json.MarshalIndent(doc, "", "  ")
}

func hasVersionMarker(doc, name string, version int) bool {
	marker := "//contract:" + name
	if version == 2 {
		return strings.Contains(doc, marker+":v2")
	}
	return strings.Contains(doc, marker) && !strings.Contains(doc, marker+":v2")
}

type taggedUnionSpec struct {
	Discriminator string
	Variants      []string
}

func commentDirective(doc, directive string) string {
	index := strings.Index(doc, directive)
	if index < 0 {
		return ""
	}
	value := doc[index+len(directive):]
	if end := strings.Index(value, "//"); end >= 0 {
		value = value[:end]
	}
	return strings.TrimSpace(value)
}

func taggedUnionSchema(expr ast.Expr, owner string, union taggedUnionSpec, defs map[string]ast.Expr, enums map[string][]string) (schema, error) {
	st, ok := expr.(*ast.StructType)
	if !ok {
		return nil, fmt.Errorf("tagged union must be a struct")
	}
	fields := map[string]*ast.Field{}
	for _, field := range st.Fields.List {
		for _, name := range field.Names {
			fields[name.Name] = field
		}
	}
	if fields[union.Discriminator] == nil {
		return nil, fmt.Errorf("discriminator field %s is missing", union.Discriminator)
	}
	variants := make(map[string]bool, len(union.Variants))
	for _, name := range union.Variants {
		if fields[name] == nil {
			return nil, fmt.Errorf("variant field %s is missing", name)
		}
		if variants[name] {
			return nil, fmt.Errorf("duplicate variant field %s", name)
		}
		variants[name] = true
	}
	branches := make([]any, 0, len(union.Variants))
	for _, variant := range union.Variants {
		properties := map[string]any{}
		required := []string{}
		for goName, field := range fields {
			if variants[goName] && goName != variant {
				continue
			}
			jsonTag := ""
			if field.Tag != nil {
				raw, err := strconv.Unquote(field.Tag.Value)
				if err != nil {
					return nil, err
				}
				jsonTag = reflectJSONName(raw)
			}
			jsonName, omit, skip := parseJSONTag(jsonTag, goName)
			if skip {
				continue
			}
			var property schema
			var err error
			switch {
			case goName == union.Discriminator:
				property = schema{"type": "string", "const": strings.ToLower(variant)}
			case variants[goName]:
				property, err = typeSchema(field.Type, owner, defs, enums)
			default:
				property, err = typeSchema(field.Type, owner, defs, enums)
			}
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", goName, err)
			}
			properties[jsonName] = property
			if goName == union.Discriminator || goName == variant || !omit {
				required = append(required, jsonName)
			}
		}
		sort.Strings(required)
		branches = append(branches, schema{"type": "object", "properties": properties, "required": required, "additionalProperties": false})
	}
	return schema{"oneOf": branches, "x-discriminator": union.Discriminator}, nil
}

func fieldName(field *ast.Field) string {
	if len(field.Names) == 0 {
		return "<anonymous>"
	}
	return field.Names[0].Name
}

func hasDirective(field *ast.Field, directive string) bool {
	for _, group := range []*ast.CommentGroup{field.Doc, field.Comment} {
		if group != nil && strings.Contains(rawComments(group), directive) {
			return true
		}
	}
	return false
}

func numericDirective(field *ast.Field, prefix string) (float64, bool, error) {
	for _, group := range []*ast.CommentGroup{field.Doc, field.Comment} {
		if group == nil {
			continue
		}
		for _, comment := range group.List {
			text := strings.TrimSpace(strings.TrimPrefix(comment.Text, "//"))
			if strings.HasPrefix(text, prefix) {
				value, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(text, prefix)), 64)
				return value, true, err
			}
		}
	}
	return 0, false, nil
}

func stringDirective(field *ast.Field, prefix string) (string, bool) {
	for _, group := range []*ast.CommentGroup{field.Doc, field.Comment} {
		if group == nil {
			continue
		}
		for _, comment := range group.List {
			text := strings.TrimSpace(strings.TrimPrefix(comment.Text, "//"))
			if strings.HasPrefix(text, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(text, prefix)), true
			}
		}
	}
	return "", false
}

func isRawJSON(expr ast.Expr) bool {
	x, ok := expr.(*ast.SelectorExpr)
	if !ok || x.Sel.Name != "RawMessage" {
		return false
	}
	pkg, ok := x.X.(*ast.Ident)
	return ok && pkg.Name == "json"
}

func isRawJSONList(expr ast.Expr) bool {
	array, ok := expr.(*ast.ArrayType)
	return ok && array.Len == nil && isRawJSON(array.Elt)
}

func rawComments(group *ast.CommentGroup) string {
	var b strings.Builder
	for _, comment := range group.List {
		b.WriteString(comment.Text)
	}
	return b.String()
}

func visitType(expr ast.Expr, defs map[string]ast.Expr, include func(string) error) error {
	switch t := expr.(type) {
	case *ast.Ident:
		switch t.Name {
		case "string", "bool", "byte", "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "float32", "float64":
			return nil
		}
		if strings.HasPrefix(t.Name, "int") || strings.HasPrefix(t.Name, "uint") || t.Name == "float64" {
			return nil
		}
		if t.Name == "Time" {
			return nil
		}
		_, ok := defs[t.Name]
		if !ok {
			return fmt.Errorf("unsupported identifier %q", t.Name)
		}
		return include(t.Name)
	case *ast.SelectorExpr:
		if x, ok := t.X.(*ast.Ident); ok && x.Name == "time" && t.Sel.Name == "Time" {
			return nil
		}
		if isRawJSON(t) {
			return nil
		}
		return fmt.Errorf("unsupported selector")
	case *ast.StarExpr:
		return visitType(t.X, defs, include)
	case *ast.ArrayType:
		if t.Len != nil {
			return fmt.Errorf("arrays are unsupported")
		}
		return visitType(t.Elt, defs, include)
	case *ast.MapType:
		k, ok := t.Key.(*ast.Ident)
		if !ok {
			return fmt.Errorf("map keys must be a string or registered string enum")
		}
		if k.Name != "string" {
			keyType, defined := defs[k.Name]
			base, isString := keyType.(*ast.Ident)
			if !defined || !isString || base.Name != "string" {
				return fmt.Errorf("map key type %s must be a defined string type", k.Name)
			}
		}
		return visitType(t.Value, defs, include)
	case *ast.StructType:
		for _, f := range t.Fields.List {
			if err := visitType(f.Type, defs, include); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported Go type %T", expr)
	}
}

func enumValues(files []string, fset *token.FileSet, root string) ([]string, error) {
	var vals []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			active := false
			for _, sp := range g.Specs {
				v, ok := sp.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if v.Type != nil {
					if id, ok := v.Type.(*ast.Ident); ok {
						active = id.Name == root
					} else {
						active = false
					}
				}
				for i, n := range v.Names {
					if n.Name == root {
						active = true
					}
					if active && i < len(v.Values) {
						if lit, ok := v.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							s, e := strconv.Unquote(lit.Value)
							if e != nil {
								return nil, e
							}
							vals = append(vals, s)
						}
					}
				}
			}
		}
	}
	return vals, nil
}

func typeSchema(expr ast.Expr, owner string, defs map[string]ast.Expr, enums map[string][]string) (schema, error) {
	switch t := expr.(type) {
	case *ast.Ident:
		switch t.Name {
		case "string":
			return schema{"type": "string"}, nil
		case "bool":
			return schema{"type": "boolean"}, nil
		case "byte":
			return schema{"type": "integer", "minimum": 0, "maximum": 255}, nil
		case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64":
			return schema{"type": "integer"}, nil
		case "float32", "float64":
			return schema{"type": "number"}, nil
		case "Time":
			return schema{"type": "string", "format": "date-time"}, nil
		}
		if values, ok := enums[t.Name]; ok {
			return schema{"type": "string", "enum": values}, nil
		}
		if expr, ok := defs[t.Name]; ok {
			if _, isStruct := expr.(*ast.StructType); isStruct {
				return schema{"$ref": "#/$defs/" + t.Name}, nil
			}
			// Defined scalar contract types (for example Revision or an opaque
			// ID) have no standalone JSON object definition; inline their base
			// scalar schema instead of emitting a dangling $ref.
			return typeSchema(expr, owner, defs, enums)
		}
		return nil, fmt.Errorf("unsupported identifier %q", t.Name)
	case *ast.SelectorExpr:
		if x, ok := t.X.(*ast.Ident); ok && x.Name == "time" && t.Sel.Name == "Time" {
			return schema{"type": "string", "format": "date-time"}, nil
		}
		return nil, fmt.Errorf("unsupported selector")
	case *ast.StarExpr:
		s, e := typeSchema(t.X, owner, defs, enums)
		if e != nil {
			return nil, e
		}
		s["x-optional-pointer"] = true
		return s, nil
	case *ast.ArrayType:
		if t.Len != nil {
			return nil, fmt.Errorf("arrays are unsupported")
		}
		items, e := typeSchema(t.Elt, owner, defs, enums)
		if e != nil {
			return nil, e
		}
		return schema{"type": "array", "items": items}, nil
	case *ast.MapType:
		v, e := typeSchema(t.Value, owner, defs, enums)
		if e != nil {
			return nil, e
		}
		k, ok := t.Key.(*ast.Ident)
		if !ok {
			return nil, fmt.Errorf("map keys must be a string or registered string enum")
		}
		if k.Name == "string" {
			return schema{"type": "object", "additionalProperties": v}, nil
		}
		if values, ok := enums[k.Name]; ok {
			return schema{"type": "object", "propertyNames": schema{"type": "string", "enum": values}, "additionalProperties": v}, nil
		}
		return nil, fmt.Errorf("map key type %s is not a registered string enum", k.Name)
	case *ast.StructType:
		properties := map[string]any{}
		required := []string{}
		for _, field := range t.Fields.List {
			if len(field.Names) == 0 {
				return nil, fmt.Errorf("anonymous fields are unsupported")
			}
			tag := ""
			if field.Tag != nil {
				u, e := strconv.Unquote(field.Tag.Value)
				if e != nil {
					return nil, e
				}
				tag = reflectJSONName(u)
			}
			for _, name := range field.Names {
				if !name.IsExported() {
					continue
				}
				jsonName, omit, skip := parseJSONTag(tag, name.Name)
				if skip {
					continue
				}
				var s schema
				var e error
				if isRawJSON(field.Type) {
					if !hasDirective(field, "contract:json") {
						return nil, fmt.Errorf("field %s: json.RawMessage requires //contract:json", name.Name)
					}
					s = schema{"x-open-json-value": true}
				} else if isRawJSONList(field.Type) {
					if !hasDirective(field, "contract:json") {
						return nil, fmt.Errorf("field %s: []json.RawMessage requires //contract:json", name.Name)
					}
					s = schema{"type": "array", "items": schema{"x-open-json-value": true}}
				} else {
					s, e = typeSchema(field.Type, owner, defs, enums)
				}
				if e != nil {
					return nil, fmt.Errorf("field %s: %w", name.Name, e)
				}
				if minimum, ok, err := numericDirective(field, "contract:min="); err != nil {
					return nil, fmt.Errorf("field %s minimum rule: %w", name.Name, err)
				} else if ok {
					s["minimum"] = minimum
				}
				if maximum, ok, err := numericDirective(field, "contract:max="); err != nil {
					return nil, fmt.Errorf("field %s maximum rule: %w", name.Name, err)
				} else if ok {
					s["maximum"] = maximum
				}
				if constant, ok, err := numericDirective(field, "contract:const="); err != nil {
					return nil, fmt.Errorf("field %s constant rule: %w", name.Name, err)
				} else if ok {
					if constant != float64(int64(constant)) {
						return nil, fmt.Errorf("field %s constant must be an integer", name.Name)
					}
					s["const"] = int64(constant)
				}
				for _, bound := range []struct {
					directive string
					keyword   string
				}{
					{"contract:minLength=", "minLength"},
					{"contract:maxLength=", "maxLength"},
					{"contract:minItems=", "minItems"},
					{"contract:maxItems=", "maxItems"},
					{"contract:maxBytes=", "x-max-utf8-bytes"},
					{"contract:maxJSONBytes=", "x-max-json-bytes"},
				} {
					value, found, err := numericDirective(field, bound.directive)
					if err != nil {
						return nil, fmt.Errorf("field %s %s rule: %w", name.Name, bound.keyword, err)
					}
					if found {
						if value < 0 || value != float64(int64(value)) {
							return nil, fmt.Errorf("field %s %s must be a non-negative integer", name.Name, bound.keyword)
						}
						s[bound.keyword] = int64(value)
					}
				}
				if pattern, found := stringDirective(field, "contract:pattern="); found {
					if _, err := regexp.Compile(pattern); err != nil {
						return nil, fmt.Errorf("field %s pattern rule: %w", name.Name, err)
					}
					s["pattern"] = pattern
				}
				if itemPattern, found := stringDirective(field, "contract:itemPattern="); found {
					if _, err := regexp.Compile(itemPattern); err != nil {
						return nil, fmt.Errorf("field %s item pattern rule: %w", name.Name, err)
					}
					items, ok := s["items"].(schema)
					if !ok || items["type"] != "string" {
						return nil, fmt.Errorf("field %s item pattern requires an array of strings", name.Name)
					}
					items["pattern"] = itemPattern
				}
				properties[jsonName] = s
				if !omit {
					required = append(required, jsonName)
				}
			}
		}
		sort.Strings(required)
		out := schema{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			out["required"] = required
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported Go type %T", expr)
	}
}

// Keep the JSON struct-tag parser local to the generator's supported subset.
func reflectJSONName(tag string) string {
	for _, part := range strings.Split(tag, " ") {
		if strings.HasPrefix(part, "json:") {
			v, _ := strconv.Unquote(strings.TrimPrefix(part, "json:"))
			return v
		}
	}
	return ""
}
func parseJSONTag(tag, fallback string) (name string, omit, skip bool) {
	if tag == "" {
		return fallback, false, false
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "-" {
		return "", false, true
	}
	if name == "" {
		name = fallback
	}
	for _, p := range parts[1:] {
		if p == "omitempty" {
			omit = true
		}
	}
	return
}
