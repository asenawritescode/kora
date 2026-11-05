package contract

import "testing"

const testNS = "0123456789abcdef"

func TestValidPackageFileTypeNew(t *testing.T) {
	cases := []struct {
		path, typ string
		want      bool
	}{
		{"queries/a.yaml", "query", true},
		{"queries/sub/a.yml", "query", true},
		{"queries/a.json", "query", false},
		{"views/a.yaml", "query", false},
		{"commands/a.yaml", "command", true},
		{"commands/a.txt", "command", false},
		{"queries/a.yaml", "command", false},
		{"print_formats/a.yml", "print_format", true},
		{"print_format/a.yml", "print_format", false},
		{"pack.yaml", "pack", true},
		{"components/" + testNS + "/pack.yaml", "pack", true},
		{"components/" + testNS + "/sub/pack.yaml", "pack", false},
		{"components/0123456789ABCDEF/pack.yaml", "pack", false},
		{"components/abc/pack.yaml", "pack", false},
		{"other/pack.yaml", "pack", false},
		{"components/" + testNS + "/queries/a.yaml", "query", false},
	}
	for _, c := range cases {
		if got := validPackageFileType(c.path, c.typ); got != c.want {
			t.Errorf("%s as %s = %v, want %v", c.path, c.typ, got, c.want)
		}
	}
}

func TestPackFileComponent(t *testing.T) {
	cases := map[string]string{
		"pack.yaml":                                "",
		"queries/a.yaml":                           "",
		"components/" + testNS + "/pack.yaml":      testNS,
		"queries/components/" + testNS + "/a.yaml": testNS,
		"components/XYZ/pack.yaml":                 "",
		"components/" + testNS:                     "",
	}
	for in, want := range cases {
		if got := PackFileComponent(in); got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
}

func TestPackageFileType(t *testing.T) {
	cases := map[string]string{
		"doctypes/a.yaml": "doctype", "queries/a.yaml": "query", "commands/a.yml": "command",
		"print_formats/a.yaml": "print_format", "pack.yaml": "pack",
		"components/" + testNS + "/pack.yaml": "pack", "scripts/a.js": "script",
	}
	for in, want := range cases {
		if got, ok := PackageFileType(in); !ok || got != want {
			t.Errorf("%q = %q,%v want %q", in, got, ok, want)
		}
	}
	if _, ok := PackageFileType("README.md"); ok {
		t.Error("README.md should not infer")
	}
}

func TestPackDigestGoldenUnchanged(t *testing.T) {
	manifest, err := CanonicalManifestJSON(map[string]any{"name": "x", "version": 1})
	if err != nil {
		t.Fatal(err)
	}
	got, err := PackDigest(manifest, []PackFile{
		{Path: "doctypes/a.yaml", ContentType: "doctype", Content: "name: A\n"},
		{Path: "pack.yaml", ContentType: "pack", Content: "id: x\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = "f5c66cc8734d22c34e99da54d93ea647d5adfe54d3f4501374eeb55c8ac49429"
	if got != want {
		t.Fatalf("digest = %s", got)
	}
}
