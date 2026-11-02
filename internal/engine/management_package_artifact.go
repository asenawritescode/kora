package engine

import (
	"context"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/site"
)

const (
	maxManagementPackageFiles = 256
	maxManagementPackageSize  = 2 << 20
)

// ManagementPackageArtifact is an immutable marketplace release projection.
// The resolver is deployment-owned and must resolve only configured providers;
// it must never interpret ArtifactRef as a URL, path, or database coordinate.
type ManagementPackageArtifact struct {
	Manifest contract.MarketplacePackageManifest
	Files    []contract.PackFile
}

type ManagementPackageArtifactResolver interface {
	ResolvePackage(context.Context, string, string, []string) (ManagementPackageArtifact, error)
}

type managementPackageResolutionError struct{ cause error }

func (e managementPackageResolutionError) Error() string { return e.cause.Error() }
func (e managementPackageResolutionError) Unwrap() error { return e.cause }

// verifiedManagementPackage resolves, bounds, hashes, validates and parses the
// release before any tenant database or runtime state is touched.
func verifiedManagementPackage(ctx context.Context, resolver ManagementPackageArtifactResolver, command contract.ApplyPackage) (*doctype.ConfigSnapshot, map[string]string, error) {
	if ctx == nil || resolver == nil {
		return nil, nil, fmt.Errorf("package application requires context and an artifact resolver")
	}
	if err := contract.ValidateApplyPackage(command); err != nil {
		return nil, nil, fmt.Errorf("invalid package application command: %w", err)
	}
	artifact, err := resolver.ResolvePackage(ctx, command.ArtifactRef, string(command.Digest), command.OptionalReleaseIDs)
	if err != nil {
		return nil, nil, managementPackageResolutionError{cause: fmt.Errorf("resolve immutable package artifact: %w", err)}
	}
	if len(artifact.Files) == 0 || len(artifact.Files) > maxManagementPackageFiles {
		return nil, nil, fmt.Errorf("package artifact file count is outside the supported limit")
	}
	total := 0
	for _, file := range artifact.Files {
		total += len(file.Path) + len(file.ContentType) + len(file.Content)
		if total > maxManagementPackageSize {
			return nil, nil, fmt.Errorf("package artifact exceeds the supported size limit")
		}
	}
	canonical, err := contract.CanonicalManifestJSON(artifact.Manifest)
	if err != nil {
		return nil, nil, fmt.Errorf("canonicalize package manifest: %w", err)
	}
	digest, err := contract.PackDigest(canonical, artifact.Files)
	if err != nil {
		return nil, nil, fmt.Errorf("verify package artifact digest: %w", err)
	}
	if digest != string(command.Digest) {
		return nil, nil, fmt.Errorf("package artifact digest does not match the accepted operation")
	}
	manifest := contract.PackManifest{
		SchemaVersion: artifact.Manifest.SchemaVersion,
		Name:          artifact.Manifest.PackageID,
		Version:       artifact.Manifest.Version,
		Requires:      artifact.Manifest.Requires,
		Exports:       artifact.Manifest.Exports,
		ScheduledJobs: artifact.Manifest.ScheduledJobs,
		Modules:       artifact.Manifest.Modules,
	}
	if err := contract.ValidateModuleSet(manifest.Modules, nil); err != nil {
		return nil, nil, fmt.Errorf("validate package modules: %w", err)
	}
	validation, err := site.ValidatePack(manifest, artifact.Files)
	if err != nil {
		return nil, nil, fmt.Errorf("validate package artifact: %w", err)
	}
	if !validation.Valid {
		return nil, nil, fmt.Errorf("package artifact failed Engine validation")
	}
	snapshot, scripts, err := site.ParseConfigPack(artifact.Manifest.PackageID, artifact.Files)
	if err != nil {
		return nil, nil, fmt.Errorf("parse package artifact: %w", err)
	}
	snapshot.ScheduledJobs = append(snapshot.ScheduledJobs, artifact.Manifest.ScheduledJobs...)
	snapshot.Modules = append(snapshot.Modules, artifact.Manifest.Modules...)
	// Config activation persists scripts from ScriptSnapshot.Source. ParseConfigPack
	// returns the verified bodies separately to preserve its import API, so bind
	// each body to its hash here before the snapshot is journaled. Without this,
	// the config version would name the script hash while SaveScriptsTx could only
	// persist an empty body.
	for _, script := range snapshot.Scripts {
		if script == nil {
			return nil, nil, fmt.Errorf("package snapshot contains an empty script entry")
		}
		source, ok := scripts[script.ScriptHash]
		if !ok || source == "" {
			return nil, nil, fmt.Errorf("package script %q has no verified source body", script.Name)
		}
		script.Source = source
	}
	for _, module := range snapshot.Modules {
		for _, owned := range module.Resources {
			if owned.Ref.Namespace != "tenant" {
				return nil, nil, fmt.Errorf("module %q resource %q must use tenant resource namespace", module.ID, owned.Ref.String())
			}
			if owned.Ref.Version < 1 {
				return nil, nil, fmt.Errorf("module %q resource %q has invalid version", module.ID, owned.Ref.String())
			}
			found := false
			for _, dt := range snapshot.DocTypes {
				if dt != nil && dt.Name == owned.Ref.Name {
					found = true
					dt.ModuleOwnerID = string(module.ID)
					dt.ModuleOwnershipMode = string(owned.MutationMode)
					break
				}
			}
			if !found {
				return nil, nil, fmt.Errorf("module %q owns missing package DocType %q", module.ID, owned.Ref.Name)
			}
		}
	}
	return snapshot, scripts, nil
}
