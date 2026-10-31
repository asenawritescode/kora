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
	"time"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/script"
)

func TestSubscriptionsPackModelsBillingRelationshipsAndLifecycle(t *testing.T) {
	files := loadConfigProfilePackFiles(t, "subscriptions")
	validation, err := ValidatePack(PackManifest{SchemaVersion: 1, Name: "Subscription Billing", Version: "1.1.2"}, files)
	if err != nil {
		t.Fatal("validate Subscription Billing pack:", err)
	}
	if !validation.Valid {
		t.Fatalf("Subscription Billing pack findings: %+v", validation.Findings)
	}

	docTypes := map[string]*doctype.DocType{}
	var permissions []*doctype.Permission
	for _, file := range files {
		var parseErr error
		switch file.ContentType {
		case "doctype":
			docTypes[file.Path], parseErr = doctype.ParseYAML([]byte(file.Content), file.Path)
		case "permissions":
			permissions, parseErr = doctype.ParsePermissionsYAML([]byte(file.Content))
		}
		if parseErr != nil {
			t.Fatalf("parse %s: %v", file.Path, parseErr)
		}
	}

	byName := map[string]*doctype.DocType{}
	for _, docType := range docTypes {
		if _, exists := byName[docType.Name]; exists {
			t.Fatalf("duplicate DocType %q", docType.Name)
		}
		byName[docType.Name] = docType
	}
	for _, name := range []string{"Billing Customer", "Plan", "Subscription", "Subscription Line", "Billing Schedule", "Recurring Invoice", "Payment", "Entitlement", "Usage Record"} {
		if byName[name] == nil {
			t.Errorf("missing %s DocType", name)
		}
	}

	assertLink := func(docTypeName, fieldName, target string) {
		t.Helper()
		field := byName[docTypeName].GetField(fieldName)
		if field == nil || field.Fieldtype != "Link" || field.Options != target {
			t.Errorf("%s.%s = %#v; want Link to %s", docTypeName, fieldName, field, target)
		}
	}
	assertLink("Subscription", "customer", "Billing Customer")
	assertLink("Subscription", "plan", "Plan")
	assertLink("Recurring Invoice", "subscription", "Subscription")
	assertLink("Payment", "invoice", "Recurring Invoice")
	assertLink("Usage Record", "entitlement", "Entitlement")

	permissionCoverage := map[string]bool{}
	for _, permission := range permissions {
		if permission.Read {
			permissionCoverage[permission.Doctype] = true
		}
	}
	for name := range byName {
		if !permissionCoverage[name] {
			t.Errorf("%s has no readable role permission", name)
		}
	}
}

func TestSubscriptionsPackBindsAndCompilesBillingHooks(t *testing.T) {
	files := loadConfigProfilePackFiles(t, "subscriptions")
	snapshot, sourceByHash, err := parseConfigPack("subscriptions", files)
	if err != nil {
		t.Fatal("parse Subscription Billing pack:", err)
	}
	want := map[string]bool{
		"Subscription/before_insert":      false,
		"Subscription/validate":           false,
		"Recurring Invoice/before_insert": false,
		"Recurring Invoice/before_save":   false,
		"Payment/before_insert":           false,
		"Payment/before_save":             false,
	}
	runner := script.NewEmbeddedRunner(script.EmbeddedConfig{PoolSize: 1})
	defer runner.Close()
	for _, record := range snapshot.Scripts {
		key := record.DocType + "/" + string(record.Event)
		if _, exists := want[key]; !exists {
			t.Errorf("unexpected or unbound subscription script: %+v", record)
			continue
		}
		want[key] = true
		source := sourceByHash[record.ScriptHash]
		if err := runner.Validate(source); err != nil {
			t.Errorf("compile %s: %v", key, err)
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("pack is missing bound billing hook %s", key)
		}
	}
}

func TestSubscriptionsPackFingerprint(t *testing.T) {
	files := loadConfigProfilePackFiles(t, "subscriptions")
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
	stored, err := os.ReadFile(filepath.Join("..", "config", "v0", "subscriptions", "config_hash.txt"))
	if err != nil {
		t.Fatal("read Subscription Billing pack fingerprint:", err)
	}
	if strings.TrimSpace(string(stored)) != want {
		t.Fatalf("Subscription Billing pack fingerprint = %q, want %q for %d files", strings.TrimSpace(string(stored)), want, len(files))
	}
}

type subscriptionScriptProvider struct {
	docs  map[string]map[string]map[string]any
	lists map[string][]map[string]any
	saved []struct {
		doctype string
		doc     map[string]any
	}
}

