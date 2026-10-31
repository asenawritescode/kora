package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestHTTPManagementPackageArtifactResolverBindsReferenceAndDigest(t *testing.T) {
	manifest := contract.MarketplacePackageManifest{SchemaVersion: 1, PackageID: "inventory", ReleaseID: "inventory-release-1", Version: "1.0.0", Kind: "module", Public: contract.ManifestPublic{Slug: "inventory", Name: "Inventory", Summary: "Inventory"}, Engine: contract.ManifestEngine{MinVersion: "1.0.0"}}
	files := []contract.PackFile{{Path: "doctypes/Stock.yaml", ContentType: "doctype", Content: "name: Stock\n"}}
	digest := strings.Repeat("a", 64)
	components := []string{"inventory-team-1.0.0"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/root/internal/engine/marketplace/releases/"+manifest.ReleaseID || r.URL.Query().Get("digest") != digest || len(r.URL.Query()["optional_release_id"]) != 1 || r.URL.Query()["optional_release_id"][0] != components[0] {
			t.Errorf("artifact request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer artifact-token" {
			t.Errorf("artifact authorization = %q", got)
		}
		w.Header().Set("X-Kora-Artifact-Digest", digest)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Manifest contract.MarketplacePackageManifest `json:"manifest"`
			Files    []contract.PackFile                 `json:"files"`
			Digest   string                              `json:"digest"`
		}{manifest, files, digest})
	}))
	defer server.Close()
	resolver, err := NewHTTPManagementPackageArtifactResolver(server.URL+"/root", "artifact-token", server.Client())
	if err != nil {
		t.Fatal("construct HTTP resolver:", err)
	}
	got, err := resolver.ResolvePackage(context.Background(), manifest.ReleaseID, digest, components)
	if err != nil || got.Manifest.ReleaseID != manifest.ReleaseID || len(got.Files) != 1 || got.Files[0].Content != files[0].Content {
		t.Fatalf("ResolvePackage() = %+v, %v", got, err)
	}
}

func TestHTTPManagementPackageArtifactResolverRejectsUnsafeConfigAndResponse(t *testing.T) {
	for _, base := range []string{"http://example.test", "https://user:password@example.test"} {
		if _, err := NewHTTPManagementPackageArtifactResolver(base, "token", nil); err == nil {
			t.Errorf("accepted unsafe artifact URL %q", base)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Kora-Artifact-Digest", strings.Repeat("b", 64))
		_, _ = w.Write([]byte(`{"manifest":{},"files":[],"digest":"` + strings.Repeat("a", 64) + `"}`))
	}))
	defer server.Close()
	resolver, err := NewHTTPManagementPackageArtifactResolver(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal("construct loopback test resolver:", err)
	}
	if _, err := resolver.ResolvePackage(context.Background(), "release-1", strings.Repeat("a", 64), nil); err == nil {
		t.Fatal("accepted response with mismatched digest header")
	}
}
