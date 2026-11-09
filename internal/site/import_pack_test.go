package site

import (
	"strings"
	"testing"
)

func TestParseConfigPackIsPureAndReturnsSnapshotAndScriptBodies(t *testing.T) {
	files := []PackFile{
		{Path: "doctypes/sale.yaml", ContentType: "doctype", Content: "name: Sale\nmodule: Sales\nfields:\n  - fieldname: total\n    fieldtype: Currency\n"},
		{Path: "roles.yaml", ContentType: "roles", Content: "- name: Cashier\n"},
		{Path: "scripts/sale.js", ContentType: "script", Content: "return true;"},
	}
	snapshot, scripts, err := ParseConfigPack("retail", files)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.DocTypes) != 1 || snapshot.DocTypes[0].Name != "Sale" {
		t.Fatalf("parsed doctypes = %#v", snapshot.DocTypes)
	}
	if len(snapshot.Roles) != 1 || snapshot.Roles[0].Name != "Cashier" {
		t.Fatalf("parsed roles = %#v", snapshot.Roles)
	}
	if len(snapshot.Scripts) != 1 || scripts[snapshot.Scripts[0].ScriptHash] != "return true;" {
		t.Fatalf("parsed script snapshot/body mismatch: %#v %#v", snapshot.Scripts, scripts)
	}
}

func TestParseConfigPackBindsLifecycleScriptsFromExplicitPath(t *testing.T) {
	files := []PackFile{
		{Path: "scripts/Contact/before_insert.js", ContentType: "script", Content: "return true;"},
		{Path: "scripts/Contact/before_save.js", ContentType: "script", Content: "return true;"},
		{Path: "scripts/unbound.js", ContentType: "script", Content: "return true;"},
	}
	snapshot, _, err := ParseConfigPack("crm", files)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ name, doctype, event string }{
		{"Contact_before_insert", "Contact", "before_insert"},
		{"Contact_before_save", "Contact", "before_save"},
		{"unbound", "", ""},
	}
	if len(snapshot.Scripts) != len(want) {
		t.Fatalf("script count = %d, want %d", len(snapshot.Scripts), len(want))
	}
	for i, expected := range want {
		got := snapshot.Scripts[i]
		if got.Name != expected.name || got.DocType != expected.doctype || got.Event != expected.event {
			t.Errorf("script[%d] = (%q, %q, %q), want (%q, %q, %q)", i, got.Name, got.DocType, got.Event, expected.name, expected.doctype, expected.event)
		}
	}
}

func TestParseConfigPackRejectsUnsafeOrOversizedInputBeforeReturningSnapshot(t *testing.T) {
	for _, files := range [][]PackFile{
		{{Path: "../sale.yaml", ContentType: "doctype", Content: "name: Sale\nmodule: Sales\n"}},
		{{Path: "doctypes/sale.yaml", ContentType: "doctype", Content: strings.Repeat("x", 256*1024+1)}},
	} {
		if snapshot, _, err := ParseConfigPack("retail", files); err == nil || snapshot != nil {
			t.Fatalf("ParseConfigPack() = (%v, error %v), expected fail-closed nil snapshot", snapshot, err)
		}
	}
}
