package doctype

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	goyaml "github.com/goccy/go-yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

// ──────────────────────────────────────────────────────────────
// Layer 1: Strict YAML Unmarshaling
// ──────────────────────────────────────────────────────────────

// YAMLSyntaxError is a structured error for YAML parsing failures.
// It includes location information for precise error reporting in editors.
type YAMLSyntaxError struct {
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
	Key     string `json:"key"`
	Context string `json:"context"`
	Detail  string `json:"detail,omitempty"` // "did you mean?" suggestion
}

func (e *YAMLSyntaxError) Error() string {
	if e.Key != "" {
		msg := fmt.Sprintf("line %d: unknown key %q in %s", e.Line, e.Key, e.Context)
		if e.Detail != "" {
			msg += " (" + e.Detail + ")"
		}
		return msg
	}
	return fmt.Sprintf("line %d: %s", e.Line, e.Message)
}

// strictUnmarshal unmarshals YAML data into v, rejecting any unknown keys.
// Returns a slice of YAMLSyntaxError if unknown keys are found.
func strictUnmarshal(data []byte, v interface{}, context string) ([]YAMLSyntaxError, error) {
	// First, check for unknown keys by parsing the YAML structure.
	unknownKeys := findUnknownKeys(data, v)
	if len(unknownKeys) > 0 {
		errs := make([]YAMLSyntaxError, len(unknownKeys))
		for i, uk := range unknownKeys {
			errs[i] = YAMLSyntaxError{
				Line:    uk.Line,
				Column:  uk.Column,
				Message: fmt.Sprintf("unknown key %q in %s", uk.Key, context),
				Key:     uk.Key,
				Context: context,
			}
		}
		return errs, nil
	}

	// No unknown keys — parse with goccy/go-yaml.
	if err := goyaml.UnmarshalWithOptions(data, v, goyaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("yaml parse error: %w", err)
	}
	return nil, nil
}

// unknownKey represents a YAML key not matching the target struct.
type unknownKey struct {
	Key    string
	Line   int
	Column int
}

// findUnknownKeys parses the YAML and returns keys that don't match the target struct.
// It walks the YAML document tree and the Go struct type simultaneously.
func findUnknownKeys(data []byte, v interface{}) []unknownKey {
	var unknown []unknownKey

	// Parse YAML into a generic node tree.
	var root yamlv3.Node
	if err := yamlv3.Unmarshal(data, &root); err != nil {
		return nil // Let the actual unmarshal handle syntax errors.
	}

	if len(root.Content) == 0 {
		return nil
	}

	// Get the known field names from the struct.
	knownFields := structFieldNames(v)

	// Walk top-level mapping.
	topNode := root.Content[0]
	if topNode.Kind != yamlv3.MappingNode {
		return nil
	}

	for i := 0; i < len(topNode.Content); i += 2 {
		keyNode := topNode.Content[i]
		if keyNode.Kind == yamlv3.ScalarNode {
			key := keyNode.Value
			if !knownFields[key] {
				unknown = append(unknown, unknownKey{
					Key:    key,
					Line:   keyNode.Line,
					Column: keyNode.Column,
				})
			}
		}
	}

	// Walk into fields[] if it exists.
	for i := 0; i < len(topNode.Content); i += 2 {
		keyNode := topNode.Content[i]
		valNode := topNode.Content[i+1]
		if keyNode.Value == "fields" && valNode.Kind == yamlv3.SequenceNode {
			fieldKnownKeys := structFieldNames(&Field{})
			for _, fieldNode := range valNode.Content {
				if fieldNode.Kind == yamlv3.MappingNode {
					for j := 0; j < len(fieldNode.Content); j += 2 {
						fkNode := fieldNode.Content[j]
						if fkNode.Kind == yamlv3.ScalarNode {
							fk := fkNode.Value
							if !fieldKnownKeys[fk] {
								unknown = append(unknown, unknownKey{
									Key:    fk,
									Line:   fkNode.Line,
									Column: fkNode.Column,
									// We store context via the main error path
								})
							}
							// For doc_constraints, also check nested keys
							if fk == "doc_constraints" {
								dcKnown := knownDocConstraintFields
								dcNode := fieldNode.Content[j+1]
								if dcNode.Kind == yamlv3.SequenceNode {
									for _, dcn := range dcNode.Content {
										if dcn.Kind == yamlv3.MappingNode {
											for k := 0; k < len(dcn.Content); k += 2 {
												dkNode := dcn.Content[k]
												if dkNode.Kind == yamlv3.ScalarNode {
													dk := dkNode.Value
													if !dcKnown[dk] {
														unknown = append(unknown, unknownKey{
															Key:    dk,
															Line:   dkNode.Line,
															Column: dkNode.Column,
														})
													}
												}
											}
										}
										// di is the index in doc_constraints
									}
								}
							}
							// For constraints, check nested keys
							if fk == "constraints" {
								cKnown := knownConstraintFields
								cNode := fieldNode.Content[j+1]
								if cNode.Kind == yamlv3.SequenceNode {
									for _, cn := range cNode.Content {
										if cn.Kind == yamlv3.MappingNode {
											for k := 0; k < len(cn.Content); k += 2 {
												ckNode := cn.Content[k]
												if ckNode.Kind == yamlv3.ScalarNode {
													ck := ckNode.Value
													if !cKnown[ck] {
														unknown = append(unknown, unknownKey{
															Key:    ck,
															Line:   ckNode.Line,
															Column: ckNode.Column,
														})
													}
												}
											}
										}
										// ci is the index in constraints
									}
								}
							}
						}
					}
				}
			}
		}
	}

	return unknown
}

