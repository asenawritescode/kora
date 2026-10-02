package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
)

type deadlineResponseWriter struct {
	http.ResponseWriter
	deadline time.Time
}

func (w *deadlineResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func TestRefreshRealtimeWriteDeadline(t *testing.T) {
	w := &deadlineResponseWriter{ResponseWriter: httptest.NewRecorder()}
	if err := refreshRealtimeWriteDeadline(w); err != nil {
		t.Fatalf("refreshRealtimeWriteDeadline: %v", err)
	}
	if !w.deadline.After(time.Now()) {
		t.Fatalf("write deadline was not advanced: %v", w.deadline)
	}
}

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
	mock.ExpectQuery("SELECT id, event_type, site, aggregate_type, aggregate_id, created_at FROM _kora_outbox WHERE site = \\? AND id > \\? ORDER BY id LIMIT 501").
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
	if len(messages) != 2 {
		t.Fatalf("replayed messages = %d, want event plus completion marker", len(messages))
	}
	if messages[0]["id"] != "evt-2" || messages[0]["doctype"] != "Customer" || messages[0]["operation"] != "update" {
		t.Fatalf("unexpected replay message: %#v", messages[0])
	}
	if messages[1]["type"] != "replay_complete" || messages[1]["cursor"] != "evt-2" || messages[1]["truncated"] != false {
		t.Fatalf("unexpected replay completion: %#v", messages[1])
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

func TestRealtimeWorkflowNotificationsAreRecipientScoped(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"operation": string(analytics.EventNotification),
		"payload":   map[string]any{"recipient": "owner@example.test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !realtimePayloadVisibleToUser(payload, "OWNER@example.test") {
		t.Fatal("notification should be visible to its recipient")
	}
	if realtimePayloadVisibleToUser(payload, "other@example.test") {
		t.Fatal("notification was visible to a different user")
	}
	if realtimePayloadVisibleToUser(payload, "") {
		t.Fatal("notification was visible without an authenticated user")
	}
	if !realtimePayloadVisibleToUser([]byte(`{"operation":"update"}`), "other@example.test") {
		t.Fatal("ordinary record updates should not be filtered as notifications")
	}
}

func TestDispatchNotificationsPublishesConfiguredWorkflowMessage(t *testing.T) {
	registry := doctype.NewRegistry()
	registry.Workflows.Register(&doctype.Workflow{
		DocumentType: "Sale",
		Notifications: []doctype.WorkflowNotification{{
			Event: "state_change", ToState: "Paid",
			Recipients: []map[string]string{{"field": "customer_email"}},
			Subject:    "Sale paid", Message: "Your payment was recorded.",
		}},
	})

	bus := analytics.NewChannelBus(8, t.TempDir())
	defer bus.Close()
	events, err := bus.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	doc := doctype.NewDocument("Sale")
	doc.Name = "SALE-1"
	doc.Set("customer_email", "buyer@example.test")
	dispatchNotifications(registry, "Sale", "Paid", doc, "demo", "cashier@example.test", bus)

	select {
	case event := <-events:
		if event.Operation != analytics.EventNotification || event.Site != "demo" || event.DocName != "SALE-1" {
			t.Fatalf("unexpected workflow notification event: %#v", event)
		}
		if event.Data["recipient"] != "buyer@example.test" || event.Data["title"] != "Sale paid" || event.Data["message"] != "Your payment was recorded." {
			t.Fatalf("notification payload missing configured content: %#v", event.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("workflow notification was not published")
	}
}
