package engine

import (
	"testing"

	"github.com/asenawritescode/kora/contract"
)

// A release whose installed record predates DocType records is reapplied and its
// record refreshed with the artifact's DocType names, not refused as a mismatch.
func TestMergeManagementPackageConstraintsRefreshesLegacyRecord(t *testing.T) {
	legacy := contract.InstalledPackage{PackageID: "pos", ReleaseID: "r1", Version: "1", Digest: "d1"}
	incoming := legacy
	incoming.DocTypes = []string{"Stock Entry"}
	merged, err := mergeManagementPackageConstraints([]contract.InstalledPackage{legacy}, incoming)
	if err != nil {
		t.Fatalf("reapply legacy release: %v", err)
	}
	if len(merged) != 1 || len(merged[0].DocTypes) != 1 || merged[0].DocTypes[0] != "Stock Entry" {
		t.Fatalf("merged = %+v, want one record carrying the DocType names", merged)
	}
	changed := incoming
	changed.Version = "2"
	if _, err := mergeManagementPackageConstraints([]contract.InstalledPackage{incoming}, changed); err == nil {
		t.Fatal("a changed release with the same ID was accepted")
	}
}