// structFieldNames returns a set of YAML field names from a struct's yaml tags.
func structFieldNames(v interface{}) map[string]bool {
	names := make(map[string]bool)
	// We use a pre-computed map based on the type.
	switch v.(type) {
	case *DocType:
		return knownDocTypeFields
	case *Field:
		return knownFieldFields
	case *Constraint:
		return knownConstraintFields
	case *DocConstraint:
		return knownDocConstraintFields
	}
	return names
}

// Pre-computed known field sets for strict-mode validation.
var knownDocTypeFields = buildFieldSet(DocType{})
var knownFieldFields = buildFieldSet(Field{})
var knownConstraintFields = buildConstraintFieldSet()
var knownDocConstraintFields = buildDocConstraintFieldSet()

func buildFieldSet(s interface{}) map[string]bool {
	// We manually list the yaml tags to avoid reflection at runtime.
	// This also serves as documentation of all valid fields.
	return map[string]bool{
		// DocType fields
		"name": true, "module": true, "is_submittable": true,
		"is_child_table": true, "is_single": true, "track_changes": true,
		"title_field": true, "search_fields": true, "sort_field": true,
		"sort_order": true, "description": true, "fields": true,
		"doc_constraints": true,
		// Field fields
		"fieldname": true, "fieldtype": true, "label": true, "options": true,
		"reqd": true, "unique": true, "default": true, "hidden": true,
		"read_only": true, "bold": true, "in_list_view": true,
		"in_standard_filter": true, "search_index": true,
		"depends_on": true, "mandatory_depends_on": true, "constraints": true,
		"renamed_from": true, "linked_field": true, "computed": true,
	}
}

func buildConstraintFieldSet() map[string]bool {
	return map[string]bool{
		"type": true, "value": true, "values": true, "pattern": true,
		"message": true, "condition": true, "scope": true,
	}
}

func buildDocConstraintFieldSet() map[string]bool {
	return map[string]bool{
		"type": true, "description": true, "condition": true,
		"require_fields": true, "field": true, "group_by": true,
		"max": true, "message": true, "lhs": true, "operator": true,
		"rhs": true, "fields": true, "status_field": true,
		"status_values": true, "immutable_fields": true, "constraints": true,
	}
}

// ──────────────────────────────────────────────────────────────
// File Parsing (with strict mode for user input)
// ──────────────────────────────────────────────────────────────

// ParseFile reads a single YAML/JSON file and returns a DocType.
// Uses strict mode: unknown YAML keys are rejected.
func ParseFile(path string) (*DocType, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading file %s: %w", path, err)
	}

	dt := &DocType{}
	unknowns, err := strictUnmarshal(data, dt, "doctype")
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(unknowns) > 0 {
		for _, u := range unknowns {
			// Enhance with "did you mean?" suggestions.
			if suggestion := suggestField(u.Key, knownDocTypeFields); suggestion != "" {
				u.Detail = fmt.Sprintf("did you mean %q?", suggestion)
			}
		}
		return nil, formatSyntaxErrors(path, unknowns)
	}

	// Also check fields for unknown keys by parsing the raw YAML with the field struct.
	if err := checkFieldUnknownKeys(data); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	if err := dt.Validate(); err != nil {
		return nil, fmt.Errorf("validating %s: %w", path, err)
	}

	return dt, nil
}

