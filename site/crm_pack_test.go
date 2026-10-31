package site

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/script"
)

func TestCRMPackAssignmentsUseWorkspaceUserPickers(t *testing.T) {
	files, err := os.ReadFile(filepath.Join("..", "config", "v0", "crm", "doctypes", "lead.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	lead, err := doctype.ParseYAML(files, "lead.yaml")
	if err != nil {
		t.Fatal("parse Lead schema:", err)
	}
	assertFieldType := func(doctypeName, fieldName string, fields []doctype.Field, want string) {
		t.Helper()
		for _, field := range fields {
			if field.Fieldname == fieldName {
				if field.Fieldtype != want {
					t.Errorf("%s.%s type = %q; want %q", doctypeName, fieldName, field.Fieldtype, want)
				}
				return
			}
		}
		t.Errorf("%s.%s is missing", doctypeName, fieldName)
	}
	assertFieldType("Lead", "assignee", lead.Fields, "Data")
	assertFieldType("Lead", "assignee_user", lead.Fields, "User")
	assertFieldType("Lead", "status", lead.Fields, "Data")
	assertFieldType("Lead", "lifecycle_stage", lead.Fields, "Select")

	for _, test := range []struct {
		name  string
		file  string
		field string
	}{
		{name: "Deal", file: "deal.yaml", field: "owner_user"},
		{name: "Activity", file: "activity.yaml", field: "assigned_to"},
		{name: "Qualification", file: "qualification.yaml", field: "assessor"},
		{name: "Risk", file: "risk.yaml", field: "owner_user"},
		{name: "Handoff", file: "handoff.yaml", field: "receiving_owner"},
	} {
		content, err := os.ReadFile(filepath.Join("..", "config", "v0", "crm", "doctypes", test.file))
		if err != nil {
			t.Fatal("read "+test.file+":", err)
		}
		doc, err := doctype.ParseYAML(content, test.file)
		if err != nil {
			t.Fatal("parse "+test.name+" schema:", err)
		}
		assertFieldType(test.name, test.field, doc.Fields, "User")
	}
}

func TestCRMPackUsesBusinessKeysWithoutOverconstrainingCompanyNames(t *testing.T) {
	parse := func(file string) *doctype.DocType {
		t.Helper()
		content, err := os.ReadFile(filepath.Join("..", "config", "v0", "crm", "doctypes", file))
		if err != nil {
			t.Fatal("read "+file+":", err)
		}
		doc, err := doctype.ParseYAML(content, file)
		if err != nil {
			t.Fatal("parse "+file+":", err)
		}
		return doc
	}
	field := func(doc *doctype.DocType, name string) *doctype.Field {
		t.Helper()
		for i := range doc.Fields {
			if doc.Fields[i].Fieldname == name {
				return &doc.Fields[i]
			}
		}
		t.Fatalf("%s.%s missing", doc.Name, name)
		return nil
	}
	company := parse("company.yaml")
	if field(company, "company_name").Unique {
		t.Fatal("company name must allow multiple branches under one customer organization")
	}
	for _, test := range []struct{ file, name string }{
		{"sales_quote.yaml", "reference"},
		{"competitor.yaml", "reference"},
	} {
		doc := parse(test.file)
		ref := field(doc, test.name)
		if !ref.Unique {
			t.Errorf("%s.%s must be unique within the tenant as a stable business reference", doc.Name, test.name)
		}
		if !ref.Reqd {
			t.Errorf("%s.%s must be required so each record has a business reference", doc.Name, test.name)
		}
	}
	for _, test := range []struct{ file, name string }{
		{"qualification.yaml", "reference"},
		{"risk.yaml", "reference"},
		{"handoff.yaml", "reference"},
	} {
		doc := parse(test.file)
		ref := field(doc, test.name)
		if ref.Unique || !ref.Reqd {
			t.Errorf("%s.%s should be a required readable title without a forced unique code", doc.Name, test.name)
		}
	}
	contact := parse("contact.yaml")
	for _, name := range []string{"email", "phone", "mobile"} {
		if field(contact, name).Unique {
			t.Errorf("Contact.%s must allow shared or changed contact details", name)
		}
	}
}

func TestCRMPackLinkFieldsResolveToReadableTitles(t *testing.T) {
	files := loadConfigProfilePackFiles(t, "crm")
	snapshot, _, err := parseConfigPack("crm", files)
	if err != nil {
		t.Fatal("parse CRM pack:", err)
	}
	docs := make(map[string]*doctype.DocType, len(snapshot.DocTypes))
	for _, doc := range snapshot.DocTypes {
		docs[doc.Name] = doc
	}
	for _, source := range snapshot.DocTypes {
		for _, field := range source.Fields {
			if field.Fieldtype != "Link" {
				continue
			}
			target := docs[field.Options]
			if target == nil {
				t.Errorf("%s.%s links to missing CRM DocType %q", source.Name, field.Fieldname, field.Options)
				continue
			}
			if target.TitleField == "" || target.TitleField == "name" {
				t.Errorf("%s.%s target %s has no readable title field", source.Name, field.Fieldname, target.Name)
				continue
			}
			title := target.GetField(target.TitleField)
			if title == nil || title.Hidden || title.Fieldtype == "Password" {
				t.Errorf("%s.%s target title %s.%s is unavailable to link pickers", source.Name, field.Fieldname, target.Name, target.TitleField)
			}
		}
	}
}

func TestCRMPackIndexesPickerSearchFields(t *testing.T) {
	files := loadConfigProfilePackFiles(t, "crm")
	snapshot, _, err := parseConfigPack("crm", files)
	if err != nil {
		t.Fatalf("parse CRM pack: %v", err)
	}
	wantIndexed := map[string][]string{
		"Company":  {"company_name", "website", "industry"},
		"Contact":  {"first_name", "last_name", "email", "company"},
		"Lead":     {"lead_name", "reference", "email", "phone", "company", "lifecycle_stage"},
		"Deal":     {"deal_name", "contact", "company"},
		"Activity": {"subject", "contact", "deal"},
	}
	for _, dt := range snapshot.DocTypes {
		for _, rawFieldName := range strings.Split(dt.SearchFields, ",") {
			fieldName := strings.TrimSpace(rawFieldName)
			if fieldName == "" {
				continue
			}
			field := dt.GetField(fieldName)
			if field == nil {
				t.Errorf("%s search_fields references missing field %q", dt.Name, fieldName)
				continue
			}
			if field.Hidden || strings.EqualFold(field.Fieldtype, "Password") || !field.SearchIndex {
				t.Errorf("%s.%s is configured for search but is hidden, sensitive, or lacks a database search index", dt.Name, fieldName)
			}
			switch field.Fieldtype {
			case "Data", "Text", "Small Text", "Long Text", "Link", "Dynamic Link", "Select", "Email", "Phone", "Code", "Autocomplete":
			default:
				t.Errorf("%s.%s has unsupported type %q in search_fields", dt.Name, fieldName, field.Fieldtype)
			}
		}
		for _, fieldName := range wantIndexed[dt.Name] {
			field := dt.GetField(fieldName)
			if field == nil {
				t.Errorf("%s.%s is in the expected picker search index but missing", dt.Name, fieldName)
				continue
			}
			if !field.SearchIndex {
				t.Errorf("%s.%s must be indexed for configured picker search", dt.Name, fieldName)
			}
		}
	}
}

func TestCRMContactNameHooksBuildReadablePickerTitle(t *testing.T) {
	files := loadConfigProfilePackFiles(t, "crm")
	snapshot, bodies, err := parseConfigPack("crm", files)
	if err != nil {
		t.Fatalf("parse CRM pack: %v", err)
	}
	for _, expectedEvent := range []string{"before_insert", "before_save"} {
		var name, source string
		for _, hook := range snapshot.Scripts {
			if hook.DocType == "Contact" && hook.Event == expectedEvent {
				name = hook.Name
				source = bodies[hook.ScriptHash]
				break
			}
		}
		if name == "" || source == "" {
			t.Fatalf("CRM pack is missing Contact %s hook", expectedEvent)
		}
		runner := script.NewEmbeddedRunner(script.EmbeddedConfig{PoolSize: 1})
		result, runErr := runner.Execute(context.Background(), script.ExecuteRequest{
			Script: source, ScriptType: script.TypeDocEvent, ScriptName: name,
			DocType: "Contact", Event: script.Event(expectedEvent),
			Document: map[string]any{"first_name": "  Ada ", "last_name": " Lovelace "},
		})
		closeErr := runner.Close()
		if runErr != nil {
			t.Fatalf("execute Contact %s hook: %v", expectedEvent, runErr)
		}
		if closeErr != nil {
			t.Fatalf("close script runner: %v", closeErr)
		}
		if result.Document["full_name"] != "Ada Lovelace" {
			t.Errorf("Contact full_name = %#v, want readable display name", result.Document["full_name"])
		}
	}
}

func TestCRMPackLegacyFieldsHideWhenEmptyButRemainReadable(t *testing.T) {
	files := loadConfigProfilePackFiles(t, "crm")
	snapshot, _, err := parseConfigPack("crm", files)
	if err != nil {
		t.Fatal("parse CRM pack:", err)
	}
	count := 0
	for _, doc := range snapshot.DocTypes {
		for _, field := range doc.Fields {
			if !strings.HasPrefix(field.Label, "Legacy ") {
				continue
			}
			count++
			if field.Hidden {
				t.Errorf("%s.%s must remain API-readable during compatibility", doc.Name, field.Fieldname)
			}
			if field.DependsOn != field.Fieldname {
				t.Errorf("%s.%s depends_on = %q; want its own value so empty legacy inputs stay off new forms", doc.Name, field.Fieldname, field.DependsOn)
			}
		}
	}
	if count == 0 {
		t.Fatal("expected CRM legacy compatibility fields")
	}
}

func TestCRMPackRepPermissionsGrantOnlyExplicitUserAssignments(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "config", "v0", "crm", "permissions.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	permissions, err := doctype.ParsePermissionsYAML(content)
	if err != nil {
		t.Fatal("parse CRM permissions:", err)
	}
	matrix := doctype.NewPermissionMatrix()
	matrix.LoadPermissionsFromDB(nil, permissions)
	for _, test := range []struct{ doctype, field string }{
		{"Lead", "assignee_user"}, {"Deal", "owner_user"}, {"Activity", "assigned_to"},
	} {
		owner, assigned := matrix.RowScopeFor([]string{"Sales Rep"}, test.doctype, "read")
		if !owner || len(assigned) != 1 || assigned[0] != test.field {
			t.Errorf("%s rep row scope = (%t, %#v), want creator OR %s", test.doctype, owner, assigned, test.field)
		}
	}
	owner, assigned := matrix.RowScopeFor([]string{"Sales Rep"}, "Company", "read")
	if !owner || len(assigned) != 0 {
		t.Fatalf("unassigned Company access widened: owner=%t fields=%#v", owner, assigned)
	}
	owner, assigned = matrix.RowScopeFor([]string{"Sales Rep", "Sales Manager"}, "Lead", "read")
	if owner || len(assigned) != 0 {
		t.Fatalf("manager role did not grant the configured team scope: owner=%t fields=%#v", owner, assigned)
	}
	if matrix.Can("Sales Rep", "Activity", "delete") || matrix.Can("Sales Manager", "Activity", "delete") {
		t.Fatal("Sales Rep or Sales Manager can hard-delete Activity history; close or cancel it through the workflow instead")
	}
	if !matrix.Can("Administrator", "Activity", "delete") {
		t.Fatal("Administrator emergency cleanup permission changed unexpectedly")
	}
}

func TestCRMPackValidatesWithoutLegacyWorkflowConfiguration(t *testing.T) {
	files := loadConfigProfilePackFiles(t, "crm")
	validation, err := ValidatePack(PackManifest{SchemaVersion: 1, Name: "CRM and Sales Pipeline", Version: "1.0.0"}, files)
	if err != nil {
		t.Fatal("validate CRM pack:", err)
	}
	if !validation.Valid {
		t.Fatalf("CRM pack findings: %+v", validation.Findings)
	}
	hasWorkspace := false
	for _, file := range files {
		if file.Path == "views/crm_workspace.yaml" {
			hasWorkspace = true
		}
		if file.ContentType == "workflow_definition" || strings.HasPrefix(file.Path, "workflows/") {
			t.Fatalf("CRM pack still includes retired workflow configuration %q", file.Path)
		}
	}
	if !hasWorkspace {
		t.Fatal("CRM pack is missing its workspace view")
	}
	snapshot, _, err := parseConfigPack("crm", files)
	if err != nil {
		t.Fatal("parse CRM pack:", err)
	}
	if len(snapshot.WorkflowDefinitions) != 0 {
		t.Fatalf("CRM pack imported %d retired workflow definitions", len(snapshot.WorkflowDefinitions))
	}
}
func TestCRMPackFingerprintMatchesAllPackFiles(t *testing.T) {
	files := loadConfigProfilePackFiles(t, "crm")
	ordered := append([]PackFile(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	hash := sha256.New()
	for _, file := range ordered {
		_, _ = hash.Write([]byte(file.Path))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(file.Content))
		_, _ = hash.Write([]byte{0})
	}
	want := hex.EncodeToString(hash.Sum(nil))
	stored, err := os.ReadFile(filepath.Join("..", "config", "v0", "crm", "config_hash.txt"))
	if err != nil {
		t.Fatal("read CRM pack fingerprint:", err)
	}
	if strings.TrimSpace(string(stored)) != want {
		t.Fatalf("CRM pack fingerprint = %q, want %q for %d files", strings.TrimSpace(string(stored)), want, len(files))
	}
}

func TestCRMPackPreservesLegacyLeadAndDealColumnTypes(t *testing.T) {
	legacyLeadYAML := `name: Lead
module: CRM
fields:
  - fieldname: assignee
    fieldtype: Data
  - fieldname: status
    fieldtype: Data
`
	legacyDealYAML := `name: Deal
module: CRM
fields:
  - fieldname: status
    fieldtype: Data
`
	legacyLead, err := doctype.ParseYAML([]byte(legacyLeadYAML), "legacy-lead.yaml")
	if err != nil {
		t.Fatal(err)
	}
	legacyDeal, err := doctype.ParseYAML([]byte(legacyDealYAML), "legacy-deal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	files := loadConfigProfilePackFiles(t, "crm")
	current, _, err := parseConfigPack("crm", files)
	if err != nil {
		t.Fatal(err)
	}
	currentByName := map[string]*doctype.DocType{}
	for _, dt := range current.DocTypes {
		currentByName[dt.Name] = dt
	}
	for _, test := range []struct {
		old      *doctype.DocType
		oldField string
		newField string
		want     string
	}{
		{old: legacyLead, oldField: "assignee", newField: "assignee", want: "Data"},
		{old: legacyLead, oldField: "status", newField: "status", want: "Data"},
		{old: legacyDeal, oldField: "status", newField: "status", want: "Data"},
	} {
		newDT := currentByName[test.old.Name]
		if newDT == nil {
			t.Fatalf("updated pack removed legacy DocType %s", test.old.Name)
		}
		oldType, newType := "", ""
		for _, field := range test.old.Fields {
			if field.Fieldname == test.oldField {
				oldType = field.Fieldtype
			}
		}
		for _, field := range newDT.Fields {
			if field.Fieldname == test.newField {
				newType = field.Fieldtype
			}
		}
		if oldType != test.want || newType != oldType {
			t.Fatalf("%s.%s type changed from %q to %q; legacy column must remain compatible", test.old.Name, test.oldField, oldType, newType)
		}
	}
	diff := doctype.DiffConfigs([]*doctype.DocType{legacyLead, legacyDeal}, []*doctype.DocType{currentByName["Lead"], currentByName["Deal"]})
	for _, change := range diff.Changes {
		if change.Type == doctype.ChangeFieldTypeChanged {
			t.Fatalf("CRM pack still changes an existing field type: %+v", change)
		}
	}
}
