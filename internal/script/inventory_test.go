package script

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/db"
)

func TestInspectLifecycleInventoryReturnsOnlyBoundedAggregate(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	query := "SELECT script_type, event, is_active FROM _kora_script WHERE site = \\? LIMIT \\?"
	mock.ExpectQuery(query).WithArgs("tenant-a", maxLifecycleInventoryRows+1).
		WillReturnRows(sqlmock.NewRows([]string{"script_type", "event", "is_active"}).
			AddRow("doc_event", "after_save", true).
			AddRow("doc_event", "after_save", true).
			AddRow("doc_event", "before_submit", true).
			AddRow("doc_event", "before_save", false).
			AddRow("doc_event", "tenant_defined_event", true).
			AddRow("doc_event", "unknown_inactive_event", false).
			AddRow("custom", "", true))

	got, err := InspectLifecycleInventory(context.Background(), database, db.MySQL(), "tenant-a")
	if err != nil {
		t.Fatal("inspect lifecycle inventory:", err)
	}
	if !got.Complete || got.RowsInspected != 7 || got.DocumentEventScripts != 6 ||
		got.ActiveDocumentEventScripts != 4 || got.InactiveDocumentEventScripts != 2 ||
		got.ActiveAfterEventScripts != 2 || got.ActiveAfterEventByEvent[string(EventAfterSave)] != 2 ||
		got.UnknownDocumentEventScripts != 2 || got.OtherScriptTypes != 1 {
		t.Fatalf("unexpected lifecycle inventory: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestInspectLifecycleInventoryUsesDialectRebinding(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	mock.ExpectQuery(`SELECT script_type, event, is_active FROM _kora_script WHERE site = \$1 LIMIT \$2`).
		WithArgs("tenant-a", maxLifecycleInventoryRows+1).
		WillReturnRows(sqlmock.NewRows([]string{"script_type", "event", "is_active"}))
	got, err := InspectLifecycleInventory(context.Background(), database, db.Resolve("postgres"), "tenant-a")
	if err != nil || !got.Complete || got.RowsInspected != 0 {
		t.Fatalf("postgres inventory = %+v, %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestInspectLifecycleInventoryFailsClosedAtRowLimit(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	rows := sqlmock.NewRows([]string{"script_type", "event", "is_active"})
	for range maxLifecycleInventoryRows + 1 {
		rows.AddRow("doc_event", "after_save", true)
	}
	mock.ExpectQuery(`SELECT script_type, event, is_active FROM _kora_script`).
		WithArgs("tenant-a", maxLifecycleInventoryRows+1).WillReturnRows(rows)
	got, err := InspectLifecycleInventory(context.Background(), database, db.MySQL(), "tenant-a")
	var inventoryErr *LifecycleInventoryError
	if !errors.As(err, &inventoryErr) || inventoryErr.Code != "row_limit_exceeded" || got.Complete || got.RowsInspected != maxLifecycleInventoryRows {
		t.Fatalf("bounded inventory = %+v, %v", got, err)
	}
	if strings.Contains(err.Error(), "after_save") || strings.Contains(err.Error(), "tenant-a") {
		t.Fatalf("inventory error leaked stored data: %v", err)
	}
}

func TestInspectLifecycleInventoryDoesNotExposeDatabaseError(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	mock.ExpectQuery(`SELECT script_type, event, is_active FROM _kora_script`).
		WithArgs("tenant-a", maxLifecycleInventoryRows+1).
		WillReturnError(errors.New("dsn password leaked by driver"))
	got, err := InspectLifecycleInventory(context.Background(), database, db.MySQL(), "tenant-a")
	var inventoryErr *LifecycleInventoryError
	if !errors.As(err, &inventoryErr) || inventoryErr.Code != "query_failed" || got.Complete {
		t.Fatalf("failed inventory = %+v, %v", got, err)
	}
	if strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "tenant-a") {
		t.Fatalf("inventory error leaked details: %v", err)
	}
}

func TestInspectLifecycleInventoryRequiresTenantScope(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	got, err := InspectLifecycleInventory(context.Background(), database, db.MySQL(), "")
	if err == nil || got.Complete {
		t.Fatalf("unscoped inventory = %+v, %v; want fail-closed error", got, err)
	}
}