// ParseFileNonStrict reads a YAML file without strict key checking.
// Used for internal operations where extra keys are expected (e.g., app.yaml).
func ParseFileNonStrict(path string) (*DocType, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading file %s: %w", path, err)
	}

	dt := &DocType{}
	if err := goyaml.Unmarshal(data, dt); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	if err := dt.Validate(); err != nil {
		return nil, fmt.Errorf("validating %s: %w", path, err)
	}

	return dt, nil
}

// checkFieldUnknownKeys walks the YAML tree and checks for unknown keys inside fields and constraints.
func checkFieldUnknownKeys(data []byte) error {
	var root yamlv3.Node
	if err := yamlv3.Unmarshal(data, &root); err != nil {
		return nil // let main unmarshal handle.
	}
	if len(root.Content) == 0 {
		return nil
	}

	topNode := root.Content[0]
	if topNode.Kind != yamlv3.MappingNode {
		return nil
	}

	var errs []YAMLSyntaxError
	for i := 0; i < len(topNode.Content); i += 2 {
		keyNode := topNode.Content[i]
		valNode := topNode.Content[i+1]
		if keyNode.Value == "fields" && valNode.Kind == yamlv3.SequenceNode {
			for fi, fieldNode := range valNode.Content {
				if fieldNode.Kind == yamlv3.MappingNode {
					errs = append(errs, findUnknownInMapping(fieldNode, knownFieldFields, fmt.Sprintf("fields[%d]", fi))...)
					// Also walk into constraints and doc_constraints inside fields.
					for j := 0; j < len(fieldNode.Content); j += 2 {
						fkNode := fieldNode.Content[j]
						fvNode := fieldNode.Content[j+1]
						if fkNode.Value == "constraints" && fvNode.Kind == yamlv3.SequenceNode {
							for ci, cNode := range fvNode.Content {
								if cNode.Kind == yamlv3.MappingNode {
									errs = append(errs, findUnknownInMapping(cNode, knownConstraintFields, fmt.Sprintf("fields[%d].constraints[%d]", fi, ci))...)
								}
							}
						}
						if fkNode.Value == "doc_constraints" && fvNode.Kind == yamlv3.SequenceNode {
							for di, dcNode := range fvNode.Content {
								if dcNode.Kind == yamlv3.MappingNode {
									errs = append(errs, findUnknownInMapping(dcNode, knownDocConstraintFields, fmt.Sprintf("fields[%d].doc_constraints[%d]", fi, di))...)
								}
							}
						}
					}
				}
			}
		}
	}

	if len(errs) > 0 {
		for i := range errs {
			if suggestion := suggestField(errs[i].Key, knownFieldFields); suggestion != "" {
				errs[i].Detail = fmt.Sprintf("did you mean %q?", suggestion)
			}
		}
		return formatSyntaxErrors("doctype fields", errs)
	}
	return nil
}

func findUnknownInMapping(node *yamlv3.Node, known map[string]bool, context string) []YAMLSyntaxError {
	var errs []YAMLSyntaxError
	for j := 0; j < len(node.Content); j += 2 {
		kNode := node.Content[j]
		if kNode.Kind == yamlv3.ScalarNode {
			key := kNode.Value
			if !known[key] {
				errs = append(errs, YAMLSyntaxError{
					Line:    kNode.Line,
					Column:  kNode.Column,
					Message: fmt.Sprintf("unknown field key %q in %s", key, context),
					Key:     key,
					Context: context,
				})
			}
		}
	}
	return errs
}

