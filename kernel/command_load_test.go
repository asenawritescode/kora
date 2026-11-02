package kernel_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/kernel"
)

func TestLoadCommandDir(t *testing.T) {
	dir := t.TempDir()
	valid := `
name: animal.register
namespace: livestock
version: 1
input:
  record: Task
transaction:
  - create:
      record: Task
      values:
        title: $input.title
`
	if err := os.WriteFile(filepath.Join(dir, "a-register.yaml"), []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	// Non-YAML file must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := kernel.LoadCommandDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := reg.Lookup("livestock.animal.register"); !ok {
		t.Fatalf("definition not registered under namespaced key")
	}
	if len(reg.List()) != 1 {
		t.Fatalf("expected exactly 1 definition, got %d", len(reg.List()))
	}
}

func TestLoadCommandDirMissingIsClean(t *testing.T) {
	reg, err := kernel.LoadCommandDir(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil || reg == nil {
		t.Fatalf("missing dir must yield empty registry, got %v/%v", reg, err)
	}
}

func TestLoadCommandDirFailsClosedOnInvalid(t *testing.T) {
	dir := t.TempDir()
	bad := "name: broken\nversion: 1\n" // no input, no steps
	if err := os.WriteFile(filepath.Join(dir, "b.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := kernel.LoadCommandDir(dir)
	if err == nil || !strings.Contains(err.Error(), "b.yaml") {
		t.Fatalf("invalid definition must fail the load with file context, got %v", err)
	}
}

func TestParseCommandResourceRejectsUnimplementedPreconditions(t *testing.T) {
	definition := `
name: issue_invoice
namespace: finance
version: 1
input:
  record: Invoice
preconditions:
  - invoice_is_draft
transaction:
  - create:
      record: Invoice
      values:
        amount: $input.amount
`
	if _, err := kernel.ParseCommandResource([]byte(definition)); err == nil || !strings.Contains(err.Error(), "preconditions are not supported") {
		t.Fatalf("unimplemented precondition must fail command registration, got %v", err)
	}
}

func TestCommandRegistryDuplicateRejected(t *testing.T) {
	r := kernel.NewCommandRegistry()
	def, err := kernel.ParseCommandResource([]byte("name: x.create\nnamespace: n\nversion: 1\ninput:\n  record: T\ntransaction:\n  - create:\n      record: T\n      values:\n        a: b\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(def); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(def); err == nil {
		t.Fatalf("duplicate registration must be rejected")
	}
}

func TestCrossEntityCommandRequiresExplicitStepScopes(t *testing.T) {
	definition := `
name: transfer.apply
namespace: finance
version: 1
input:
  record: Transfer
cross_entity:
  resource: Intercompany Transfer
  operation: write
  target_entity_field: input.target_entity_id
transaction:
  - create:
      record: Transfer
      business_scope: source
      values:
        title: $input.title
  - create:
      record: Transfer
      business_scope: target
      values:
        title: $input.title
`
	parsed, err := kernel.ParseCommandResource([]byte(definition))
	if err != nil || parsed.CrossEntity == nil {
		t.Fatalf("valid registered cross-entity command rejected: command=%+v err=%v", parsed, err)
	}
	unsafe := strings.Replace(definition, "      business_scope: target\n", "", 1)
	if _, err := kernel.ParseCommandResource([]byte(unsafe)); err == nil {
		t.Fatal("cross-entity command with an implicit step scope was registered")
	}
	undeclared := strings.Replace(definition, "cross_entity:\n  resource: Intercompany Transfer\n  operation: write\n  target_entity_field: input.target_entity_id\n", "", 1)
	if _, err := kernel.ParseCommandResource([]byte(undeclared)); err == nil {
		t.Fatal("step scope was accepted without a registered cross-entity operation")
	}
}
