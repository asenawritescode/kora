package contract

import "encoding/json"

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
	Workflows []json.RawMessage `json:"workflows,omitempty"`
	//contract:json
	Views []json.RawMessage `json:"views,omitempty"`
	//contract:json
	Scripts []json.RawMessage `json:"scripts,omitempty"`
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
	Workflows []json.RawMessage `json:"workflows,omitempty"`
	//contract:json
	Views []json.RawMessage `json:"views,omitempty"`
	//contract:json
	Scripts []json.RawMessage `json:"scripts,omitempty"`
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