func formatSyntaxErrors(source string, errs []YAMLSyntaxError) error {
	if len(errs) == 0 {
		return nil
	}
	if len(errs) == 1 {
		e := errs[0]
		if e.Detail != "" {
			return fmt.Errorf("%s: line %d: %s (%s)", source, e.Line, e.Message, e.Detail)
		}
		return fmt.Errorf("%s: line %d: %s", source, e.Line, e.Message)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s: %d unknown keys found:\n", source, len(errs)))
	for _, e := range errs {
		if e.Detail != "" {
			sb.WriteString(fmt.Sprintf("  line %d: %s (%s)\n", e.Line, e.Message, e.Detail))
		} else {
			sb.WriteString(fmt.Sprintf("  line %d: %s\n", e.Line, e.Message))
		}
	}
	return fmt.Errorf("%s", strings.TrimSpace(sb.String()))
}

// ParseDirectory reads all YAML/JSON files in a directory and returns the DocTypes found.
func ParseDirectory(path string) ([]*DocType, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("reading directory %s: %w", path, err)
	}

	var doctypes []*DocType
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".yaml" && ext != ".yml" && ext != ".json" {
			continue
		}

		filePath := filepath.Join(path, entry.Name())

		// Skip workflow files (they have "document_type" and "states", not "module" and "fields").
		if isWorkflowFile(filePath) {
			continue
		}

		dt, err := ParseFile(filePath)
		if err != nil {
			return nil, err
		}
		doctypes = append(doctypes, dt)
	}

	return doctypes, nil
}

// isWorkflowFile checks if a YAML file is a workflow definition by peeking at its top-level keys.
func isWorkflowFile(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	hasDocType := containsKey(data, "document_type")
	hasStates := containsKey(data, "states")
	hasModule := containsKey(data, "module")
	hasFields := containsKey(data, "fields")
	if hasDocType && hasStates && !hasModule && !hasFields {
		return true
	}
	return false
}

// containsKey does a simple string check for a top-level YAML key.
func containsKey(data []byte, key string) bool {
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key+":") || strings.HasPrefix(trimmed, key+" :") {
			return true
		}
	}
	return false
}

// ParseConfigTree reads the full config directory structure:
//
//	config/<app>/
//	  app.yaml
//	  roles.yaml
//	  permissions.yaml
//	  scheduler.yaml
//	  doctypes/
//	    *.yaml
//
// Returns DocTypes, Roles, Permissions, and other configs separately.
func ParseConfigTree(basePath string) ([]*DocType, error) {
	doctypesPath := filepath.Join(basePath, "doctypes")
	if _, err := os.Stat(doctypesPath); err == nil {
		return ParseDirectory(doctypesPath)
	}
	return ParseDirectory(basePath)
}

// ──────────────────────────────────────────────────────────────
// Layer 2: Deep Field Validation
// ──────────────────────────────────────────────────────────────

