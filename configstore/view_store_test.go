package configstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	koraDB "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

func newViewStoreForTest(t *testing.T) (*Store, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := &Store{DB: db, Dialect: &koraDB.MySQLDialect{}}
	return s, mock
}

func TestViewStore_SaveAndLoad(t *testing.T) {
	s, mock := newViewStoreForTest(t)

	// Build a view with components.
	v := &doctype.View{
		Name:   "Test View",
		Route:  "/test",
		Type:   "workspace",
		Layout: "two_panel",
		Label:  "Test Workspace",
		Module: "Testing",
		Components: []doctype.ViewComponent{
			{
				ID:     "list",
				Type:   "record_table",
				Region: "main",
				Bindings: map[string]string{
					"title": "subject",
				},
				MobileColumns: []string{"subject", "status"},
			},
		},
	}

	configJSON, _ := json.Marshal(v.Components)

	// Expect a transaction: Begin, INSERT/UPSERT, Commit.
	mock.ExpectBegin()

	// The INSERT uses dialect-aware placeholders and upsert.
	mock.ExpectExec("INSERT INTO _kora_view").
		WithArgs(
			"Test View", "test-site", "/test", "workspace", "two_panel",
			"Test Workspace", "Testing", "", 0, "", 0, string(configJSON), 0,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := s.SaveView(v, "test-site"); err != nil {
		t.Fatalf("SaveView: %v", err)
	}

	// Test LoadView separately.
	mock.ExpectQuery("SELECT .* FROM _kora_view WHERE name = \\? AND \\(site = \\? OR site = ''\\)").
		WithArgs("Test View", "test-site").
		WillReturnRows(sqlmock.NewRows([]string{
			"name", "route", "type", "layout", "label", "module", "source_doctype",
			"public_enabled", "public_components", "public_allow_mutations", "config_json",
		}).AddRow(
			"Test View", "/test", "workspace", "two_panel", "Test Workspace", "Testing", "",
			0, "", 0, string(configJSON),
		))

	loaded, err := s.LoadView("Test View", "test-site")
	if err != nil {
		t.Fatalf("LoadView: %v", err)
	}
	if loaded.Name != v.Name {
		t.Errorf("name: got %q, want %q", loaded.Name, v.Name)
	}
	if loaded.Route != v.Route {
		t.Errorf("route: got %q, want %q", loaded.Route, v.Route)
	}
	if len(loaded.Components) != 1 {
		t.Fatalf("expected 1 component, got %d", len(loaded.Components))
	}
	if loaded.Components[0].Bindings["title"] != "subject" {
		t.Errorf("binding: got %q, want 'subject'", loaded.Components[0].Bindings["title"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestViewStore_LoadMultiple(t *testing.T) {
	s, mock := newViewStoreForTest(t)

	mock.ExpectQuery("SELECT .* FROM _kora_view WHERE site = \\? OR site = '' ORDER BY idx").
		WithArgs("test-site").
		WillReturnRows(sqlmock.NewRows([]string{
			"name", "route", "type", "layout", "label", "module", "source_doctype",
			"public_enabled", "public_components", "public_allow_mutations", "config_json",
		}).
			AddRow("View One", "/one", "workspace", "single", "", "Workspace", "", 0, "", 0, "[]").
			AddRow("View Two", "/two", "dashboard", "grid", "", "Workspace", "", 0, "", 0, "[]"),
		)

	loaded, err := s.LoadViews("test-site")
	if err != nil {
		t.Fatalf("LoadViews: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 views, got %d", len(loaded))
	}
	if loaded[0].Name != "View One" || loaded[1].Name != "View Two" {
		t.Errorf("unexpected view order: %v", loaded)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestViewStore_Delete(t *testing.T) {
	s, mock := newViewStoreForTest(t)

	mock.ExpectExec("DELETE FROM _kora_view WHERE name = \\? AND site = \\?").
		WithArgs("Deletable", "test-site").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.DeleteView("Deletable", "test-site"); err != nil {
		t.Fatalf("DeleteView: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestViewStore_PublicAccessRoundTrip(t *testing.T) {
	s, mock := newViewStoreForTest(t)

	v := &doctype.View{
		Name:  "Public Catalog",
		Route: "/catalog",
		Type:  "collection",
		PublicAccess: &doctype.ViewPublicAccess{
			Enabled:        true,
			Components:     []string{"products", "search"},
			AllowMutations: true,
		},
	}

	configJSON, _ := json.Marshal(v.Components)
	publicComponentsJSON, _ := json.Marshal(v.PublicAccess.Components)

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO _kora_view").
		WithArgs(
			"Public Catalog", "test-site", "/catalog", "collection", "single", "", "Workspace",
			"", 1, string(publicComponentsJSON), 1, string(configJSON), 0,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := s.SaveView(v, "test-site"); err != nil {
		t.Fatalf("SaveView: %v", err)
	}

	// Load back.
	mock.ExpectQuery("SELECT .* FROM _kora_view WHERE name = \\? AND \\(site = \\? OR site = ''\\)").
		WithArgs("Public Catalog", "test-site").
		WillReturnRows(sqlmock.NewRows([]string{
			"name", "route", "type", "layout", "label", "module", "source_doctype",
			"public_enabled", "public_components", "public_allow_mutations", "config_json",
		}).AddRow(
			"Public Catalog", "/catalog", "collection", "single", "", "Workspace", "",
			1, string(publicComponentsJSON), 1, string(configJSON),
		))

	loaded, err := s.LoadView("Public Catalog", "test-site")
	if err != nil {
		t.Fatalf("LoadView: %v", err)
	}
	if loaded.PublicAccess == nil || !loaded.PublicAccess.Enabled {
		t.Fatal("expected public access enabled")
	}
	if !loaded.PublicAccess.AllowsComponent("products") {
		t.Error("expected products allowed")
	}
	if loaded.PublicAccess.AllowsComponent("cart") {
		t.Error("expected cart NOT allowed")
	}
	if !loaded.PublicAccess.AllowMutations {
		t.Error("expected mutations allowed")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestViewStore_SaveViewDoesNotReplaceExistingViews(t *testing.T) {
	s, mock := newViewStoreForTest(t)

	first := &doctype.View{Name: "View A", Route: "/a", Type: "workspace"}
	second := &doctype.View{Name: "View B", Route: "/b", Type: "dashboard"}

	configA, _ := json.Marshal(first.Components)
	configB, _ := json.Marshal(second.Components)

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO _kora_view").
		WithArgs("View A", "test-site", "/a", "workspace", "single", "", "Workspace", "", 0, "", 0, string(configA), 0).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO _kora_view").
		WithArgs("View B", "test-site", "/b", "dashboard", "single", "", "Workspace", "", 0, "", 0, string(configB), 0).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := s.SaveView(first, "test-site"); err != nil {
		t.Fatalf("SaveView first: %v", err)
	}
	if err := s.SaveView(second, "test-site"); err != nil {
		t.Fatalf("SaveView second: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestViewStore_SaveTx_ReplacesAll(t *testing.T) {
	s, mock := newViewStoreForTest(t)

	views := []*doctype.View{
		{Name: "View A", Route: "/a", Type: "workspace"},
		{Name: "View B", Route: "/b", Type: "dashboard"},
	}

	configA, _ := json.Marshal(views[0].Components)
	configB, _ := json.Marshal(views[1].Components)

	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM _kora_view WHERE site = ?").
		WithArgs("test-site").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("INSERT INTO _kora_view").
		WithArgs("View A", "test-site", "/a", "workspace", "single", "", "Workspace", "", 0, "", 0, string(configA), 0).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO _kora_view").
		WithArgs("View B", "test-site", "/b", "dashboard", "single", "", "Workspace", "", 0, "", 0, string(configB), 1).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	tx, err := s.DB.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := s.SaveViewsTx(tx, views, "test-site"); err != nil {
		t.Fatalf("SaveViewsTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestViewStore_NotFound(t *testing.T) {
	s, mock := newViewStoreForTest(t)

	mock.ExpectQuery("SELECT .* FROM _kora_view WHERE name = \\? AND \\(site = \\? OR site = ''\\)").
		WithArgs("Missing", "test-site").
		WillReturnError(sql.ErrNoRows)

	_, err := s.LoadView("Missing", "test-site")
	if err == nil {
		t.Error("expected error for missing view, got nil")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("LoadView error = %v, want wrapped sql.ErrNoRows", err)
	}
	if !strings.Contains(err.Error(), `view "Missing" not found`) {
		t.Errorf("unexpected error: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
