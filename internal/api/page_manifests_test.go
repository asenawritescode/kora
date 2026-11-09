package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/gin-gonic/gin"
)

func TestPageManifestPurposeSeparatesPlainListsFromCustomWorkflows(t *testing.T) {
	plainList := &doctype.PageManifest{Metadata: doctype.PageManifestMetadata{Name: "Daily Sales"}, Spec: doctype.PageManifestSpec{
		Route:     "/daily-sales",
		Resources: []doctype.PageResource{{ID: "primary", Query: "document.list", Params: map[string]any{"doctype": "Daily Sale"}}},
		Layout: doctype.PageManifestLayout{Children: []doctype.PageComponent{{
			Component: "record_table", Data: "primary.data", Props: map[string]any{"source_doctype": "Daily Sale"},
		}}},
	}}
	if got := pageManifestPurpose(plainList); got != "record_list" {
		t.Fatalf("plain table purpose = %q, want record_list", got)
	}
	if got := pageManifestSourceDocType(plainList); got != "Daily Sale" {
		t.Fatalf("table source = %q, want Daily Sale", got)
	}

	filteredTable := *plainList
	filteredTable.Spec.Resources = []doctype.PageResource{{ID: "primary", Query: "document.list", Params: map[string]any{
		"doctype": "Daily Sale", "filters": []map[string]any{{"field": "status", "op": "=", "value": "Open"}},
	}}}
	if got := pageManifestPurpose(&filteredTable); got != "table_view" {
		t.Fatalf("filtered table purpose = %q, want table_view", got)
	}

	custom := *plainList
	custom.Spec.Layout.Children = append(custom.Spec.Layout.Children, doctype.PageComponent{Component: "payment_panel"})
	if got := pageManifestPurpose(&custom); got != "workflow" {
		t.Fatalf("multi-component purpose = %q, want workflow", got)
	}

	withAction := *plainList
	withAction.Spec.Actions = []doctype.PageAction{{ID: "checkout", Command: "create_transaction"}}
	if got := pageManifestPurpose(&withAction); got != "workflow" {
		t.Fatalf("action screen purpose = %q, want workflow", got)
	}
}

func TestPageManifestTaskOutcomeIsAvailableForAdministratorReview(t *testing.T) {
	manifest := &doctype.PageManifest{Spec: doctype.PageManifestSpec{Task: &doctype.ViewTask{
		User: "Cashier", Trigger: "At shift close", Outcome: "Reconcile cash and M-Pesa totals",
		Reads: []string{"Sale"}, CompletionSignal: "Shift marked reviewed",
	}}}
	if got := pageManifestTaskOutcome(manifest); got != "Reconcile cash and M-Pesa totals" {
		t.Fatalf("task outcome = %q", got)
	}
	if got := pageManifestTaskOutcome(&doctype.PageManifest{}); got != "" {
		t.Fatalf("taskless page outcome = %q, want empty", got)
	}
}

func TestPageManifestEquivalent_UsesSemanticNormalization(t *testing.T) {
	a := &doctype.PageManifest{
		APIVersion: "ui.kora.dev/v1",
		Kind:       "Page",
		Metadata: doctype.PageManifestMetadata{
			Name:    "demos-table",
			Version: "0.1.0",
			Package: "tenant.workspace",
			Status:  "draft",
		},
		Spec: doctype.PageManifestSpec{
			Route:        "/demos",
			Runtime:      ">=2.0.0 <3.0.0",
			Offline:      "read_only",
			Layout:       doctype.PageManifestLayout{Type: "table", Columns: 12},
			Permissions:  nil,
			Capabilities: nil,
			Resources:    nil,
			Actions:      nil,
		},
	}

	b := &doctype.PageManifest{
		APIVersion: "ui.kora.dev/v1",
		Kind:       "Page",
		Metadata: doctype.PageManifestMetadata{
			Name:    "demos-table",
			Version: "0.1.0",
			Package: "tenant.workspace",
			Status:  "draft",
		},
		Spec: doctype.PageManifestSpec{
			Route:        "/demos",
			Runtime:      ">=2.0.0 <3.0.0",
			Offline:      "read_only",
			Layout:       doctype.PageManifestLayout{Type: "table", Columns: 12, Children: []doctype.PageComponent{}},
			Permissions:  []string{},
			Capabilities: []string{},
			Resources:    []doctype.PageResource{},
			Actions:      []doctype.PageAction{},
		},
	}

	if !pageManifestEquivalent(a, b) {
		t.Fatal("expected manifests to be semantically equivalent")
	}

	b.Spec.Layout.Columns = 8
	if pageManifestEquivalent(a, b) {
		t.Fatal("expected manifests with different layout columns to differ")
	}
}

func TestHandleSystemPageManifestUpdate_NoOpDoesNotCreateVersion(t *testing.T) {
	handler, _, mock, sqlDB := setupTestHandler(t)
	gin.SetMode(gin.TestMode)

	requestManifest := doctype.PageManifest{
		APIVersion: "ui.kora.dev/v1",
		Kind:       "Page",
		Metadata: doctype.PageManifestMetadata{
			Name:    "demos-table",
			Version: "0.1.0",
			Package: "tenant.workspace",
			Status:  "draft",
		},
		Spec: doctype.PageManifestSpec{
			Route:   "/demos",
			Runtime: ">=2.0.0 <3.0.0",
			Offline: "read_only",
			Resources: []doctype.PageResource{
				{
					ID:    "primary",
					Query: "document.list",
					Params: map[string]any{
						"doctype": "Demo",
						"limit":   50,
					},
				},
			},
			Layout: doctype.PageManifestLayout{
				Type:     "table",
				Columns:  12,
				Children: []doctype.PageComponent{},
			},
		},
	}

	configBytes, err := json.Marshal(requestManifest)
	if err != nil {
		t.Fatalf("marshal request manifest: %v", err)
	}

	mock.ExpectQuery("SELECT name, route, type, layout, label, module, source_doctype,\\s+public_enabled, public_components, public_allow_mutations, config_json\\s+FROM _kora_view WHERE name = \\? AND \\(site = \\? OR site = ''\\)").
		WithArgs("demos-table", "sync").
		WillReturnRows(ginRowsForPageManifest("demos-table", "/demos", "table", "tenant.workspace", string(configBytes)))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/system/page-manifests/demos-table", bytes.NewBuffer(configBytes))
	c.Params = gin.Params{{Key: "name", Value: "demos-table"}}
	c.Set("site_name", "sync")
	c.Set("site_db", sqlDB)

	handler.HandleSystemPageManifestUpdate(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("response data type = %T, want map[string]any", resp.Data)
	}
	if got := data["version_num"]; got != float64(0) {
		t.Fatalf("version_num = %v, want 0", got)
	}
	if got := data["version_id"]; got != "" {
		t.Fatalf("version_id = %v, want empty string", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func ginRowsForPageManifest(name, route, layout, module, configJSON string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"name", "route", "type", "layout", "label", "module", "source_doctype",
		"public_enabled", "public_components", "public_allow_mutations", "config_json",
	}).AddRow(name, route, "page_manifest", layout, name, module, "", 0, nil, 0, configJSON)
}