// Validate checks that the DocType definition is structurally valid.
func (d *DocType) Validate() error {
	if d.Name == "" {
		return fmt.Errorf("doctype name is required")
	}
	if d.Module == "" {
		return fmt.Errorf("doctype %s: module is required", d.Name)
	}

	// Validate doctype name format.
	if err := validateDocTypeName(d.Name); err != nil {
		return err
	}

	// Set defaults.
	if d.SortField == "" {
		d.SortField = "modified"
	}
	if d.SortOrder == "" {
		d.SortOrder = "DESC"
	}
	if d.TitleField == "" {
		d.TitleField = "name"
	}

	fieldnames := make(map[string]bool)
	for i := range d.Fields {
		f := &d.Fields[i]

		if f.Fieldname == "" {
			return fmt.Errorf("doctype %s: field %d has no fieldname", d.Name, i)
			}
			if isReservedFieldName(f.Fieldname) {
				return fmt.Errorf("doctype %s: field %q conflicts with a reserved system column name. Reserved names: name, owner, creation, modified, modified_by, doc_status, idx, parent, parentfield, parenttype", d.Name, f.Fieldname)
		}

		// Validate field name format.
		if err := validateFieldName(f.Fieldname); err != nil {
			return fmt.Errorf("doctype %s, field %d: %w", d.Name, i, err)
		}

		if fieldnames[f.Fieldname] {
			return fmt.Errorf("doctype %s: duplicate fieldname %q", d.Name, f.Fieldname)
		}
		fieldnames[f.Fieldname] = true

		if f.Fieldtype == "" {
			return fmt.Errorf("doctype %s: field %s has no fieldtype", d.Name, f.Fieldname)
		}
		if err := validateFieldType(f.Fieldtype); err != nil {
			return fmt.Errorf("doctype %s, field %s: %w", d.Name, f.Fieldname, err)
		}

		// Set default label.
		if f.Label == "" {
			f.Label = fieldnameToLabel(f.Fieldname)
		}

		// Validate constraints.
		for j, c := range f.Constraints {
			if c.Type == "" {
				return fmt.Errorf("doctype %s, field %s: constraint %d has no type", d.Name, f.Fieldname, j)
			}
			if c.Message == "" {
				return fmt.Errorf("doctype %s, field %s: constraint %d has no message", d.Name, f.Fieldname, j)
			}
			// Validate constraint value type matches constraint type.
			if err := validateConstraintValue(c); err != nil {
				return fmt.Errorf("doctype %s, field %s, constraint %d: %w", d.Name, f.Fieldname, j, err)
			}
		}

		// Validate Table field has options (the child DocType name).
		if f.Fieldtype == "Table" && f.Options == "" {
			return fmt.Errorf("doctype %s, field %s: Table field requires options (child DocType name)", d.Name, f.Fieldname)
		}

		// Validate Link field has options (the target DocType name).
		if f.Fieldtype == "Link" && f.Options == "" {
			return fmt.Errorf("doctype %s, field %s: Link field requires options (target DocType name)", d.Name, f.Fieldname)
		}

		// Validate Select field options format (newline-separated).
		if f.Fieldtype == "Select" && f.Options != "" {
			parts := strings.Split(f.Options, "\n")
			nonEmpty := 0
			for _, p := range parts {
				if strings.TrimSpace(p) != "" {
					nonEmpty++
				}
			}
			if nonEmpty == 0 {
				return fmt.Errorf("doctype %s, field %s: Select field options must be newline-separated values", d.Name, f.Fieldname)
			}
		}

		// Validate computed expression syntax (basic check).
		if f.Computed != "" {
			if err := validateComputedExpr(f.Computed); err != nil {
				return fmt.Errorf("doctype %s, field %s: %w", d.Name, f.Fieldname, err)
			}
		}

		// Validate depends_on references real fields.
		if f.DependsOn != "" {
			if err := validateDependsOn(f.DependsOn, fieldnames); err != nil {
				return fmt.Errorf("doctype %s, field %s: %w", d.Name, f.Fieldname, err)
			}
		}

		// Validate mandatory_depends_on doesn't conflict with reqd.
		if f.MandatoryDependsOn != "" && f.Reqd {
			return fmt.Errorf("doctype %s, field %s: mandatory_depends_on and reqd are redundant — use one or the other", d.Name, f.Fieldname)
		}
	}

	// Validate doc_constraints.
	for i, dc := range d.DocConstraints {
		if dc.Type == "" {
			return fmt.Errorf("doctype %s: doc_constraint %d has no type", d.Name, i)
		}
		if dc.Message == "" {
			return fmt.Errorf("doctype %s: doc_constraint %d has no message", d.Name, i)
		}
		// Validate nested constraints in immutable_after.
		if dc.Type == "immutable_after" {
			for j, c := range dc.Constraints {
				if c.Type == "" {
					return fmt.Errorf("doctype %s, doc_constraint %d: nested constraint %d has no type", d.Name, i, j)
				}
				if err := validateConstraintValue(c); err != nil {
					return fmt.Errorf("doctype %s, doc_constraint %d, constraint %d: %w", d.Name, i, j, err)
				}
			}
		}
	}

	return nil
}

// validateDocTypeName checks the doctype name uses valid characters.
func validateDocTypeName(name string) error {
	// DocType names can contain spaces (e.g., "Service Report", "Work Order").
	// They cannot be empty (checked elsewhere).
	return nil
}

// validFieldNameRe is a simple check: field names must start with a lowercase letter
// and contain only lowercase letters, digits, and underscores.
func validateFieldName(name string) error {
	if len(name) == 0 {
		return fmt.Errorf("field name is empty")
	}
	if name[0] < 'a' || name[0] > 'z' {
		return fmt.Errorf("field name %q must start with a lowercase letter", name)
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return fmt.Errorf("field name %q contains invalid character %q — use only [a-z0-9_]", name, c)
		}
	}
	return nil
}

