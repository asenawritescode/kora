package site

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
)

// These aliases preserve source compatibility while keeping public package
// artifact DTO ownership in contract rather than the Engine implementation.
type PackageManifest = contract.MarketplacePackageManifest
type ManifestPublic = contract.ManifestPublic
type TemplateDetailPageContent = contract.TemplateDetailPageContent
type ManifestPrice = contract.ManifestPrice
type ManifestEngine = contract.ManifestEngine
type ManifestPackRef = contract.ManifestPackRef
type ManifestExports = contract.ManifestExports
type ManifestComponent = contract.ManifestComponent
type ScheduledJobDefinition = contract.ScheduledJobDefinition
type PackManifest = contract.PackManifest
type PackFinding = contract.PackFinding
type PackValidation = contract.PackValidation

// CanonicalManifestJSON emits RFC 8785 compatible JSON for the v1 manifest
// shape. V1 numeric fields are integers in the interoperable IEEE-754 range.
func CanonicalManifestJSON(manifest any) ([]byte, error) {
	return contract.CanonicalManifestJSON(manifest)
}

// ValidatePack checks v1 manifest identity and package file boundaries before
// deeper YAML/schema validation or import.
func ValidatePack(manifest PackManifest, files []PackFile) (PackValidation, error) {
	result := PackValidation{Valid: true, Findings: []PackFinding{}}
	add := func(code, file, message string) {
		result.Valid = false
		result.Findings = append(result.Findings, PackFinding{Code: code, Severity: "error", Path: file, Message: message})
	}
	if manifest.SchemaVersion != 1 {
		add("schema_version", "", "unsupported pack manifest schema version")
	}
	if strings.TrimSpace(manifest.Name) == "" {
		add("name_required", "", "pack name is required")
	}
	if strings.TrimSpace(manifest.Version) == "" {
		add("version_required", "", "pack version is required")
	}
	if err := contract.ValidateManifestPackRefs(manifest.Requires, manifest.Conflicts); err != nil {
		add("manifest_references_invalid", "", err.Error())
	}
	if len(files) == 0 {
		add("files_required", "", "pack has no files")
	}
	if err := contract.ValidateScheduledJobDefinitions(manifest.ScheduledJobs); err != nil {
		add("scheduled_jobs_invalid", "", err.Error())
	}
	if len(manifest.ScheduledJobs) > 64 {
		add("scheduled_jobs_limit", "", "pack contains more than 64 scheduled jobs")
	}
	jobNames := map[string]bool{}
	jobConfigBytes := 0
	for _, job := range manifest.ScheduledJobs {
		name := strings.TrimSpace(job.Name)
		if name == "" || strings.TrimSpace(job.Type) == "" || strings.TrimSpace(job.Schedule) == "" {
			add("scheduled_job_required", "", "scheduled jobs require name, type, and schedule")
			continue
		}
		if jobNames[name] {
			add("scheduled_job_duplicate", "", "scheduled job names must be unique")
		}
		jobNames[name] = true
		if err := contract.ValidateCronSchedule(job.Schedule); err != nil {
			add("scheduled_job_schedule", "", "scheduled job has an unsupported schedule: "+err.Error())
		}
		encoded, err := json.Marshal(job.Config)
		if err != nil {
			add("scheduled_job_config", "", "scheduled job config must contain JSON values only")
			continue
		}
		trimmedConfig := strings.TrimSpace(string(job.Config))
		if len(job.Config) > 0 && (trimmedConfig == "" || trimmedConfig[0] != '{') {
			add("scheduled_job_config", "", "scheduled job config must be a JSON object")
			continue
		}
		jobConfigBytes += len(encoded)
	}
	if jobConfigBytes > 64<<10 {
		add("scheduled_job_config_limit", "", "scheduled job configuration exceeds 64 KiB")
	}
	seen := map[string]bool{}
	resources := newPackResourceNames()
	userFields := map[string]map[string]bool{}
	docTypes := map[string]*doctype.DocType{}
	docTypePaths := map[string]string{}
	type assignmentRule struct{ path, doctype, role, field string }
	var assignmentRules []assignmentRule
	total := 0
	for _, file := range files {
		clean, err := normalizePackPath(file.Path)
		if err != nil {
			add("unsafe_path", file.Path, err.Error())
			continue
		}
		if seen[clean] {
			add("duplicate_path", clean, "duplicate normalized file path")
		} else {
			seen[clean] = true
		}
		if !validPackFileType(clean, file.ContentType) {
			add("content_type", clean, "file extension and content type are not supported")
		}
		if err := validatePackFileSyntax(file); err != nil {
			add("invalid_file", clean, err.Error())
		} else if err := resources.add(file); err != nil {
			add("resource_conflict", clean, err.Error())
		}
		if file.ContentType == "doctype" {
			if definition, err := doctype.ParseYAML([]byte(file.Content), file.Path); err == nil {
				docTypes[strings.ToLower(strings.TrimSpace(definition.Name))] = definition
				docTypePaths[strings.ToLower(strings.TrimSpace(definition.Name))] = clean
				if userFields[definition.Name] == nil {
					userFields[definition.Name] = map[string]bool{}
				}
				for _, field := range definition.Fields {
					if field.Fieldtype == "User" {
						userFields[definition.Name][field.Fieldname] = true
					}
				}
			}
		}
		if file.ContentType == "permissions" {
			if definitions, err := doctype.ParsePermissionsYAML([]byte(file.Content)); err == nil {
				for _, definition := range definitions {
					for _, field := range definition.IfAssigned {
						assignmentRules = append(assignmentRules, assignmentRule{clean, definition.Doctype, definition.Role, field})
					}
				}
			}
		}
		if len(file.Content) > 256*1024 {
			add("file_too_large", clean, "file exceeds 256 KB")
		}
		total += len(file.Content)
	}
	// Link pickers render the target's title_field but send free-text searches
	// to Engine's indexed database query. Require that display title to be both
	// explicitly searchable and indexed wherever the target is in this pack.
	for _, source := range docTypes {
		for _, field := range source.Fields {
			if field.Fieldtype != "Link" {
				continue
			}
			targetKey := strings.ToLower(strings.TrimSpace(field.Options))
			target := docTypes[targetKey]
			if target == nil {
				continue // the selected composition may supply this target
			}
			titleName := strings.TrimSpace(target.TitleField)
			title := target.GetField(titleName)
			if titleName == "" || titleName == "name" || title == nil || title.Hidden || strings.EqualFold(title.Fieldtype, "Password") || !isLinkTitleSearchableType(title.Fieldtype) {
				add("link_title_unsearchable", docTypePaths[targetKey], fmt.Sprintf("%s.%s target %s must declare a visible human-readable title_field", source.Name, field.Fieldname, target.Name))
				continue
			}
			declared := false
			for _, candidate := range strings.Split(target.SearchFields, ",") {
				if strings.TrimSpace(candidate) == titleName {
					declared = true
					break
				}
			}
			if !declared || !title.SearchIndex {
				add("link_title_unsearchable", docTypePaths[targetKey], fmt.Sprintf("%s.%s target title %s.%s must be declared in search_fields and indexed for database picker search", source.Name, field.Fieldname, target.Name, titleName))
			}
		}
	}
	for _, rule := range assignmentRules {
		if !userFields[rule.doctype][rule.field] {
			add("assignment_scope_field", rule.path, fmt.Sprintf("if_assigned field %q for %s/%s must name a User field declared in the package", rule.field, rule.role, rule.doctype))
		}
	}
	if total > 2*1024*1024 {
		add("pack_too_large", "", "pack exceeds 2 MB")
	}
	return result, nil
}

