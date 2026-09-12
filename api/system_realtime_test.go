package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/orm"
)

func TestReplayRealtimeReadsSiteScopedOutboxAfterCursor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()

	h := NewHandler(nil, &orm.TxManager{DB: mockDB, Dialect: db.Resolve("mysql")})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/system/realtime?after=evt-1", nil)
	c.Set("site_name", "tenant-a")

	occurredAt := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	mock.ExpectQuery("SELECT id, event_type, site, aggregate_type, aggregate_id, created_at FROM _kora_outbox WHERE site = \\? AND id > \\? ORDER BY id LIMIT 500").
		WithArgs("tenant-a", "evt-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "event_type", "site", "aggregate_type", "aggregate_id", "created_at"}).
			AddRow("evt-2", "kora.customer.after_update", "tenant-a", "Customer", "CUS-1", occurredAt))

	var messages []map[string]any
	err = h.replayRealtime(c, "evt-1", []string{"doctype:customer"}, func(payload []byte) error {
		var message map[string]any
		if err := json.Unmarshal(payload, &message); err != nil {
			return err
		}
		messages = append(messages, message)
		return nil
	})
	if err != nil {
		t.Fatalf("replayRealtime: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("replayed messages = %d, want 1", len(messages))
	}
	if messages[0]["id"] != "evt-2" || messages[0]["doctype"] != "Customer" || messages[0]["operation"] != "update" {
		t.Fatalf("unexpected replay message: %#v", messages[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRealtimeScopesFilterTargets(t *testing.T) {
	if !matchesRealtimeScope([]string{"doctype:customer"}, "doctype:Customer", "Customer") {
		t.Fatal("expected customer scope to match")
	}
	if matchesRealtimeScope([]string{"doctype:invoice"}, "doctype:Customer", "Customer") {
		t.Fatal("unexpected invoice scope match")
	}
	if !realtimePayloadMatchesScopes([]byte(`{"resource":"doctype:Invoice"}`), []string{"doctype:invoice"}) {
		t.Fatal("expected payload scope to match")
	}
}

func TestRealtimeOperationDefaultsSafely(t *testing.T) {
	if got := realtimeOperation("kora.customer.after_insert"); got != "insert" {
		t.Fatalf("operation = %q, want insert", got)
	}
	if got := realtimeOperation("unknown"); got != "update" {
		t.Fatalf("unknown operation = %q, want update", got)
	}
}
