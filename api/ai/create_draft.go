package ai

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
)

func executeListDoctypes(reg *doctype.Registry) string {
	var lines []string
	for _, dt := range reg.All() {
		if dt.IsChildTable {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s (%d fields)", dt.Name, len(dt.DataFields())))
	}
	if len(lines) == 0 {
		return "No doctypes found."
	}
	return strings.Join(lines, "\n")
}

func executeValidateYAML(yamlStr string) string {
	if yamlStr == "" {
		return "Error: no YAML content provided."
	}
	syntaxErrs, validationErrs, err := doctype.ValidateYAML([]byte(yamlStr))
	if err != nil {
		return fmt.Sprintf("Error parsing YAML: %v", err)
	}
	if len(syntaxErrs) == 0 && len(validationErrs) == 0 {
		return "YAML is valid."
	}
	var parts []string
	for _, e := range syntaxErrs {
		parts = append(parts, fmt.Sprintf("Line %d: %s (%s)", e.Line, e.Message, e.Detail))
	}
	for _, e := range validationErrs {
		parts = append(parts, fmt.Sprintf("Validation: %s (field: %s)", e.Message, e.Field))
	}
	return strings.Join(parts, "\n")
}

func executeUpdateDoctypeDraft(tx *orm.TxManager, reg *doctype.Registry, yamlStr, owner, siteName string) string {
	if yamlStr == "" {
		return "Error: no YAML content provided. Use validate_doctype_yaml first."
	}

	// 1. Validate YAML.
	syntaxErrs, validationErrs, err := doctype.ValidateYAML([]byte(yamlStr))
	if err != nil {
		return fmt.Sprintf("Error parsing YAML: %v", err)
	}
	if len(syntaxErrs) > 0 || len(validationErrs) > 0 {
		var msgs []string
		for _, e := range syntaxErrs {
			msgs = append(msgs, fmt.Sprintf("Line %d: %s", e.Line, e.Message))
		}
		for _, e := range validationErrs {
			msgs = append(msgs, fmt.Sprintf("Validation: %s", e.Message))
		}
		return fmt.Sprintf("YAML validation failed:\n%s\n\nFix errors and validate again before updating.", strings.Join(msgs, "\n"))
	}

	// 2. Parse YAML into DocType struct.
	var dt doctype.DocType
	if err := yaml.Unmarshal([]byte(yamlStr), &dt); err != nil {
		return fmt.Sprintf("Error parsing YAML: %v", err)
	}
	if dt.Name == "" {
		return "Error: doctype 'name' is required in the YAML."
	}

	// 3. Check the doctype exists.
	if !reg.Has(dt.Name) {
		return fmt.Sprintf("Error: DocType %q does not exist. Use create_doctype_draft to create new doctypes.", dt.Name)
	}

	// 4. Run full validation.
	if err := dt.Validate(); err != nil {
		return fmt.Sprintf("Error validating doctype: %v", err)
	}

	// 5. Save the update as a Draft version ONLY — do not modify the live doctype.
	// The existing doctype stays as-is in _kora_doctype and the registry until activation.
	original := reg.Get(dt.Name) // save reference to restore after snapshot
	reg.Register(&dt)            // temporarily register updated version for snapshot
	store := configstore.NewStore(tx.DB, tx.Dialect)
	snapshot, _ := store.CollectSnapshot(reg, siteName)
	reg.Register(original)       // restore original — live doctype unchanged
	if owner == "" || owner == "mcp-agent" {
		owner = "ai-assistant"
	}
	verID, verNum, err := store.CreateConfigVersion(
		siteName, owner, "Updated "+dt.Name+" via AI (Draft)", "Draft", snapshot,
	)
	if err != nil {
		slog.Warn("config version creation failed", "error", err)
	}

	fields := len(dt.DataFields())
	return fmt.Sprintf(
		"✓ Updated DocType %q as DRAFT (%d fields). Version #%d (ID: %s). A human must review and activate it before it takes effect.",
		dt.Name, fields, verNum, verID,
	)
}

func executeCreateDoctypeDraft(tx *orm.TxManager, reg *doctype.Registry, yamlStr, owner, siteName string) string {
	if yamlStr == "" {
		return "Error: no YAML content provided. Use validate_doctype_yaml first."
	}

	// 1. Validate YAML.
	syntaxErrs, validationErrs, err := doctype.ValidateYAML([]byte(yamlStr))
	if err != nil {
		return fmt.Sprintf("Error parsing YAML: %v", err)
	}
	if len(syntaxErrs) > 0 || len(validationErrs) > 0 {
		var msgs []string
		for _, e := range syntaxErrs {
			msgs = append(msgs, fmt.Sprintf("Line %d: %s", e.Line, e.Message))
		}
		for _, e := range validationErrs {
			msgs = append(msgs, fmt.Sprintf("Validation: %s", e.Message))
		}
		return fmt.Sprintf("YAML validation failed:\n%s\n\nFix errors and validate again before creating.", strings.Join(msgs, "\n"))
	}

	// 2. Parse YAML into DocType struct.
	var dt doctype.DocType
	if err := yaml.Unmarshal([]byte(yamlStr), &dt); err != nil {
		return fmt.Sprintf("Error parsing YAML: %v", err)
	}
	if dt.Name == "" {
		return "Error: doctype 'name' is required in the YAML."
	}

	// 3. Check for duplicate.
	if reg.Has(dt.Name) {
		return fmt.Sprintf("Error: DocType %q already exists.", dt.Name)
	}

	// 4. Run full validation on the parsed struct.
	if err := dt.Validate(); err != nil {
		return fmt.Sprintf("Error validating doctype: %v", err)
	}

	// 5. Register temporarily to collect a complete snapshot.
	reg.Register(&dt)
	store := configstore.NewStore(tx.DB, tx.Dialect)
	snapshot, _ := store.CollectSnapshot(reg, siteName)
	// Remove from the runtime registry — Draft doctypes only exist in the
	// config version snapshot until activation. SaveDocType is called during
	// activation, not creation.
	reg.Unregister(dt.Name)
	if owner == "" || owner == "mcp-agent" {
		owner = "ai-assistant"
	}
	verID, verNum, err := store.CreateConfigVersion(
		siteName, owner, "Created "+dt.Name+" via AI (Draft)", "Draft", snapshot,
	)
	if err != nil {
		slog.Warn("config version creation failed", "error", err)
	}

	fields := len(dt.DataFields())
	return fmt.Sprintf(
		"✓ Created DocType %q as DRAFT (%d fields). Version #%d (ID: %s). A human must review and activate it before it takes effect.",
		dt.Name, fields, verNum, verID,
	)
}

func formatCell(fieldname string, v any) string {
	if v == nil {
		return ""
	}
	s := fmt.Sprintf("%v", v)
	// Format known decimal fields to 2 places.
	if strings.Contains(s, ".") && len(s) > 4 {
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return fmt.Sprintf("%.2f", n)
		}
	}
	// Boolean → emoji checkmark.
	if s == "1" && (strings.Contains(fieldname, "is_") || strings.Contains(fieldname, "available") ||
		fieldname == "completed") {
		return "✅"
	}
	if s == "0" && (strings.Contains(fieldname, "is_") || strings.Contains(fieldname, "available") ||
		fieldname == "completed") {
		return "❌"
	}
	return s
}
