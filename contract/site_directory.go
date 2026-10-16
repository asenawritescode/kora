package contract

import (
	"fmt"
	"strings"
)

// SiteDirectoryVersion is the current provider-neutral site directory wire
// contract. Breaking changes require a new versioned contract.
const SiteDirectoryVersion = 1

// SiteDescriptor is the credential-free projection shared by Engine and its
// control-plane consumers. Database connection details are never part of this
// contract; data-plane references are opaque identifiers only.
//
//contract:wire
type SiteDescriptor struct {
	Version           int      `json:"version"`
	SiteID            string   `json:"site_id"`
	Aliases           []string `json:"aliases"`
	Status            string   `json:"status"`
	ConfigRevision    uint64   `json:"config_revision,omitempty"`
	DirectoryRevision uint64   `json:"directory_revision,omitempty"`
	RuntimeCellID     string   `json:"runtime_cell_id,omitempty"`
	DataPlaneRef      string   `json:"data_plane_ref,omitempty"`
}

// SiteDirectoryEntryV2 is the versioned, deployment-neutral projection used
// by deployment ingress. RuntimeCellID remains the v1 storage/wire adapter
// during migration; it is the only writable assignment source until cutover.
//
//contract:wire:v2
type SiteDirectoryEntryV2 struct {
	Version           uint16       `json:"version"`
	SiteID            TenantID     `json:"site_id"`
	Aliases           []string     `json:"aliases"`
	Status            string       `json:"status"`
	ConfigRevision    Revision     `json:"config_revision"`
	DirectoryRevision Revision     `json:"directory_revision"`
	DeploymentID      DeploymentID `json:"deployment_id"`
	DataPlaneRef      string       `json:"data_plane_ref,omitempty"`
}

// ValidateSiteDirectoryEntryV2 checks the safe, versioned routing projection.
// It intentionally does not interpret aliases as authorization grants.
func ValidateSiteDirectoryEntryV2(value SiteDirectoryEntryV2) error {
	if value.Version != 2 {
		return fmt.Errorf("site directory entry version must be 2")
	}
	if _, err := ParseTenantID(string(value.SiteID)); err != nil {
		return fmt.Errorf("site ID: %w", err)
	}
	if strings.TrimSpace(value.Status) == "" || len(value.Status) > 32 {
		return fmt.Errorf("site status must be 1 to 32 bytes")
	}
	if _, err := ParseRevision(string(value.ConfigRevision)); err != nil {
		return fmt.Errorf("config revision: %w", err)
	}
	if _, err := ParseRevision(string(value.DirectoryRevision)); err != nil {
		return fmt.Errorf("directory revision: %w", err)
	}
	if _, err := ParseDeploymentID(string(value.DeploymentID)); err != nil {
		return fmt.Errorf("deployment ID: %w", err)
	}
	if len(value.Aliases) > 50 {
		return fmt.Errorf("site directory entry exceeds 50 aliases")
	}
	seen := make(map[string]struct{}, len(value.Aliases))
	for _, alias := range value.Aliases {
		if err := ValidateTenantAlias(alias); err != nil {
			return err
		}
		if _, exists := seen[alias]; exists {
			return fmt.Errorf("site directory aliases contain a duplicate")
		}
		seen[alias] = struct{}{}
	}
	if value.DataPlaneRef != "" && !managementRefPattern.MatchString(value.DataPlaneRef) {
		return fmt.Errorf("data-plane reference must be opaque")
	}
	return nil
}