func isLinkTitleSearchableType(fieldType string) bool {
	switch fieldType {
	case "Data", "Text", "Small Text", "Long Text", "Link", "Dynamic Link", "Select", "Email", "Phone", "Code", "Autocomplete":
		return true
	default:
		return false
	}
}

// packResourceNames mirrors the names Engine registries use when importing a
// package. File paths can be distinct while the resources they define collide.
type packResourceNames struct {
	doctypes         map[string]string
	doctypeResources map[string]string
	roles            map[string]string
	permissions      map[string]string
	workflows        map[string]string
	views            map[string]string
	scripts          map[string]string
	reports          map[string]string
}

func newPackResourceNames() packResourceNames {
	return packResourceNames{
		doctypes: map[string]string{}, doctypeResources: map[string]string{}, roles: map[string]string{}, permissions: map[string]string{},
		workflows: map[string]string{}, views: map[string]string{}, scripts: map[string]string{}, reports: map[string]string{},
	}
}

func (names packResourceNames) add(file PackFile) error {
	register := func(namespace map[string]string, name, label string) error {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			return nil
		}
		if previousPath, exists := namespace[key]; exists {
			return fmt.Errorf("%s %q is defined more than once (also in %s)", label, name, previousPath)
		}
		namespace[key] = file.Path
		return nil
	}

	switch file.ContentType {
	case "doctype":
		definition, err := doctype.ParseYAML([]byte(file.Content), file.Path)
		if err != nil {
			return err
		}
		if err := register(names.doctypes, definition.Name, "DocType"); err != nil {
			return err
		}
		return register(names.doctypeResources, definition.ResourceName, "API resource name")
	case "roles":
		definitions, err := doctype.ParseRolesYAML([]byte(file.Content))
		if err != nil {
			return err
		}
		for _, definition := range definitions {
			if err := register(names.roles, definition.Name, "role"); err != nil {
				return err
			}
		}
	case "permissions":
		definitions, err := doctype.ParsePermissionsYAML([]byte(file.Content))
		if err != nil {
			return err
		}
		for _, definition := range definitions {
			if err := register(names.permissions, definition.Role+"/"+definition.Doctype, "role permission"); err != nil {
				return err
			}
		}
	case "workflow_definition":
		var definition contract.WorkflowDefinition
		if err := json.Unmarshal([]byte(file.Content), &definition); err != nil {
			return err
		}
		return register(names.workflows, string(definition.ID), "workflow definition")
	case "view":
		definition, err := doctype.ParseViewYAML([]byte(file.Content))
		if err != nil {
			return err
		}
		if err := register(names.views, definition.Name, "view"); err != nil {
			return err
		}
		return register(names.views, "route:"+definition.Route, "view route")
	case "script":
		name := strings.TrimSuffix(path.Base(file.Path), path.Ext(file.Path))
		if doctypeName, eventName := packScriptBinding(file.Path); doctypeName != "" {
			name = strings.ReplaceAll(doctypeName, " ", "_") + "_" + eventName
		}
		return register(names.scripts, name, "script")
	case "report":
		definition, err := analytics.ParseReportYAML([]byte(file.Content), file.Path)
		if err != nil {
			return err
		}
		if err := register(names.reports, definition.Name, "report"); err != nil {
			return err
		}
		if definition.Route != "" {
			return register(names.views, "route:"+definition.Route, "report route")
		}
	}
	return nil
}

