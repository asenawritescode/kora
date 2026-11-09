package engine

import (
	"bytes"
	"context"
	"encoding/json"
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
func verifiedManagementPackage(ctx context.Context, resolver ManagementPackageArtifactResolver, command contract.ApplyPackage) (*doctype.ConfigSnapshot, map[string]string, contract.MarketplacePackageManifest, error) {
	if ctx == nil || resolver == nil {
		return nil, nil, contract.MarketplacePackageManifest{}, fmt.Errorf("package application requires context and an artifact resolver")
	}
	if err := contract.ValidateApplyPackage(command); err != nil {
		return nil, nil, contract.MarketplacePackageManifest{}, fmt.Errorf("invalid package application command: %w", err)
	}
	artifact, err := resolver.ResolvePackage(ctx, command.ArtifactRef, command.Digest, command.OptionalReleaseIDs)
	if err != nil {
		return nil, nil, contract.MarketplacePackageManifest{}, managementPackageResolutionError{cause: fmt.Errorf("resolve immutable package artifact: %w", err)}
	}
	if len(artifact.Files) == 0 || len(artifact.Files) > maxManagementPackageFiles {
		return nil, nil, contract.MarketplacePackageManifest{}, fmt.Errorf("package artifact file count is outside the supported limit")
	}
	total := 0
	for _, file := range artifact.Files {
		total += len(file.Path) + len(file.ContentType) + len(file.Content)
		if total > maxManagementPackageSize {
			return nil, nil, contract.MarketplacePackageManifest{}, fmt.Errorf("package artifact exceeds the supported size limit")
		}
	}
	canonical, err := contract.CanonicalManifestJSON(artifact.Manifest)
	if err != nil {
		return nil, nil, contract.MarketplacePackageManifest{}, fmt.Errorf("canonicalize package manifest: %w", err)
	}
	digest, err := contract.PackDigest(canonical, artifact.Files)
	if err != nil {
		return nil, nil, contract.MarketplacePackageManifest{}, fmt.Errorf("verify package artifact digest: %w", err)
	}
	if digest != command.Digest {
		return nil, nil, contract.MarketplacePackageManifest{}, fmt.Errorf("package artifact digest does not match the accepted operation")
	}
	manifest := contract.PackManifest{
		SchemaVersion: artifact.Manifest.SchemaVersion,
		Name:          artifact.Manifest.PackageID,
		Version:       artifact.Manifest.Version,
		Requires:      artifact.Manifest.Requires,
		Conflicts:     artifact.Manifest.Conflicts,
		Exports:       artifact.Manifest.Exports,
		ScheduledJobs: artifact.Manifest.ScheduledJobs,
		Modules:       artifact.Manifest.Modules,
	}
	if err := contract.ValidateModuleSet(manifest.Modules, nil); err != nil {
		return nil, nil, contract.MarketplacePackageManifest{}, fmt.Errorf("validate package modules: %w", err)
	}
	validation, err := site.ValidatePack(manifest, artifact.Files)
	if err != nil {
		return nil, nil, contract.MarketplacePackageManifest{}, fmt.Errorf("validate package artifact: %w", err)
	}
	if !validation.Valid {
		return nil, nil, contract.MarketplacePackageManifest{}, fmt.Errorf("package artifact failed Engine validation: %v", validation.Findings)
	}
	snapshot, scripts, err := site.ParseConfigPack(artifact.Manifest.PackageID, artifact.Files)
	if err != nil {
		return nil, nil, contract.MarketplacePackageManifest{}, fmt.Errorf("parse package artifact: %w", err)
	}
	snapshot.ScheduledJobs = append(snapshot.ScheduledJobs, artifact.Manifest.ScheduledJobs...)
	modules, err := mergeManifestModules(snapshot.Modules, artifact.Manifest.Modules)
	if err != nil {
		return nil, nil, contract.MarketplacePackageManifest{}, err
	}
	snapshot.Modules = modules
	// Config activation persists scripts from ScriptSnapshot.Source. ParseConfigPack
	// returns the verified bodies separately to preserve its import API, so bind
	// each body to its hash here before the snapshot is journaled. Without this,
	// the config version would name the script hash while SaveScriptsTx could only
	// persist an empty body.
	for _, script := range snapshot.Scripts {
		if script == nil {
			return nil, nil, contract.MarketplacePackageManifest{}, fmt.Errorf("package snapshot contains an empty script entry")
		}
		source, ok := scripts[script.ScriptHash]
		if !ok || source == "" {
			return nil, nil, contract.MarketplacePackageManifest{}, fmt.Errorf("package script %q has no verified source body", script.Name)
		}
		script.Source = source
	}
	for _, module := range snapshot.Modules {
		for _, owned := range module.Resources {
			if owned.Ref.Version < 1 {
				return nil, nil, contract.MarketplacePackageManifest{}, fmt.Errorf("module %q resource %q has invalid version", module.ID, owned.Ref.String())
			}
			found, err := bindOwnedPackageResource(snapshot, module.ID, owned)
			if err != nil {
				return nil, nil, contract.MarketplacePackageManifest{}, err
			}
			if !found {
				return nil, nil, contract.MarketplacePackageManifest{}, fmt.Errorf("module %q owns missing package resource %q", module.ID, owned.Ref.String())
			}
		}
	}
	return snapshot, scripts, artifact.Manifest, nil
}

// bindOwnedPackageResource verifies that every declared package owner points
// at a resource in the immutable artifact. The namespace is the resource kind;
// the resource name is its canonical identity within that kind.
func bindOwnedPackageResource(snapshot *doctype.ConfigSnapshot, owner contract.ModuleID, resource contract.OwnedResource) (bool, error) {
	if snapshot == nil {
		return false, fmt.Errorf("module %q resource %q has no package snapshot", owner, resource.Ref.String())
	}
	setDocTypeOwner := func(dt *doctype.DocType) error {
		dt.ModuleOwnerID = string(owner)
		dt.ModuleAccess = resource.Access
		dt.ModuleOwnershipMode = string(resource.MutationMode)
		dt.BusinessScopePolicy = resource.ScopeKind
		dt.BusinessScopeField = resource.ScopeField
		if resource.ScopeKind == contract.ScopeEntityOwned {
			field := dt.GetField(resource.ScopeField)
			if field == nil || field.Fieldtype != "Link" {
				return fmt.Errorf("module %q resource %q entity scope field must be a Link field", owner, resource.Ref.String())
			}
		}
		return nil
	}
	switch resource.Ref.Namespace {
	case "tenant":
		for _, dt := range snapshot.DocTypes {
			if dt != nil && dt.Name == resource.Ref.Name {
				return true, setDocTypeOwner(dt)
			}
		}
	case "role":
		for _, value := range snapshot.Roles {
			if value != nil && value.Name == resource.Ref.Name {
				return true, nil
			}
		}
	case "permission":
		for _, value := range snapshot.Permissions {
			if value != nil && value.Doctype+"/"+value.Role == resource.Ref.Name {
				return true, nil
			}
		}
	case "workflow":
		for _, value := range snapshot.WorkflowDefinitions {
			if string(value.ID) == resource.Ref.Name {
				return true, nil
			}
		}
	case "view":
		for _, value := range snapshot.Views {
			if value != nil && value.Name == resource.Ref.Name {
				return true, nil
			}
		}
	case "metric":
		for _, value := range snapshot.AnalyticsMetrics {
			if value != nil && value.Name == resource.Ref.Name {
				return true, nil
			}
		}
	case "script":
		for _, value := range snapshot.Scripts {
			if value != nil && value.Name == resource.Ref.Name {
				return true, nil
			}
		}
	case "job":
		for _, value := range snapshot.ScheduledJobs {
			if value.Name == resource.Ref.Name {
				return true, nil
			}
		}
	case "query":
		for _, module := range snapshot.Modules {
			if module.ID != owner {
				continue
			}
			for _, value := range module.Queries {
				if value.Name == resource.Ref.Name {
					return true, nil
				}
			}
		}
	case "projection":
		for _, module := range snapshot.Modules {
			if module.ID != owner {
				continue
			}
			for _, value := range module.Projections {
				if value.ID == resource.Ref.Name {
					return true, nil
				}
			}
		}
	case "command":
		for _, module := range snapshot.Modules {
			if module.ID != owner {
				continue
			}
			for _, offer := range module.Provides {
				for _, operation := range offer.Operations {
					if string(offer.Name)+"."+operation.Name == resource.Ref.Name {
						return true, nil
					}
				}
			}
		}
	case "report":
		for _, value := range snapshot.Reports {
			var report struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(value, &report) == nil && report.Name == resource.Ref.Name {
				return true, nil
			}
		}
	}
	return false, nil
}

// mergeManifestModules appends manifest modules to the generated ones. A
// manifest module with a generated ID must be canonically identical.
func mergeManifestModules(generated, manifest []contract.ModuleDescriptor) ([]contract.ModuleDescriptor, error) {
	byID := make(map[contract.ModuleID]contract.ModuleDescriptor, len(generated))
	for _, m := range generated {
		byID[m.ID] = m
	}
	out := append([]contract.ModuleDescriptor(nil), generated...)
	for _, m := range manifest {
		gen, ok := byID[m.ID]
		if !ok {
			out = append(out, m)
			continue
		}
		a, errA := contract.CanonicalManifestJSON(gen)
		b, errB := contract.CanonicalManifestJSON(m)
		if errA != nil || errB != nil || !bytes.Equal(a, b) {
			return nil, fmt.Errorf("pack.module_mismatch: manifest module %q differs from the module generated from pack.yaml", m.ID)
		}
	}
	return out, nil
}