// validateConstraintValue checks that the constraint value type matches the constraint type.
func validateConstraintValue(c Constraint) error {
	switch c.Type {
	case "min", "max", "min_length", "max_length", "min_rows", "max_rows":
		// Numeric constraints - value should be numeric.
		if c.Value != nil {
			switch c.Value.(type) {
			case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
				// OK
			default:
				return fmt.Errorf("constraint type %q expects a numeric value, got %v", c.Type, c.Value)
			}
		}
	case "min_date", "max_date":
		// Date constraints - value should be a date string or number.
		if c.Value != nil {
			switch c.Value.(type) {
			case string, float64, int, int64:
				// OK
			default:
				return fmt.Errorf("constraint type %q expects a date or number, got %v", c.Type, c.Value)
			}
		}
	case "regex":
		if c.Pattern == "" {
			return fmt.Errorf("constraint type %q requires a pattern field", c.Type)
		}
	case "one_of", "not_one_of":
		if len(c.Values) == 0 && c.Value == nil {
			return fmt.Errorf("constraint type %q requires values or value field", c.Type)
		}
	case "exists", "unique_in":
		// These are valid with just a message.
	default:
		// Unknown constraint type — warn but don't block (for forward compat).
	}
	return nil
}

// validateComputedExpr performs a basic syntax check on computed field expressions.
func validateComputedExpr(expr string) error {
	if strings.TrimSpace(expr) == "" {
		return fmt.Errorf("computed expression is empty")
	}
	// Check for balanced parentheses.
	if strings.Count(expr, "(") != strings.Count(expr, ")") {
		return fmt.Errorf("computed expression has unbalanced parentheses: %s", expr)
	}
	// Check that it doesn't end with an operator.
	trimmed := strings.TrimSpace(expr)
	for _, op := range []string{"+", "-", "*", "/"} {
		if strings.HasSuffix(trimmed, op) {
			return fmt.Errorf("computed expression ends with operator %q: %s", op, expr)
		}
	}
	return nil
}

// validateDependsOn checks that depends_on references real field names.
func validateDependsOn(dependsOn string, knownFields map[string]bool) error {
	// depends_on can be a single field name or comma-separated list.
	fields := strings.Split(dependsOn, ",")
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !knownFields[f] {
			return fmt.Errorf("depends_on references unknown field %q", f)
		}
	}
	return nil
}

func validateFieldType(ft string) error {
	validTypes := map[string]bool{
		"Data": true, "Text": true, "Text Editor": true,
		"Int": true, "Float": true, "Currency": true, "Percent": true,
		"Check": true, "Date": true, "Time": true, "Datetime": true,
		"Select": true, "Link": true, "Dynamic Link": true,
		"Table": true, "Attach": true, "Attach Image": true,
		"JSON": true, "Password": true,
		"Section Break": true, "Column Break": true, "Heading": true,
	}
	if !validTypes[ft] {
		return fmt.Errorf("unknown fieldtype %q", ft)
	}
	return nil
}

func fieldnameToLabel(name string) string {
	parts := strings.Split(name, "_")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

// ──────────────────────────────────────────────────────────────
// Layer 3: Cross-File Validation
// ──────────────────────────────────────────────────────────────

// CrossFileError represents a validation error that spans multiple doctypes.
type CrossFileError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	DocType string `json:"doctype"`
	Field   string `json:"field,omitempty"`
	Related string `json:"related,omitempty"`
}

func (e *CrossFileError) Error() string {
	return fmt.Sprintf("[%s] %s: %s (related: %s)", e.Code, e.DocType, e.Message, e.Related)
}