func (p *subscriptionScriptProvider) GetDoc(doctypeName, name string) (map[string]any, error) {
	if docs := p.docs[doctypeName]; docs != nil {
		return docs[name], nil
	}
	return nil, nil
}
func (p *subscriptionScriptProvider) GetList(doctypeName string, _ map[string]any, _ string, _, _ int) ([]map[string]any, error) {
	return p.lists[doctypeName], nil
}
func (p *subscriptionScriptProvider) SaveDoc(doctypeName string, doc map[string]any, _ string) error {
	p.saved = append(p.saved, struct {
		doctype string
		doc     map[string]any
	}{doctypeName, doc})
	return nil
}
func (*subscriptionScriptProvider) CreateDoc(string, map[string]any, string, string) (map[string]any, error) {
	return nil, nil
}
func (*subscriptionScriptProvider) DeleteDoc(string, string) error   { return nil }
func (*subscriptionScriptProvider) GetSecret(string) (string, error) { return "", nil }
func (*subscriptionScriptProvider) DoHTTP(*script.HTTPRequest) (*script.HTTPResponse, error) {
	return nil, nil
}

func TestSubscriptionPackScriptsCopyTermsPrepareInvoicesAndAllocatePayments(t *testing.T) {
	files := loadConfigProfilePackFiles(t, "subscriptions")
	snapshot, sourceByHash, err := parseConfigPack("subscriptions", files)
	if err != nil {
		t.Fatal("parse Subscription Billing pack:", err)
	}
	sources := map[string]string{}
	for _, record := range snapshot.Scripts {
		sources[record.DocType+"/"+string(record.Event)] = sourceByHash[record.ScriptHash]
	}
	runner := script.NewEmbeddedRunner(script.EmbeddedConfig{PoolSize: 1})
	defer runner.Close()
	provider := &subscriptionScriptProvider{
		docs: map[string]map[string]map[string]any{
			"Plan":              {"plan-a": {"name": "plan-a", "status": "Active", "unit_price": 12.5, "currency": "USD", "billing_interval": "Quarterly", "interval_count": 3, "tax_rate": 16}},
			"Subscription":      {"sub-a": {"name": "sub-a", "status": "Active", "customer": "customer-a", "plan": "plan-a", "unit_price": 12.5, "quantity": 2, "currency": "USD"}},
			"Recurring Invoice": {"inv-a": {"name": "inv-a", "subscription": "sub-a", "customer": "customer-a", "currency": "USD", "subtotal": 34.8, "tax_amount": 0, "total": 34.8, "amount_paid": 0, "balance": 34.8, "status": "Issued"}},
		},
		lists: map[string][]map[string]any{
			"Subscription Line": {{"unit_price": 2.5, "quantity": 2}},
		},
	}
	execute := func(key, doctypeName string, event script.Event, doc, oldDoc map[string]any) map[string]any {
		t.Helper()
		result, runErr := runner.Execute(context.Background(), script.ExecuteRequest{
			Script: sources[key], ScriptType: script.TypeDocEvent, DocType: doctypeName, Event: event,
			Document: doc, OldDocument: oldDoc, Provider: provider, Timeout: time.Second,
		})
		if runErr != nil {
			t.Fatalf("execute %s: %v", key, runErr)
		}
		if result == nil || !result.Modified {
			t.Fatalf("%s did not return a modified document", key)
		}
		return result.Document
	}

	newSubscription := execute("Subscription/before_insert", "Subscription", script.EventBeforeInsert,
		map[string]any{"plan": "plan-a", "override_plan_terms": false}, nil)
	if newSubscription["unit_price"] != 12.5 || newSubscription["currency"] != "USD" || newSubscription["billing_interval"] != "Quarterly" || newSubscription["interval_count"] != 3 {
		t.Fatalf("plan terms were not copied: %#v", newSubscription)
	}

	newInvoice := execute("Recurring Invoice/before_insert", "Recurring Invoice", script.EventBeforeInsert,
		map[string]any{"subscription": "sub-a", "status": "Draft"}, nil)
	if newInvoice["customer"] != "customer-a" || newInvoice["currency"] != "USD" || newInvoice["subtotal"] != 30.0 || newInvoice["tax_amount"] != 4.8 || newInvoice["total"] != 34.8 || newInvoice["balance"] != 34.8 {
		t.Fatalf("invoice was not prepared from subscription terms and lines: %#v", newInvoice)
	}

	newPayment := execute("Payment/before_insert", "Payment", script.EventBeforeInsert,
		map[string]any{"invoice": "inv-a", "amount": 10.0, "currency": "USD", "status": "Succeeded"}, nil)
	if newPayment["subscription"] != "sub-a" || newPayment["customer"] != "customer-a" || len(provider.saved) != 1 {
		t.Fatalf("payment was not linked/applied: payment=%#v saves=%#v", newPayment, provider.saved)
	}
	if provider.saved[0].doctype != "Recurring Invoice" || provider.saved[0].doc["amount_paid"] != 10.0 || provider.saved[0].doc["status"] != "Partially Paid" {
		t.Fatalf("payment allocation = %#v", provider.saved[0])
	}
}
