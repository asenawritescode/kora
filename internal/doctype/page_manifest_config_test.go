package doctype

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParsePageManifestsDirectoryReadsNativeYAML(t *testing.T) {
	dir := t.TempDir()
	data := []byte("apiVersion: ui.kora.dev/v1\nkind: Page\nmetadata:\n  name: Register\n  version: 1.0.0\n  package: retail\n  status: active\nspec:\n  route: /register\n  runtime: \">=2.0.0 <3.0.0\"\n  permissions: []\n  capabilities: []\n  offline: read_only\n  resources: []\n  actions: []\n  layout:\n    type: single\n    children: []\n")
	if err := os.WriteFile(filepath.Join(dir, "register.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	manifests, err := ParsePageManifestsDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 || manifests[0].Metadata.Name != "Register" || manifests[0].Spec.Route != "/register" {
		t.Fatalf("unexpected parsed manifests: %#v", manifests)
	}
}
