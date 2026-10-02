package site

import (
	"crypto/rand"
	"encoding/hex"
	"strings"

	"github.com/asenawritescode/kora/contract"
)

type SiteDescriptor = contract.SiteDescriptor

// DescriptorFromDBSiteInfo makes a safe wire representation of a registered
// site. It never copies database credentials or storage connection details.
func DescriptorFromDBSiteInfo(info DBSiteInfo) SiteDescriptor {
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
	return SiteDescriptor{Version: contract.SiteDirectoryVersion, SiteID: info.SiteID, Aliases: aliases, Status: status, ConfigRevision: info.ConfigRevision, RuntimeCellID: strings.TrimSpace(info.RuntimeCellID)}
}

// NormalizeSiteAlias canonicalizes host aliases for directory comparisons.
func NormalizeSiteAlias(alias string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(alias)), ".")
}

// SiteBelongsToRuntimeCell preserves legacy single-cell behavior when the
// process has no explicit assignment. With an explicit cell, blank assignments
// are treated as "default" and never duplicated into non-default cells.
func SiteBelongsToRuntimeCell(siteCellID, processCellID string) bool {
	processCellID = strings.ToLower(strings.TrimSpace(processCellID))
	if processCellID == "" {
		return true
	}
	siteCellID = strings.ToLower(strings.TrimSpace(siteCellID))
	if siteCellID == "" {
		siteCellID = "default"
	}
	return siteCellID == processCellID
}

func newCanonicalSiteID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return "site_" + hex.EncodeToString(id[:]), nil
}
