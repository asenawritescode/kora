package site

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/contract"
)

// Descriptor is the durable registry record of a site: its identity, deployment
// assignment, database coordinates, domains and status. It carries no credentials.
type Descriptor = contract.SiteDescriptor

// DeploymentEntryV2FromDescriptor projects the existing single assignment
// source into the versioned deployment contract. Empty legacy assignments are
// the default deployment; a non-empty runtime_cell_id maps to the same ID
// until the controlled deployment-assignment cutover.
func DeploymentEntryV2FromDescriptor(descriptor Descriptor, directoryRevision uint64) (contract.SiteDirectoryEntryV2, error) {
	cellID := strings.TrimSpace(descriptor.RuntimeCellID)
	if cellID == "" {
		cellID = "default"
	}
	deploymentID, err := NormalizeRuntimeCellID(cellID)
	if err != nil || deploymentID == "" {
		if err == nil {
			err = errors.New("deployment assignment is empty")
		}
		return contract.SiteDirectoryEntryV2{}, err
	}
	entry := contract.SiteDirectoryEntryV2{
		Version: 2, SiteID: contract.TenantID(descriptor.SiteID),
		Aliases: append([]string(nil), descriptor.Aliases...), Status: descriptor.Status,
		ConfigRevision:    contract.Revision(strconv.FormatUint(descriptor.ConfigRevision, 10)),
		DirectoryRevision: contract.Revision(strconv.FormatUint(directoryRevision, 10)),
		DeploymentID:      contract.DeploymentID(deploymentID), DataPlaneRef: descriptor.DataPlaneRef,
	}
	if err := contract.ValidateSiteDirectoryEntryV2(entry); err != nil {
		return contract.SiteDirectoryEntryV2{}, err
	}
	return entry, nil
}

// ValidateDeploymentDirectoryV2 makes Engine reject malformed or ambiguous
// placement metadata before publishing a tenant snapshot. It validates the
// complete credential-free directory and keeps v1 assignment compatibility in
// one conversion path.
func ValidateDeploymentDirectoryV2(descriptors []Descriptor, directoryRevision uint64) error {
	seen := make(map[string]struct{}, len(descriptors))
	for _, descriptor := range descriptors {
		if _, duplicate := seen[descriptor.SiteID]; duplicate {
			return fmt.Errorf("tenant directory contains duplicate site ID %q", descriptor.SiteID)
		}
		entry, err := DeploymentEntryV2FromDescriptor(descriptor, directoryRevision)
		if err != nil {
			return fmt.Errorf("tenant %q has invalid deployment directory metadata: %w", descriptor.SiteID, err)
		}
		seen[string(entry.SiteID)] = struct{}{}
	}
	return nil
}

// DescriptorFromDBSiteInfo makes a safe wire representation of a registered
// site. It never copies database credentials or storage connection details.
func DescriptorFromDBSiteInfo(info DBSiteInfo) Descriptor {
	aliases := make([]string, 0, len(info.Domains)+1)
	seen := make(map[string]struct{}, len(info.Domains)+1)
	for _, alias := range append(append([]string(nil), info.Domains...), info.Name) {
		normalized := NormalizeSiteAlias(alias)
		if normalized == "" {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		aliases = append(aliases, normalized)
	}
	status := info.Status
	if status == "" {
		status = "active"
	}
	return Descriptor{Version: contract.SiteDirectoryVersion, SiteID: info.SiteID, Aliases: aliases, Status: status, ConfigRevision: info.ConfigRevision, RuntimeCellID: strings.TrimSpace(info.RuntimeCellID), DataPlaneRef: DataPlaneReference(info)}
}

// DataPlaneReference is a stable, credential-free identifier for the tenant's
// database and file-storage allocation. It is derived only from Engine-owned
// directory data so management operations can bind recovery checkpoints to
// the exact allocation without exposing connection coordinates.
func DataPlaneReference(info DBSiteInfo) string {
	if strings.TrimSpace(info.DBType) == "" || strings.TrimSpace(info.DBName) == "" {
		return ""
	}
	fileStorage := strings.ToLower(strings.TrimSpace(info.FileStorage))
	if fileStorage == "" {
		fileStorage = "local"
	}
	parts := []string{
		strings.ToLower(strings.TrimSpace(info.DBType)),
		strings.TrimSpace(info.DBHost),
		strconv.Itoa(info.DBPort),
		info.DBName,
		fileStorage,
		info.StorageBucket,
	}
	var canonical strings.Builder
	for _, part := range parts {
		canonical.WriteString(strconv.Itoa(len(part)))
		canonical.WriteByte(':')
		canonical.WriteString(part)
		canonical.WriteByte('|')
	}
	digest := sha256.Sum256([]byte(canonical.String()))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// NormalizeSiteAlias canonicalizes host aliases for directory comparisons.
func NormalizeSiteAlias(alias string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(alias)), ".")
}

// BelongsToDeployment preserves single-Engine behavior when the process
// has no explicit placement. With a configured deployment, a blank legacy
// assignment belongs only to "default"; invalid IDs fail closed.
func BelongsToDeployment(siteDeploymentID, processDeploymentID string) bool {
	processDeploymentID = strings.TrimSpace(processDeploymentID)
	if processDeploymentID == "" {
		return true
	}
	processDeploymentID, err := NormalizeRuntimeCellID(processDeploymentID)
	if err != nil || processDeploymentID == "" {
		return false
	}
	siteDeploymentID = strings.TrimSpace(siteDeploymentID)
	if siteDeploymentID == "" {
		siteDeploymentID = "default"
	}
	siteDeploymentID, err = NormalizeRuntimeCellID(siteDeploymentID)
	return err == nil && siteDeploymentID != "" && siteDeploymentID == processDeploymentID
}

// BelongsToRuntimeCell is the legacy adapter for deployment ownership.
func BelongsToRuntimeCell(siteCellID, processCellID string) bool {
	return BelongsToDeployment(siteCellID, processCellID)
}

func newCanonicalSiteID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return "site_" + hex.EncodeToString(id[:]), nil
}