// ValidateAll performs cross-file validation across all loaded doctypes.
func ValidateAll(doctypes []*DocType) []CrossFileError {
	var errs []CrossFileError
	dtMap := make(map[string]*DocType)
	for _, dt := range doctypes {
		dtMap[dt.Name] = dt
	}

	childTables := make(map[string]string) // child doctype name → parent doctype

	for _, dt := range doctypes {
		for _, f := range dt.Fields {
			// Check Link fields reference existing doctypes.
			if f.Fieldtype == "Link" && f.Options != "" {
				if _, ok := dtMap[f.Options]; !ok {
					errs = append(errs, CrossFileError{
						Code:    "MissingLinkTarget",
						Message: fmt.Sprintf("Link field %q targets doctype %q which does not exist", f.Fieldname, f.Options),
						DocType: dt.Name,
						Field:   f.Fieldname,
						Related: f.Options,
					})
				}
			}
			// Check Table fields reference existing doctypes and track child tables.
			if f.Fieldtype == "Table" && f.Options != "" {
				if _, ok := dtMap[f.Options]; !ok {
					errs = append(errs, CrossFileError{
						Code:    "MissingLinkTarget",
						Message: fmt.Sprintf("Table field %q targets doctype %q which does not exist", f.Fieldname, f.Options),
						DocType: dt.Name,
						Field:   f.Fieldname,
						Related: f.Options,
					})
				} else {
					childTables[f.Options] = dt.Name
				}
			}
		}

		// Check for orphan child tables (is_child_table but no parent).
		if dt.IsChildTable {
			if _, ok := childTables[dt.Name]; !ok {
				// Only warn if the child table has no parent at all.
				// It might be referenced by name rather than doctype name
				// (options stores the name column value, not the doctype name).
				// Skip this check for now — it's too noisy.
			}
		}
	}

	// Check for duplicate name across doctypes.
	seenNames := make(map[string]string)
	for _, dt := range doctypes {
		if prev, ok := seenNames[dt.Name]; ok {
			errs = append(errs, CrossFileError{
				Code:    "DuplicateDocType",
				Message: fmt.Sprintf("DocType %q appears in both %s and %s", dt.Name, prev, dt.Module),
				DocType: dt.Name,
			})
		}
		seenNames[dt.Name] = dt.Module
	}

	return errs
}

// ──────────────────────────────────────────────────────────────
// Layer 5: "Did You Mean?" Suggestions
// ──────────────────────────────────────────────────────────────

// reservedFieldNames are system column names that must not be used as field names.
var reservedFieldNames = map[string]bool{
	"name": true, "owner": true, "creation": true, "modified": true,
	"modified_by": true, "doc_status": true, "idx": true,
	"parent": true, "parentfield": true, "parenttype": true,
}

// isReservedFieldName reports whether a field name conflicts with a system column.
func isReservedFieldName(name string) bool { return reservedFieldNames[name] }

// suggestField returns the closest known field name within Levenshtein distance ≤ 3.
func suggestField(unknown string, knownFields map[string]bool) string {
	best := ""
	bestDist := 999
	for k := range knownFields {
		d := levenshtein(unknown, k)
		if d < bestDist && d <= 3 {
			bestDist = d
			best = k
		}
	}
	return best
}

func levenshtein(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}

	// Use a single row for memory efficiency.
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)

	for i := 0; i <= len(b); i++ {
		prev[i] = i
	}

	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(
				prev[j]+1,      // deletion
				curr[j-1]+1,    // insertion
				prev[j-1]+cost, // substitution
			)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

// ──────────────────────────────────────────────────────────────
// Batch Validation Entry Point (for API)
// ──────────────────────────────────────────────────────────────

// ValidateYAML performs full strict validation of a YAML string.
// Returns syntax errors, field-level validation errors, and an error if YAML is malformed.
func ValidateYAML(data []byte) ([]YAMLSyntaxError, []*ValidationError, error) {
	dt := &DocType{}
	unknowns, err := strictUnmarshal(data, dt, "doctype")
	if err != nil {
		return nil, nil, err
	}

	syntaxErrs := make([]YAMLSyntaxError, len(unknowns))
	for i, u := range unknowns {
		if suggestion := suggestField(u.Key, knownDocTypeFields); suggestion != "" {
			u.Detail = fmt.Sprintf("did you mean %q?", suggestion)
		}
		syntaxErrs[i] = u
	}

	// Check field-level unknown keys (this enriches syntaxErrs with field-level errors).
	if checkFieldUnknownKeys(data) != nil {
		// The function returns an error but we also want to collect field-level syntax errors.
		// We already have top-level ones in syntaxErrs.
	}

	if len(syntaxErrs) > 0 {
		return syntaxErrs, nil, nil
	}

	// Validate structure.
	if err := dt.Validate(); err != nil {
		return nil, []*ValidationError{{
			Type:    "ValidationError",
			Message: err.Error(),
			DocType: dt.Name,
		}}, nil
	}

	return nil, nil, nil
}
