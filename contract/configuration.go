package contract

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ConfigurationVersionSummaryV2 identifies a stored tenant configuration.
// It is intended for authenticated management clients that need to anchor a
// proposed change to the current active or draft version.
//
//contract:wire:v2
type ConfigurationVersionSummaryV2 struct {
	ID      string `json:"id"`
	Site    string `json:"site"`
	Version int    `json:"version"`
	Status  string `json:"status"`
}

// ConfigurationSnapshotResponseV2 returns a versioned declarative snapshot;
// it contains configuration metadata only, never tenant business records.
//
//contract:wire:v2
type ConfigurationSnapshotResponseV2 struct {
	VersionID        string   `json:"version_id"`
	Version          int      `json:"version"`
	Status           string   `json:"status"`
	Site             string   `json:"site"`
	Label            string   `json:"label"`
	DoctypeNames     []string `json:"doctype_names"`
	DoctypeCount     int      `json:"doctype_count"`
	RolesCount       int      `json:"roles_count"`
	PermissionsCount int      `json:"permissions_count"`
	//contract:json
	Snapshot json.RawMessage `json:"snapshot"`
	//contract:json
	PackFiles []ConfigurationSnapshotFileV2 `json:"pack_files"`
}

//contract:wire:v2
type ConfigurationSnapshotFileV2 struct {
	Path        string `json:"path"`
	Content     string `json:"content"`
	ContentType string `json:"content_type"`
}

// ConfigurationCandidateV2 is the read-only onboarding validation input.
// Resource bodies remain JSON because their versioned Engine schemas evolve
// independently; this envelope fixes the cross-repository field names.
//
//contract:wire:v2
type ConfigurationCandidateV2 struct {
	ProposalID          string `json:"proposal_id,omitempty"`
	ConversationID      string `json:"conversation_id,omitempty"`
	BaseConfigVersionID string `json:"base_config_version_id,omitempty"`
	//contract:json
	Doctypes []json.RawMessage `json:"doctypes"`
	//contract:json
	Roles []json.RawMessage `json:"roles,omitempty"`
	//contract:json
	Permissions []json.RawMessage `json:"permissions,omitempty"`
	//contract:json
	Views []json.RawMessage `json:"views,omitempty"`
	//contract:json
	Scripts []json.RawMessage `json:"scripts,omitempty"`
	// V2 workflow definitions use the same strict decoder as package imports.
	//contract:json
	WorkflowDefinitions []json.RawMessage `json:"workflow_definitions,omitempty"`
	//contract:maxItems=512
	PublishedBindings []ConfigurationSurfaceBinding `json:"published_bindings,omitempty"`
}

// ConfigurationSurfaceKind identifies an owner-published surface referenced
// by a generated page or view.
//
//contract:enum:v2
type ConfigurationSurfaceKind string

const (
	ConfigurationQuery      ConfigurationSurfaceKind = "query"
	ConfigurationProjection ConfigurationSurfaceKind = "projection"
	ConfigurationOperation  ConfigurationSurfaceKind = "operation"
)

// ConfigurationSurfaceBinding pins generated UI to one exact registered
// module surface. It is descriptive configuration, never an authorization
// grant. Execution must still authorize the current actor and business scope.
//
//contract:wire:v2
type ConfigurationSurfaceBinding struct {
	Kind            ConfigurationSurfaceKind `json:"kind"`
	CatalogRevision uint64                   `json:"catalog_revision"`
	BusinessScope   BusinessScope            `json:"business_scope"`
	//contract:maxLength=128
	//contract:pattern=^[a-z][a-z0-9_.-]{0,127}$
	OwnerModuleID ModuleID `json:"owner_module_id"`
	//contract:maxLength=160
	Name         string     `json:"name,omitempty"`
	OperationRef string     `json:"operation_ref,omitempty"`
	InputSchema  *SchemaRef `json:"input_schema,omitempty"`
	OutputSchema SchemaRef  `json:"output_schema"`
}

func (b ConfigurationSurfaceBinding) Validate() error {
	if b.CatalogRevision == 0 || b.BusinessScope.Validate() != nil || !moduleIDPattern.MatchString(string(b.OwnerModuleID)) || b.OutputSchema.ID == "" || b.OutputSchema.Version == 0 {
		return fmt.Errorf("published surface binding identity, scope, revision, and output schema are required")
	}
	if b.InputSchema != nil && (b.InputSchema.ID == "" || b.InputSchema.Version == 0) {
		return fmt.Errorf("published surface binding input schema is invalid")
	}
	switch b.Kind {
	case ConfigurationQuery:
		if !validConfigurationSurfaceName(b.Name) || b.OperationRef != "" || b.InputSchema == nil {
			return fmt.Errorf("query binding requires one name and input/output schemas")
		}
	case ConfigurationProjection:
		if !validConfigurationSurfaceName(b.Name) || b.OperationRef != "" || b.InputSchema != nil {
			return fmt.Errorf("projection binding requires one name and output schema")
		}
	case ConfigurationOperation:
		parts := strings.Split(b.OperationRef, "~")
		if b.Name != "" || b.InputSchema == nil || len(parts) != 3 || parts[0] != string(b.OwnerModuleID) || !validModuleName(parts[1]) || !validModuleName(parts[2]) || PublishedOperationRef(b.OwnerModuleID, CapabilityName(parts[1]), parts[2]) != b.OperationRef {
			return fmt.Errorf("operation binding requires its canonical operation reference and schemas")
		}
	default:
		return fmt.Errorf("published surface binding kind is invalid")
	}
	return nil
}

