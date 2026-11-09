package site

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedMarketplaceContractFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/marketplace/v1/contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Manifest       PackageManifest `json:"manifest"`
		Files          []PackFile      `json:"files"`
		ManifestSHA256 string          `json:"manifest_sha256"`
		ArtifactSHA256 string          `json:"artifact_sha256"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalManifestJSON(fixture.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestHash := ManifestDigest(canonical)
	artifactHash, err := PackDigest(canonical, fixture.Files)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.ManifestSHA256 == "" || fixture.ArtifactSHA256 == "" || manifestHash != fixture.ManifestSHA256 || artifactHash != fixture.ArtifactSHA256 {
		t.Fatalf("fixture hashes manifest=%s artifact=%s; expected manifest=%s artifact=%s", manifestHash, artifactHash, fixture.ManifestSHA256, fixture.ArtifactSHA256)
	}
}

func TestPRDTemplatePackagesHaveSearchableLinkedTitles(t *testing.T) {
	packs := []string{
		"small-business", "accounting", "invoicing", "expense", "budgeting", "kora-mvp", "erp_kenya", "erp",
		"crm", "marketing", "helpdesk", "customer-success", "inventory", "purchasing",
		"retail-pos", "ecommerce", "wholesale", "subscriptions", "projectmgmt", "professional-services",
		"fieldwork", "maintenance", "construction", "manufacturing", "quality", "fleet", "logistics",
		"hr", "recruitment", "payroll", "school", "clinic", "pharmacy", "membership", "hotel",
		"restaurant", "propertymgmt", "agriculture", "ngo", "sacco", "events", "documents", "internal-requests",
	}
	for _, pack := range packs {
		t.Run(pack, func(t *testing.T) {
			root := filepath.Join("..", "..", "config", "v0", pack)
			if pack == "retail-pos" || pack == "kora-mvp" {
				root = filepath.Join("..", "..", "config", pack)
			}
			files := loadPackFilesAtRoot(t, root)
			validation, err := ValidatePack(PackManifest{SchemaVersion: 1, Name: pack, Version: "1.0.0"}, files)
			if err != nil {
				t.Fatal(err)
			}
			for _, finding := range validation.Findings {
				if finding.Code == "link_title_unsearchable" {
					t.Errorf("%s: %s", finding.Path, finding.Message)
				}
			}
			if pack == "retail-pos" {
				assertWorkflowDefaultsMatchInitialStates(t, files)
			}
		})
	}
}

func assertWorkflowDefaultsMatchInitialStates(t *testing.T, files []PackFile) {
	t.Helper()
	for _, file := range files {
		if file.ContentType == "workflow_definition" || strings.HasPrefix(file.Path, "workflows/") {
			t.Errorf("retail POS pack still contains retired lifecycle workflow %q", file.Path)
		}
	}
}

func loadPackFilesAtRoot(t *testing.T, root string) []PackFile {
	t.Helper()
	var files []PackFile
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		contentType := ""
		switch {
		case rel == "roles.yaml":
			contentType = "roles"
		case rel == "permissions.yaml":
			contentType = "permissions"
		case strings.HasPrefix(rel, "workflows/") && strings.HasSuffix(rel, ".json"):
			contentType = "workflow_definition"
		case strings.HasPrefix(rel, "doctypes/"):
			contentType = "doctype"
		case strings.HasPrefix(rel, "views/"):
			contentType = "view"
		case strings.HasPrefix(rel, "reports/"):
			contentType = "report"
		case strings.HasPrefix(rel, "scripts/"):
			contentType = "script"
		default:
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, PackFile{Path: rel, ContentType: contentType, Content: string(content)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func testPackageManifest() PackageManifest {
	return PackageManifest{
		SchemaVersion: 1,
		PackageID:     "retail",
		ReleaseID:     "retail-1.0.0",
		Version:       "1.0.0",
		Kind:          "module",
		Public:        ManifestPublic{Slug: "retail", Name: "Retail", Summary: "Track <sales> & stock"},
		Engine:        ManifestEngine{MinVersion: "1.0.0"},
	}
}

func TestCanonicalManifestJSONUsesJCSKeyOrderAndStringEscaping(t *testing.T) {
	got, err := CanonicalManifestJSON(testPackageManifest())
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(text, `\\u003c`) || strings.Contains(text, `\\u0026`) {
		t.Fatalf("HTML characters were escaped: %s", text)
	}
	if !strings.Contains(text, `"package_id":"retail","public":`) {
		t.Fatalf("manifest keys are not canonical: %s", text)
	}
	if strings.Index(text, `"engine"`) > strings.Index(text, `"kind"`) {
		t.Fatalf("object keys are not sorted: %s", text)
	}
}

func TestPackDigestIsIndependentOfInputFileOrder(t *testing.T) {
	manifest, err := CanonicalManifestJSON(testPackageManifest())
	if err != nil {
		t.Fatal(err)
	}
	a := PackFile{Path: "doctypes/sale.yaml", ContentType: "doctype", Content: "name: Sale\n"}
	b := PackFile{Path: "roles.yaml", ContentType: "roles", Content: "roles: []\n"}
	first, err := PackDigest(manifest, []PackFile{a, b})
	if err != nil {
		t.Fatal(err)
	}
	second, err := PackDigest(manifest, []PackFile{b, a})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("file order changed digest: %s != %s", first, second)
	}
	if err := ParsePackDigest(first); err != nil {
		t.Fatal(err)
	}
}

func TestPackDigestRejectsNonCanonicalManifestAndUnsafeFiles(t *testing.T) {
	files := []PackFile{{Path: "roles.yaml", ContentType: "roles", Content: "roles: []\n"}}
	if _, err := PackDigest([]byte(`{"z":1,"a":2}`), files); err == nil {
		t.Fatal("expected noncanonical manifest rejection")
	}
	manifest, err := CanonicalManifestJSON(testPackageManifest())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../roles.yaml", "roles/../roles.yaml", "/roles.yaml", `roles\\roles.yaml`} {
		bad := []PackFile{{Path: path, ContentType: "roles", Content: "roles: []\n"}}
		if _, err := PackDigest(manifest, bad); err == nil {
			t.Errorf("expected path rejection for %q", path)
		}
	}
	dup := []PackFile{
		{Path: "roles.yaml", ContentType: "roles", Content: "roles: []\n"},
		{Path: "roles.yaml", ContentType: "roles", Content: "roles: []\n"},
	}
	if _, err := PackDigest(manifest, dup); err == nil {
		t.Fatal("expected duplicate path rejection")
	}
}

func TestValidatePackReportsUnsupportedRuntimeManifestAndFiles(t *testing.T) {
	result, err := ValidatePack(PackManifest{SchemaVersion: 9}, []PackFile{{Path: "../unsafe.yaml", ContentType: "doctype"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Valid || len(result.Findings) < 2 {
		t.Fatalf("expected validation findings, got %#v", result)
	}
}

func TestValidatePackValidatesScheduledJobDefinitions(t *testing.T) {
	files := []PackFile{{Path: "doctypes/invoice.yaml", ContentType: "doctype", Content: "name: Invoice\nmodule: Billing\nfields: []\n"}}
	manifest := PackManifest{
		SchemaVersion: 1, Name: "billing", Version: "1.0.0",
		ScheduledJobs: []ScheduledJobDefinition{{Name: "daily", Type: "subscription_invoice", Schedule: "0 0 * * *", Config: json.RawMessage(`{"due_days":14}`)}},
	}
	result, err := ValidatePack(manifest, files)
	if err != nil {
		t.Fatal("ValidatePack:", err)
	}
	if !result.Valid {
		t.Fatalf("valid scheduled job pack was rejected: %#v", result.Findings)
	}

	manifest.ScheduledJobs[0].Schedule = "@daily"
	result, err = ValidatePack(manifest, files)
	if err != nil {
		t.Fatal("ValidatePack:", err)
	}
	if result.Valid {
		t.Fatal("unsupported cron syntax was accepted")
	}
	for _, finding := range result.Findings {
		if finding.Code == "scheduled_job_schedule" {
			return
		}
	}
	t.Fatalf("missing scheduled job schedule finding: %#v", result.Findings)
}

func TestValidatePackRejectsCrossFileResourceNameCollisions(t *testing.T) {
	files := []PackFile{
		{Path: "doctypes/order.yaml", ContentType: "doctype", Content: "name: Order\nmodule: Sales\nfields: []\n"},
		{Path: "doctypes/order_copy.yaml", ContentType: "doctype", Content: "name: order\nmodule: Sales\nfields: []\n"},
	}
	result, err := ValidatePack(PackManifest{SchemaVersion: 1, Name: "retail", Version: "1.0.0"}, files)
	if err != nil {
		t.Fatal(err)
	}
	if result.Valid {
		t.Fatalf("expected resource collision to fail validation: %#v", result)
	}
	for _, finding := range result.Findings {
		if finding.Code == "resource_conflict" && finding.Path == "doctypes/order_copy.yaml" {
			return
		}
	}
	t.Fatalf("expected a field-linked duplicate resource finding, got %#v", result.Findings)
}

func TestValidatePackRejectsDuplicateEngineResourceNames(t *testing.T) {
	files := []PackFile{
		{Path: "doctypes/invoice.yaml", ContentType: "doctype", Content: "name: Invoice\nmodule: Sales\nresource_name: ledger-entry\nfields: []\n"},
		{Path: "doctypes/payment.yaml", ContentType: "doctype", Content: "name: Payment\nmodule: Sales\nresource_name: ledger-entry\nfields: []\n"},
	}
	result, err := ValidatePack(PackManifest{SchemaVersion: 1, Name: "retail", Version: "1.0.0"}, files)
	if err != nil {
		t.Fatal(err)
	}
	if result.Valid {
		t.Fatalf("expected duplicate resource_name to fail validation: %#v", result)
	}
	for _, finding := range result.Findings {
		if finding.Code == "resource_conflict" && finding.Path == "doctypes/payment.yaml" {
			return
		}
	}
	t.Fatalf("expected a finding for the resource name collision, got %#v", result.Findings)
}

func TestValidatePackRequiresAssignmentScopeToNameAUserField(t *testing.T) {
	manifest := PackManifest{SchemaVersion: 1, Name: "assigned tasks", Version: "1.0.0"}
	files := []PackFile{
		{Path: "doctypes/task.yaml", ContentType: "doctype", Content: "name: Task\nmodule: Work\nfields:\n  - fieldname: assigned_to\n    fieldtype: Data\n"},
		{Path: "permissions.yaml", ContentType: "permissions", Content: "- doctype: Task\n  role: Worker\n  can_read: true\n  if_assigned: [assigned_to]\n"},
	}
	result, err := ValidatePack(manifest, files)
	if err != nil {
		t.Fatal(err)
	}
	if result.Valid {
		t.Fatal("assignment scope accepted a non-User field")
	}
	found := false
	for _, finding := range result.Findings {
		found = found || finding.Code == "assignment_scope_field"
	}
	if !found {
		t.Fatalf("missing assignment scope finding: %#v", result.Findings)
	}
	files[0].Content = "name: Task\nmodule: Work\nfields:\n  - fieldname: assigned_to\n    fieldtype: User\n"
	result, err = ValidatePack(manifest, files)
	if err != nil || !result.Valid {
		t.Fatalf("valid User assignment pack rejected: %#v err=%v", result, err)
	}
}

func TestValidatePackRequiresLinkedDisplayTitleToBeDatabaseSearchable(t *testing.T) {
	manifest := PackManifest{SchemaVersion: 1, Name: "contacts", Version: "1.0.0"}
	source := PackFile{Path: "doctypes/order.yaml", ContentType: "doctype", Content: "name: Order\nmodule: Sales\nfields:\n  - fieldname: customer\n    fieldtype: Link\n    options: Customer\n"}
	target := PackFile{Path: "doctypes/customer.yaml", ContentType: "doctype", Content: "name: Customer\nmodule: Sales\ntitle_field: display_name\nsearch_fields: display_name, email\nfields:\n  - fieldname: display_name\n    fieldtype: Data\n    search_index: true\n  - fieldname: email\n    fieldtype: Data\n    search_index: true\n"}
	result, err := ValidatePack(manifest, []PackFile{source, target})
	if err != nil || !result.Valid {
		t.Fatalf("indexed human title should validate: %#v err=%v", result, err)
	}

	for _, invalidTarget := range []string{
		"name: Customer\nmodule: Sales\ntitle_field: display_name\nsearch_fields: email\nfields:\n  - fieldname: display_name\n    fieldtype: Data\n    search_index: true\n  - fieldname: email\n    fieldtype: Data\n    search_index: true\n",
		"name: Customer\nmodule: Sales\ntitle_field: display_name\nsearch_fields: display_name\nfields:\n  - fieldname: display_name\n    fieldtype: Data\n",
		"name: Customer\nmodule: Sales\ntitle_field: display_name\nsearch_fields: display_name\nfields:\n  - fieldname: display_name\n    fieldtype: Int\n    search_index: true\n",
	} {
		invalid := target
		invalid.Content = invalidTarget
		result, err = ValidatePack(manifest, []PackFile{source, invalid})
		if err != nil {
			t.Fatalf("validate invalid title pack: %v", err)
		}
		if result.Valid {
			t.Fatalf("unsearchable linked title was accepted: %#v", result)
		}
		found := false
		for _, finding := range result.Findings {
			found = found || finding.Code == "link_title_unsearchable"
		}
		if !found {
			t.Fatalf("missing link title search finding: %#v", result.Findings)
		}
	}

}