func validatePackFileSyntax(file PackFile) error {
	data := []byte(file.Content)
	switch file.ContentType {
	case "doctype":
		_, err := doctype.ParseYAML(data, file.Path)
		return err
	case "roles":
		_, err := doctype.ParseRolesYAML(data)
		return err
	case "permissions":
		_, err := doctype.ParsePermissionsYAML(data)
		return err
	case "workflow_definition":
		var definition contract.WorkflowDefinition
		if err := json.Unmarshal(data, &definition); err != nil {
			return err
		}
		if definition.ID == "" || definition.Version == 0 || len(definition.Steps) == 0 {
			return fmt.Errorf("workflow definition requires an id, version, and steps")
		}
		return nil
	case "view":
		_, err := doctype.ParseViewYAML(data)
		return err
	case "report":
		_, err := analytics.ParseReportYAML(data, file.Path)
		return err
	case "script":
		if strings.TrimSpace(file.Content) == "" {
			return fmt.Errorf("script file is empty")
		}
		return nil
	case "pack":
		if file.Path != packFileName {
			return fmt.Errorf("content type pack requires path %s", packFileName)
		}
		return nil // parsed and name-checked by ParseConfigPack
	default:
		return fmt.Errorf("unsupported content type %q", file.ContentType)
	}
}

func normalizePackPath(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, '\\') || strings.ContainsRune(value, 0) || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("absolute, empty, or non-portable path rejected")
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return "", fmt.Errorf("path traversal or non-normalized path rejected")
	}
	return clean, nil
}

func validPackFileType(filePath, contentType string) bool {
	ext := strings.ToLower(path.Ext(filePath))
	switch contentType {
	case "doctype":
		return strings.HasPrefix(filePath, "doctypes/") && (ext == ".yaml" || ext == ".yml")
	case "roles":
		return (filePath == "roles.yaml" || (strings.HasPrefix(filePath, "components/") && path.Base(filePath) == "roles.yaml")) && ext == ".yaml"
	case "permissions":
		return (filePath == "permissions.yaml" || (strings.HasPrefix(filePath, "components/") && path.Base(filePath) == "permissions.yaml")) && ext == ".yaml"
	case "workflow_definition":
		return strings.HasPrefix(filePath, "workflows/") && ext == ".json"
	case "view":
		return strings.HasPrefix(filePath, "views/") && (ext == ".yaml" || ext == ".yml")
	case "report":
		return strings.HasPrefix(filePath, "reports/") && (ext == ".yaml" || ext == ".yml")
	case "script":
		return strings.HasPrefix(filePath, "scripts/") && ext == ".js"
	case "pack":
		return filePath == packFileName
	default:
		return false
	}
}

// PackDigest computes the kora-pack-v1 digest over the canonical manifest and
// sorted, length-prefixed files. It also validates paths and content types.
func PackDigest(canonicalManifest []byte, files []PackFile) (string, error) {
	return contract.PackDigest(canonicalManifest, files)
}

// ManifestDigest returns SHA-256 over canonical manifest bytes.
func ManifestDigest(canonical []byte) string {
	return contract.ManifestDigest(canonical)
}

// ParsePackDigest verifies the stored lowercase digest shape.
func ParsePackDigest(value string) error {
	return contract.ParsePackDigest(value)
}
