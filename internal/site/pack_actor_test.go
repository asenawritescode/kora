package site

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackRejectsActorWrittenToNonUserField(t *testing.T) {
	tests := []struct {
		name      string
		fieldtype string
		wantCode  bool
	}{
		{name: "data field is rejected", fieldtype: "Data", wantCode: true},
		{name: "text field is rejected", fieldtype: "Text", wantCode: true},
		{name: "user field is accepted", fieldtype: "User", wantCode: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files := []PackFile{
				{Path: "doctypes/till_session.yaml", ContentType: "doctype", Content: `name: Till Session
module: POS
fields:
  - fieldname: cashier
    fieldtype: ` + tc.fieldtype + `
    label: Cashier
`},
				{Path: "commands/open_till.yaml", ContentType: "command", Content: `name: open_till
namespace: pos
version: 1
permission: create
input:
  record: Till Session
transaction:
  - create:
      record: Till Session
      values:
        cashier: $actor
        status: Open
`},
			}
			validation, err := ValidatePack(PackManifest{SchemaVersion: 1, Name: "pos-test", Version: "1.0.0"}, files)
			if err != nil {
				t.Fatal(err)
			}
			got := findingsWithCode(validation.Findings, "actor_field_not_user")
			if tc.wantCode && len(got) != 1 {
				t.Fatalf("want one actor_field_not_user finding, got %#v", validation.Findings)
			}
			if !tc.wantCode && len(got) != 0 {
				t.Fatalf("want no actor finding, got %#v", got)
			}
			if tc.wantCode && !strings.Contains(got[0].Message, "Till Session.cashier") {
				t.Fatalf("finding does not name the field: %q", got[0].Message)
			}
		})
	}
}

func TestPOSPackWritesActorOnlyToUserFields(t *testing.T) {
	root := filepath.Join("..", "..", "config", "pos")
	files := append(loadPackFilesAtRoot(t, root), loadPackCommandFiles(t, root)...)
	validation, err := ValidatePack(PackManifest{SchemaVersion: 1, Name: "pos", Version: "1.0.0"}, files)
	if err != nil {
		t.Fatal(err)
	}
	if got := findingsWithCode(validation.Findings, "actor_field_not_user"); len(got) != 0 {
		t.Fatalf("POS pack stores $actor in non-User fields: %#v", got)
	}
}

// loadPackCommandFiles reads the command files of a pack root. The shared
// loader skips commands, so a rule over commands needs them loaded here.
func loadPackCommandFiles(t *testing.T, root string) []PackFile {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "commands", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no command files under %s", root)
	}
	var files []PackFile
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, PackFile{Path: filepath.ToSlash(rel), ContentType: "command", Content: string(content)})
	}
	return files
}

func findingsWithCode(findings []PackFinding, code string) []PackFinding {
	var out []PackFinding
	for _, f := range findings {
		if f.Code == code {
			out = append(out, f)
		}
	}
	return out
}
