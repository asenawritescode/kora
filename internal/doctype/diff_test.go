package doctype

import "testing"

func TestConfigDiffFull_NilDoctypesIsSafe(t *testing.T) {
	diff := &ConfigDiffFull{
		SectionChanges: []SectionChange{{Section: "views", Change: "modified", Name: "POS"}},
	}
	if got := diff.Summary(); got != "0 added, 0 removed, 0 changed (0 breaking), 1 section changes" {
		t.Fatalf("Summary() = %q", got)
	}
	if got := diff.BreakingChanges(); got != nil {
		t.Fatalf("BreakingChanges() = %#v, want nil", got)
	}
}

func TestDiffFullSnapshotsIncludesModifiedAccessSections(t *testing.T) {
	before := &ConfigSnapshot{
		Roles:       []*Role{{Name: "Shop Staff", WorkspaceAccess: true}},
		Permissions: []*Permission{{Doctype: "Stock Count", Role: "Shop Staff", Read: true, Create: true}},
	}
	after := &ConfigSnapshot{
		Roles:       []*Role{{Name: "Shop Staff", WorkspaceAccess: true, Description: "Prepares draft counts"}},
		Permissions: []*Permission{{Doctype: "Stock Count", Role: "Shop Staff", Read: true, Create: true, Write: true}},
	}

	diff := DiffFullSnapshots(before, after)
	if got, want := len(diff.SectionChanges), 2; got != want {
		t.Fatalf("section change count = %d, want %d: %#v", got, want, diff.SectionChanges)
	}
	want := []SectionChange{
		{Section: "permissions", Change: "modified", Name: "Stock Count|Shop Staff", Details: `Permission "Stock Count|Shop Staff" updated`},
		{Section: "roles", Change: "modified", Name: "Shop Staff", Details: `role "Shop Staff" updated`},
	}
	for i := range want {
		if diff.SectionChanges[i] != want[i] {
			t.Fatalf("section change[%d] = %#v, want %#v", i, diff.SectionChanges[i], want[i])
		}
	}
	if got, want := diff.Summary(), "0 added, 0 removed, 0 changed (0 breaking), 2 section changes"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}