func validConfigurationSurfaceName(value string) bool {
	return value != "" && len(value) <= 160 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "~/\\")
}

func ValidateConfigurationSurfaceBindings(bindings []ConfigurationSurfaceBinding) error {
	if len(bindings) > 512 {
		return fmt.Errorf("published surface binding count exceeds its limit")
	}
	seen := make(map[string]bool, len(bindings))
	for _, binding := range bindings {
		if err := binding.Validate(); err != nil {
			return err
		}
		key := string(binding.BusinessScope.TenantID) + "/" + string(binding.BusinessScope.Kind) + "/" + binding.BusinessScope.LegalEntityID + "/" + string(binding.Kind) + "/" + string(binding.OwnerModuleID) + "/" + binding.Name + "/" + binding.OperationRef
		if seen[key] {
			return fmt.Errorf("published surface binding is duplicated")
		}
		seen[key] = true
	}
	return nil
}

//contract:wire:v2
type ConfigurationConflict struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

//contract:wire:v2
type ConfigurationImpact struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Action string `json:"action"`
	Safe   bool   `json:"safe"`
}

// ConfigurationCheckV2 is the shared read-only result of onboarding
// configuration validation and impact analysis.
//
//contract:wire:v2
type ConfigurationCheckV2 struct {
	Valid             bool                    `json:"valid"`
	OrderedDocTypes   []string                `json:"ordered_doctypes"`
	BaseConfigVersion string                  `json:"base_config_version_id,omitempty"`
	Errors            []string                `json:"errors,omitempty"`
	Warnings          []string                `json:"warnings,omitempty"`
	Conflicts         []ConfigurationConflict `json:"conflicts,omitempty"`
	Impact            []ConfigurationImpact   `json:"impact,omitempty"`
	//contract:maxItems=512
	PublishedBindings []ConfigurationSurfaceBinding `json:"published_bindings,omitempty"`
	ModuleBlockers    []ModuleBlocker               `json:"module_blockers,omitempty"`
}

// ConfigurationDraftV2 is Cloud's request to compose an inactive Engine
// configuration version. Resource bodies remain JSON values because their
// domain schemas are validated by Engine; callers must not treat this as a
// generic activation or tenant mutation API.
//
//contract:wire:v2
type ConfigurationDraftV2 struct {
	ProposalID           string `json:"proposal_id"`
	DraftConfigVersionID string `json:"draft_config_version_id,omitempty"`
	ConversationID       string `json:"conversation_id,omitempty"`
	BaseConfigVersionID  string `json:"base_config_version_id,omitempty"`
	// CreatedBy is retained for source compatibility. Engine attribution comes
	// from the authenticated execution identity, never this caller field.
	CreatedBy string `json:"created_by,omitempty"`
	//contract:json
	DocTypes []json.RawMessage `json:"doctypes"`
	//contract:json
	Roles []json.RawMessage `json:"roles,omitempty"`
	//contract:json
	Permissions []json.RawMessage `json:"permissions,omitempty"`
	//contract:json
	Views []json.RawMessage `json:"views,omitempty"`
	//contract:json
	Scripts       []json.RawMessage        `json:"scripts,omitempty"`
	ScheduledJobs []ScheduledJobDefinition `json:"scheduled_jobs,omitempty"`
	//contract:json
	WorkflowDefinitions []json.RawMessage `json:"workflow_definitions,omitempty"`
	//contract:maxItems=512
	PublishedBindings []ConfigurationSurfaceBinding `json:"published_bindings,omitempty"`
}

// ConfigurationDraftResultV2 identifies the one inactive version composed
// from a candidate. The response does not imply eligibility or activation.
//
//contract:wire:v2
type ConfigurationDraftResultV2 struct {
	ProposalID           string   `json:"proposal_id"`
	ConversationID       string   `json:"conversation_id,omitempty"`
	DraftConfigVersionID string   `json:"draft_config_version_id"`
	VersionNum           int      `json:"version_num"`
	Status               string   `json:"status"`
	OrderedDocTypes      []string `json:"ordered_doctypes"`
}
