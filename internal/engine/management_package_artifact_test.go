package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

type staticManagementPackageArtifact struct {
	artifact ManagementPackageArtifact
	ref      string
}

func (r staticManagementPackageArtifact) ResolvePackage(_ context.Context, ref, _ string, _ []string) (ManagementPackageArtifact, error) {
	if ref != r.ref {
		return ManagementPackageArtifact{}, context.Canceled
	}
	return r.artifact, nil
}

func validManagementPackageCommand(t *testing.T, manifest contract.MarketplacePackageManifest, files []contract.PackFile) contract.ApplyPackage {
	t.Helper()
	canonical, err := contract.CanonicalManifestJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := contract.PackDigest(canonical, files)
	if err != nil {
		t.Fatal(err)
	}
	return contract.ApplyPackage{
		Meta: contract.CommandMeta{IdempotencyKey: "apply-key"}, Tenant: "tenant-1",
		ArtifactRef: "release:retail-1", Digest: digest, ExpectedConfigRevision: "4",
	}
}

func TestVerifiedManagementPackageChecksDigestBeforeReturningSnapshot(t *testing.T) {
	manifest := contract.MarketplacePackageManifest{
		SchemaVersion: 1, PackageID: "retail", ReleaseID: "retail-1", Version: "1.0.0", Kind: "module",
		Public:        contract.ManifestPublic{Slug: "retail", Name: "Retail", Summary: "Retail"},
		ScheduledJobs: []contract.ScheduledJobDefinition{{Name: "nightly", Type: "application_task", Schedule: "0 1 * * *", Config: json.RawMessage(`{"mode":"safe"}`)}},
		Engine:        contract.ManifestEngine{MinVersion: "1.0.0"},
	}
	files := []contract.PackFile{{Path: "doctypes/stock.yaml", Content: "name: Stock\nmodule: Inventory\nfields:\n  - fieldname: title\n    fieldtype: Data\n", ContentType: "doctype"}}
	command := validManagementPackageCommand(t, manifest, files)
	resolver := staticManagementPackageArtifact{artifact: ManagementPackageArtifact{Manifest: manifest, Files: files}, ref: command.ArtifactRef}
	snapshot, scripts, err := verifiedManagementPackage(context.Background(), resolver, command)
	if err != nil {
		t.Fatalf("verifiedManagementPackage() error = %v", err)
	}
	if len(snapshot.DocTypes) != 1 || snapshot.DocTypes[0].Name != "Stock" || len(snapshot.ScheduledJobs) != 1 || snapshot.ScheduledJobs[0].Name != "nightly" || len(scripts) != 0 {
		t.Fatalf("parsed snapshot = %#v, scripts = %#v", snapshot, scripts)
	}

	resolver.artifact.Files[0].Content += "# changed after review\n"
	if snapshot, _, err := verifiedManagementPackage(context.Background(), resolver, command); err == nil || snapshot != nil {
		t.Fatalf("mutated artifact returned snapshot %#v, error %v", snapshot, err)
	}
}

func TestVerifiedManagementPackageRejectsUnboundedArtifacts(t *testing.T) {
	manifest := contract.MarketplacePackageManifest{SchemaVersion: 1, PackageID: "retail", ReleaseID: "retail-1", Version: "1.0.0", Kind: "module"}
	files := make([]contract.PackFile, maxManagementPackageFiles+1)
	for i := range files {
		files[i] = contract.PackFile{Path: "doctypes/stock.yaml", Content: "name: Stock", ContentType: "doctype"}
	}
	command := contract.ApplyPackage{Meta: contract.CommandMeta{IdempotencyKey: "apply-key"}, Tenant: "tenant-1", ArtifactRef: "release:retail-1", Digest: strings.Repeat("a", 64), ExpectedConfigRevision: "4"}
	resolver := staticManagementPackageArtifact{artifact: ManagementPackageArtifact{Manifest: manifest, Files: files}, ref: command.ArtifactRef}
	if snapshot, _, err := verifiedManagementPackage(context.Background(), resolver, command); err == nil || snapshot != nil {
		t.Fatalf("oversized artifact returned snapshot %#v, error %v", snapshot, err)
	}
}

func TestVerifiedManagementPackageBindsScriptBodiesToSnapshot(t *testing.T) {
	manifest := contract.MarketplacePackageManifest{
		SchemaVersion: 1, PackageID: "retail", ReleaseID: "retail-1", Version: "1.0.0", Kind: "module",
		Public: contract.ManifestPublic{Slug: "retail", Name: "Retail", Summary: "Retail"},
		Engine: contract.ManifestEngine{MinVersion: "1.0.0"},
	}
	files := []contract.PackFile{{Path: "scripts/stock_check.js", Content: "return true;", ContentType: "script"}}
	command := validManagementPackageCommand(t, manifest, files)
	resolver := staticManagementPackageArtifact{artifact: ManagementPackageArtifact{Manifest: manifest, Files: files}, ref: command.ArtifactRef}
	snapshot, scripts, err := verifiedManagementPackage(context.Background(), resolver, command)
	if err != nil {
		t.Fatalf("verifiedManagementPackage() error = %v", err)
	}
	if len(snapshot.Scripts) != 1 {
		t.Fatalf("snapshot scripts = %#v, want one script", snapshot.Scripts)
	}
	script := snapshot.Scripts[0]
	if script.Source != "return true;" || scripts[script.ScriptHash] != script.Source {
		t.Fatalf("script snapshot source = %q, verified body = %q", script.Source, scripts[script.ScriptHash])
	}
}
