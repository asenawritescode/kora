package contract

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ModuleID identifies a durable business ownership boundary. It is independent
// of package identity, tenant identity, and ResourceRef namespace.
//
//contract:wire:v2
type ModuleID string

//contract:enum:v2
type ModuleAccess string

//contract:enum:v2
type MutationMode string

//contract:enum:v2
type OperationEffect string

const (
	ModulePrivate    ModuleAccess    = "private"
	ModulePublished  ModuleAccess    = "published"
	MutationCRUD     MutationMode    = "crud"
	MutationCommands MutationMode    = "commands_only"
	EffectQuery      OperationEffect = "query"
	EffectLocal      OperationEffect = "local_command"
	EffectExternal   OperationEffect = "external_command"
)

//contract:wire:v2
type OwnedResource struct {
	Ref          ResourceRef  `json:"ref"`
	Access       ModuleAccess `json:"access"`
	MutationMode MutationMode `json:"mutation_mode"`
}

//contract:wire:v2
type OperationSpec struct {
	Name   string          `json:"name"`
	Input  SchemaRef       `json:"input"`
	Output SchemaRef       `json:"output"`
	Effect OperationEffect `json:"effect"`
}

//contract:wire:v2
type CapabilityOffer struct {
	Name       CapabilityName  `json:"name"`
	Contract   SchemaRef       `json:"contract"`
	Operations []OperationSpec `json:"operations"`
}

//contract:wire:v2
type CapabilityRequirement struct {
	Name     CapabilityName `json:"name"`
	Contract SchemaRef      `json:"contract"`
	Required bool           `json:"required"`
}

// ModuleInstallation is the persisted lifecycle identity for one tenant module.
//
//contract:wire:v2
type ModuleInstallation struct {
	TenantID       TenantID `json:"tenant_id"`
	ModuleID       ModuleID `json:"module_id"`
	Version        string   `json:"version"`
	ArtifactDigest string   `json:"artifact_digest"`
	State          string   `json:"state"`
	Revision       uint64   `json:"revision"`
}

// ResourceOwnership is durable ownership of a canonical resource identity.
//
//contract:wire:v2
type ResourceOwnership struct {
	TenantID      TenantID    `json:"tenant_id"`
	Resource      ResourceRef `json:"resource"`
	OwnerModuleID ModuleID    `json:"owner_module_id"`
}

// ModuleSnapshot is a complete candidate/published ownership state.
//
//contract:wire:v2
type ModuleSnapshot struct {
	TenantID      TenantID             `json:"tenant_id"`
	Revision      uint64               `json:"revision"`
	Installations []ModuleInstallation `json:"installations"`
	Ownership     []ResourceOwnership  `json:"ownership"`
}

// ModuleStore is the persistence boundary used by activation. CommitActivation
// must atomically compare and increment the snapshot revision with the caller's
// activation transaction.
type ModuleStore interface {
	Load(tenant TenantID) (ModuleSnapshot, error)
}

// ModuleDescriptor is the canonical package level business module contract.
//
//contract:wire:v2
type ModuleDescriptor struct {
	ID        ModuleID                `json:"id"`
	Version   string                  `json:"version"`
	Resources []OwnedResource         `json:"resources,omitempty"`
	Provides  []CapabilityOffer       `json:"provides,omitempty"`
	Requires  []CapabilityRequirement `json:"requires,omitempty"`
}

var moduleIDPattern = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9-]+)+$`)
var moduleSemverPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

func (m ModuleDescriptor) Validate(registeredSchema func(SchemaRef) bool) error {
	if !moduleIDPattern.MatchString(string(m.ID)) {
		return fmt.Errorf("invalid module id %q", m.ID)
	}
	if !moduleSemverPattern.MatchString(m.Version) {
		return fmt.Errorf("module %q requires a semver version", m.ID)
	}
	resources := map[string]bool{}
	for _, resource := range m.Resources {
		key := fmt.Sprintf("%s/%s", resource.Ref.Namespace, resource.Ref.Name)
		if resource.Ref.Namespace == "" || resource.Ref.Name == "" || resource.Ref.Version < 1 {
			return fmt.Errorf("module %q has invalid resource reference", m.ID)
		}
		if resources[key] {
			return fmt.Errorf("module %q duplicates resource %q", m.ID, key)
		}
		resources[key] = true
		if resource.Access != ModulePrivate && resource.Access != ModulePublished {
			return fmt.Errorf("module %q resource %q has invalid access", m.ID, key)
		}
		if resource.MutationMode != MutationCRUD && resource.MutationMode != MutationCommands {
			return fmt.Errorf("module %q resource %q has invalid mutation mode", m.ID, key)
		}
	}
	capabilities := map[string]bool{}
	for _, offer := range m.Provides {
		if offer.Name == "" || capabilities[string(offer.Name)] {
			return fmt.Errorf("module %q has empty or duplicate capability offer %q", m.ID, offer.Name)
		}
		capabilities[string(offer.Name)] = true
		if err := validateModuleSchema(offer.Contract, registeredSchema); err != nil {
			return err
		}
		ops := map[string]bool{}
		for _, op := range offer.Operations {
			if op.Name == "" || ops[op.Name] {
				return fmt.Errorf("module %q capability %q has empty or duplicate operation", m.ID, offer.Name)
			}
			ops[op.Name] = true
			if op.Effect != EffectQuery && op.Effect != EffectLocal && op.Effect != EffectExternal {
				return fmt.Errorf("module %q operation %q has unknown effect %q", m.ID, op.Name, op.Effect)
			}
			if err := validateModuleSchema(op.Input, registeredSchema); err != nil {
				return err
			}
			if err := validateModuleSchema(op.Output, registeredSchema); err != nil {
				return err
			}
		}
	}
	required := map[string]bool{}
	for _, req := range m.Requires {
		if req.Name == "" || required[string(req.Name)] {
			return fmt.Errorf("module %q has empty or duplicate capability requirement", m.ID)
		}
		required[string(req.Name)] = true
		if err := validateModuleSchema(req.Contract, registeredSchema); err != nil {
			return err
		}
	}
	return nil
}

func validateModuleSchema(ref SchemaRef, registered func(SchemaRef) bool) error {
	if strings.TrimSpace(ref.ID) == "" || ref.Version == 0 {
		return fmt.Errorf("schema id and version are required")
	}
	if registered != nil && !registered(ref) {
		return fmt.Errorf("schema %s@%d is not registered", ref.ID, ref.Version)
	}
	return nil
}

// ValidateModuleSet rejects ambiguous ownership and returns deterministic errors.
func ValidateModuleSet(modules []ModuleDescriptor, registered func(SchemaRef) bool) error {
	ids, owners := map[ModuleID]bool{}, map[string]ModuleID{}
	ordered := append([]ModuleDescriptor(nil), modules...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for _, module := range ordered {
		if ids[module.ID] {
			return fmt.Errorf("duplicate module id %q", module.ID)
		}
		ids[module.ID] = true
		if err := module.Validate(registered); err != nil {
			return err
		}
		for _, resource := range module.Resources {
			key := fmt.Sprintf("%s/%s", resource.Ref.Namespace, resource.Ref.Name)
			if owner, ok := owners[key]; ok {
				return fmt.Errorf("resource %q has duplicate owners %q and %q", key, owner, module.ID)
			}
			owners[key] = module.ID
		}
	}
	return nil
}
