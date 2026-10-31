package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/policy"
)

func setRealtimeTestExecution(c *gin.Context, roles []string, delegated bool) {
	site := c.GetString("site_name")
	if site == "" {
		site = "tenant-a"
	}
	user := c.GetString("user")
	if user == "" {
		user = "reader@example.test"
	}
	actor := contract.ActorContext{PrincipalID: user, PrincipalType: contract.PrincipalHuman, SubjectUserID: user, Site: site, Roles: roles, AuthenticatedAt: time.Now().UTC()}
	var execution policy.Execution
	var err error
	if delegated {
		actor.PrincipalID = "extension:test"
		actor.PrincipalType = contract.PrincipalService
		var grants []policy.Grant
		if value, ok := c.Get("extension_permissions"); ok {
			if permissions, ok := value.([]doctype.Permission); ok {
				for _, permission := range permissions {
					if permission.Read {
						grants = append(grants, policy.Grant{Resource: permission.Doctype, Operation: "read", OwnerOnly: permission.IfOwner})
					}
				}
			}
		}
		execution, err = policy.NewVerifiedExecutionWithGrants(contract.TenantID(site), actor, grants)
	} else {
		execution, err = policy.NewVerifiedExecution(contract.TenantID(site), actor)
	}
	if err != nil {
		panic(err)
	}
	c.Set("policy_execution", execution)
}

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

type rejectingRealtimeAdmission struct{}

func (rejectingRealtimeAdmission) Acquire(context.Context) (func(), error) {
	return nil, errors.New("capacity")
}

func TestHandleSystemRealtimeRejectsWhenProcessCapacityIsFull(t *testing.T) {
	gin.SetMode(gin.TestMode)
	services := NewSiteRuntimeServices()
	services.Replace("tenant-a", SiteRuntimeService{RealtimeAdmission: rejectingRealtimeAdmission{}})
	h := NewHandler(nil, nil)
	h.RuntimeServices = services
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/system/realtime", nil)
	c.Set("site_name", "tenant-a")

	h.HandleSystemRealtime(c)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["error"].(map[string]any)["code"] != "realtime.capacity_exhausted" {
		t.Fatalf("unexpected capacity error response: %#v", response)
	}
}

func TestHandleSystemRealtimeRejectsWhenReplayWorkAdmissionIsFull(t *testing.T) {
	gin.SetMode(gin.TestMode)
	admission, err := dbaccess.NewAdmission(1, 0, time.Second)
	if err != nil {
		t.Fatalf("create work admission: %v", err)
	}
	release, err := admission.Acquire(context.Background())
	if err != nil {
		t.Fatalf("occupy work admission: %v", err)
	}
	defer release()
	services := NewSiteRuntimeServices()
	services.Replace("tenant-a", SiteRuntimeService{WorkAdmission: admission})
	h := NewHandler(nil, nil)
	h.RuntimeServices = services
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/system/realtime?after=evt-1", nil)
	c.Set("site_name", "tenant-a")

	h.HandleSystemRealtime(c)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["error"].(map[string]any)["code"] != "dependency.unavailable" {
		t.Fatalf("unexpected capacity error response: %#v", response)
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
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Customer"})
	registry.Permissions.AddRole(&doctype.Role{Name: "Reader"})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Customer", Role: "Reader", Read: true})
	h.Registry = registry
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/system/realtime?after=evt-1", nil)
	c.Set("site_name", "tenant-a")
	c.Set("user", "reader@example.test")
	c.Set("user_roles", []string{"Reader"})
	setRealtimeTestExecution(c, []string{"Reader"}, false)

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

func TestRealtimeReadAuthorizationAndFieldProjection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Invoice", Fields: []doctype.Field{{Fieldname: "amount", Fieldtype: "Currency"}, {Fieldname: "secret", Fieldtype: "Data", Hidden: true}}})
	reg.Permissions.AddRole(&doctype.Role{Name: "Viewer"})
	reg.Permissions.SetPermission(&doctype.Permission{Doctype: "Invoice", Role: "Viewer", Read: true})
	h := NewHandler(reg, nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Set("user_roles", []string{"Viewer"})
	c.Set("user", "a@example.test")
	setRealtimeTestExecution(c, []string{"Viewer"}, false)
	data := map[string]any{"owner": "a@example.test", "amount": 12, "secret": "private"}
	if !h.realtimeEventVisible(c, "Invoice", "INV-1", data, analytics.EventUpdate) {
		t.Fatal("read role was denied")
	}
	projected := visibleRealtimeFields(reg.Get("Invoice"), data)
	if _, ok := projected["secret"]; ok {
		t.Fatal("hidden field was projected")
	}
	c.Set("user_roles", []string{"Unknown"})
	setRealtimeTestExecution(c, []string{"Unknown"}, false)
	if h.realtimeEventVisible(c, "Invoice", "INV-1", data, analytics.EventUpdate) {
		t.Fatal("event without read permission was allowed")
	}
	c.Set("user_roles", []string{})
	setRealtimeTestExecution(c, nil, false)
	if h.realtimeEventVisible(c, "Invoice", "INV-1", data, analytics.EventUpdate) {
		t.Fatal("event without an authenticated read role was allowed")
	}
	c.Set("user_roles", []string{"Viewer"})
	c.Set("user", "")
	setRealtimeTestExecution(c, []string{"Viewer"}, false)
	reg.Permissions.SetPermission(&doctype.Permission{Doctype: "Invoice", Role: "Viewer", Read: true, IfOwner: true})
	if h.realtimeEventVisible(c, "Invoice", "INV-1", data, analytics.EventUpdate) {
		t.Fatal("owner-scoped event was allowed without an actor identity")
	}
}

func TestRealtimeAndPermissionProjectionRequireVerifiedExecution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Invoice"})
	reg.Permissions.AddRole(&doctype.Role{Name: doctype.AdminRole})
	reg.Permissions.SetPermission(&doctype.Permission{Doctype: "Invoice", Role: doctype.AdminRole, Read: true})
	h := NewHandler(reg, nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Set("user_roles", []string{doctype.AdminRole})
	c.Set("user", "caller@example.test")
	if h.realtimeEventVisible(c, "Invoice", "INV-1", map[string]any{}, analytics.EventUpdate) {
		t.Fatal("unverified caller-supplied roles authorized realtime delivery")
	}
	for operation, allowed := range getUserPermissions(reg, c, "Invoice") {
		if allowed {
			t.Fatalf("unverified context reported %s permission", operation)
		}
	}
}

func TestRealtimeNotificationsUseVerifiedRecipientIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Set("site_name", "tenant-a")
	c.Set("user", "bob@example.test")
	execution, err := policy.NewVerifiedExecution("tenant-stable", contract.ActorContext{
		PrincipalID: "alice@example.test", PrincipalType: contract.PrincipalHuman,
		SubjectUserID: "alice@example.test", Site: "tenant-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Set("policy_execution", execution)
	if h.realtimeEventVisible(c, "", "", map[string]any{"recipient": "bob@example.test"}, analytics.EventNotification) {
		t.Fatal("notification recipient from legacy Gin user field bypassed verified identity")
	}
	if !h.realtimeEventVisible(c, "", "", map[string]any{"recipient": "alice@example.test"}, analytics.EventNotification) {
		t.Fatal("verified actor could not receive its own notification")
	}
}

func TestRealtimeExtensionReadPermissionStillHonorsOwnerRestriction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Invoice"})
	h := NewHandler(reg, nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Set("auth_type", "extension")
	c.Set("extension_permissions", []doctype.Permission{{Doctype: "Invoice", Read: true, IfOwner: true}})
	setRealtimeTestExecution(c, nil, true)
	if h.realtimeEventVisible(c, "Invoice", "INV-1", map[string]any{"owner": "other@example.test"}, analytics.EventUpdate) {
		t.Fatal("extension received an event outside its owner restriction")
	}
	c.Set("user", "owner@example.test")
	if h.realtimeEventVisible(c, "Invoice", "INV-1", map[string]any{"owner": "owner@example.test"}, analytics.EventUpdate) {
		t.Fatal("owner-scoped extension event was trusted without checking the current record")
	}
	c.Set("extension_permissions", []doctype.Permission{{Doctype: "Invoice", Read: true}})
	setRealtimeTestExecution(c, nil, true)
	if !h.realtimeEventVisible(c, "Invoice", "INV-1", map[string]any{"owner": "other@example.test"}, analytics.EventUpdate) {
		t.Fatal("extension without owner restriction was incorrectly narrowed")
	}
}

func TestReplayRealtimeRequiresCurrentReadAuthorization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()

	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Customer"})
	registry.Permissions.AddRole(&doctype.Role{Name: "Reader"})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Customer", Role: "Reader", Read: true})
	h := NewHandler(registry, &orm.TxManager{DB: mockDB, Dialect: db.Resolve("mysql")})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/api/system/realtime?after=evt-1", nil)
	c.Set("site_name", "tenant-a")
	c.Set("user_roles", []string{"Reader"})
	setRealtimeTestExecution(c, []string{"Reader"}, false)
	mock.ExpectQuery("SELECT id, event_type, site, aggregate_type, aggregate_id, created_at FROM _kora_outbox WHERE site = \\? AND id > \\? ORDER BY id LIMIT 501").
		WithArgs("tenant-a", "evt-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "event_type", "site", "aggregate_type", "aggregate_id", "created_at"}).
			AddRow("evt-2", "kora.customer.after_update", "tenant-a", "Customer", "CUS-1", time.Now().UTC()))

	var delivered int
	err = h.replayRealtime(c, "evt-1", nil, func(payload []byte) error {
		var message map[string]any
		if err := json.Unmarshal(payload, &message); err != nil {
			return err
		}
		if message["type"] == "change" {
			delivered++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("authorized replay: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("authorized replay delivered %d changes, want 1", delivered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReplayRealtimeAcceptsExactVerifiedReadGrant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Customer"})
	handler := NewHandler(registry, &orm.TxManager{DB: database, Dialect: db.Resolve("mysql")})
	execution, err := policy.NewVerifiedExecutionWithGrants("tenant-a", contract.ActorContext{
		PrincipalID: "service:replay-reader", PrincipalType: contract.PrincipalService, Site: "tenant-a",
	}, []policy.Grant{{Resource: "Customer", Operation: "read"}})
	if err != nil {
		t.Fatal("create verified replay execution:", err)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/api/system/realtime?after=evt-1", nil)
	c.Set("site_name", "tenant-a")
	c.Set("user", "forged-user")
	c.Set("user_roles", []string{doctype.AdminRole})
	c.Set("policy_execution", execution)
	mock.ExpectQuery("SELECT id, event_type, site, aggregate_type, aggregate_id, created_at FROM _kora_outbox WHERE site = \\? AND id > \\? ORDER BY id LIMIT 501").
		WithArgs("tenant-a", "evt-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "event_type", "site", "aggregate_type", "aggregate_id", "created_at"}).
			AddRow("evt-2", "kora.customer.after_update", "tenant-a", "Customer", "CUS-1", time.Now().UTC()))
	var delivered int
	if err := handler.replayRealtime(c, "evt-1", nil, func(payload []byte) error {
		var message map[string]any
		if err := json.Unmarshal(payload, &message); err != nil {
			return err
		}
		if message["type"] == "change" {
			delivered++
		}
		return nil
	}); err != nil {
		t.Fatal("replay exact-grant change:", err)
	}
	if delivered != 1 {
		t.Fatalf("exact-grant replay delivered %d changes, want one", delivered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestReplayRealtimeDoesNotDeliverAfterReadRoleRevocation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()

	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Customer"})
	registry.Permissions.AddRole(&doctype.Role{Name: "Reader"})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Customer", Role: "Reader", Read: true})
	h := NewHandler(registry, &orm.TxManager{DB: mockDB, Dialect: db.Resolve("mysql")})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/api/system/realtime?after=evt-1", nil)
	c.Set("site_name", "tenant-a")
	c.Set("user", "reader@example.test")
	// The session remains authenticated, but current authorization no longer
	// includes the role that granted read access when the stream was opened.
	c.Set("user_roles", []string{"RevokedReader"})
	setRealtimeTestExecution(c, []string{"RevokedReader"}, false)
	mock.ExpectQuery("SELECT id, event_type, site, aggregate_type, aggregate_id, created_at FROM _kora_outbox WHERE site = \\? AND id > \\? ORDER BY id LIMIT 501").
		WithArgs("tenant-a", "evt-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "event_type", "site", "aggregate_type", "aggregate_id", "created_at"}).
			AddRow("evt-2", "kora.customer.after_update", "tenant-a", "Customer", "CUS-1", time.Now().UTC()))

	var delivered int
	err = h.replayRealtime(c, "evt-1", nil, func(payload []byte) error {
		var message map[string]any
		if err := json.Unmarshal(payload, &message); err != nil {
			return err
		}
		if message["type"] == "change" {
			delivered++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("replay after role revocation: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("replay delivered %d changes after read role revocation, want 0", delivered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorizeRealtimeEnvelopeRemovesOldData(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Invoice", Fields: []doctype.Field{{Fieldname: "amount", Fieldtype: "Currency"}}})
	reg.Permissions.AddRole(&doctype.Role{Name: doctype.AdminRole})
	h := NewHandler(reg, nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Set("user_roles", []string{doctype.AdminRole})
	setRealtimeTestExecution(c, []string{doctype.AdminRole}, false)
	raw := []byte(`{"type":"kora.invoice.after_update","aggregate_type":"Invoice","aggregate_id":"INV-1","data":{"data":{"amount":10,"private":"x"},"old_data":{"amount":1}}}`)
	got, ok := h.authorizeRealtimeEnvelope(c, raw)
	if !ok {
		t.Fatal("authorized event was rejected")
	}
	var envelope map[string]any
	if err := json.Unmarshal(got, &envelope); err != nil {
		t.Fatal(err)
	}
	payload := envelope["data"].(map[string]any)
	if _, ok := payload["old_data"]; ok {
		t.Fatal("old data was forwarded")
	}
	fields := payload["data"].(map[string]any)
	if len(fields) != 1 || fields["amount"] != float64(10) {
		t.Fatalf("unexpected visible fields: %#v", fields)
	}
}

func TestAuthorizeLegacyNATSRealtimeEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Invoice", Fields: []doctype.Field{{Fieldname: "amount", Fieldtype: "Currency"}}})
	reg.Permissions.AddRole(&doctype.Role{Name: doctype.AdminRole})
	h := NewHandler(reg, nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Set("user_roles", []string{doctype.AdminRole})
	setRealtimeTestExecution(c, []string{doctype.AdminRole}, false)
	raw := []byte(`{"type":"change","doctype":"Invoice","doc_name":"INV-1","operation":"update","payload":{"amount":10,"old_data":{"amount":1}}}`)
	got, ok := h.authorizeRealtimeEnvelope(c, raw)
	if !ok {
		t.Fatal("legacy NATS change event was rejected")
	}
	var envelope map[string]any
	if err := json.Unmarshal(got, &envelope); err != nil {
		t.Fatal(err)
	}
	payload := envelope["payload"].(map[string]any)
	if _, ok := payload["old_data"]; ok {
		t.Fatal("legacy old data was forwarded")
	}
	if len(payload) != 1 || payload["amount"] != float64(10) {
		t.Fatalf("unexpected legacy projection: %#v", payload)
	}
}

func TestRealtimeOwnerScopeMatchesLocalAndNATSAuthorization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Invoice", Fields: []doctype.Field{{Fieldname: "amount", Fieldtype: "Currency"}}})
	reg.Permissions.AddRole(&doctype.Role{Name: "OwnerReader"})
	reg.Permissions.SetPermission(&doctype.Permission{Doctype: "Invoice", Role: "OwnerReader", Read: true, IfOwner: true})
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()
	h := NewHandler(reg, &orm.TxManager{DB: mockDB, Dialect: db.Resolve("mysql")})
	query := "SELECT amount, name, owner, creation, modified, modified_by, doc_status, revision FROM .* WHERE name = \\? AND owner = \\?"

	for _, tc := range []struct {
		user      string
		wantAllow bool
	}{
		{user: "alice@example.test", wantAllow: false},
		{user: "bob@example.test", wantAllow: true},
	} {
		t.Run(tc.user, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			c.Set("user", tc.user)
			c.Set("user_roles", []string{"OwnerReader"})
			setRealtimeTestExecution(c, []string{"OwnerReader"}, false)
			for range 2 {
				expectation := mock.ExpectQuery(query).WithArgs("INV-1", tc.user)
				if tc.user == "bob@example.test" {
					expectation.WillReturnRows(sqlmock.NewRows([]string{"amount", "name", "owner", "creation", "modified", "modified_by", "doc_status", "revision"}).AddRow("125", "INV-1", "bob@example.test", nil, nil, nil, 0, 1))
				} else {
					expectation.WillReturnRows(sqlmock.NewRows([]string{"amount", "name", "owner", "creation", "modified", "modified_by", "doc_status", "revision"}))
				}
			}
			// The event snapshot says Alice owned this document; the current row
			// belongs to Bob, so only Bob may receive the delayed event.
			data := map[string]any{"owner": "alice@example.test", "amount": 125}
			localAllowed := h.realtimeEventVisible(c, "Invoice", "INV-1", data, analytics.EventUpdate)
			raw := []byte(`{"type":"change","doctype":"Invoice","doc_name":"INV-1","operation":"update","payload":{"owner":"alice@example.test","amount":125}}`)
			_, natsAllowed := h.authorizeRealtimeEnvelope(c, raw)
			if localAllowed != tc.wantAllow || natsAllowed != tc.wantAllow {
				t.Fatalf("authorization mismatch: local=%v nats=%v want=%v", localAllowed, natsAllowed, tc.wantAllow)
			}
		})
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRealtimeAssignmentScopeUsesCurrentRecordForLocalAndReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}, {Fieldname: "assigned_to", Fieldtype: "User"}}})
	registry.Permissions.AddRole(&doctype.Role{Name: "Assignee"})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Task", Role: "Assignee", Read: true, IfAssigned: []string{"assigned_to"}})
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	handler := NewHandler(registry, &orm.TxManager{DB: database, Dialect: db.Resolve("mysql")})
	query := "SELECT title, assigned_to, name, owner, creation, modified, modified_by, doc_status, revision FROM .* WHERE name = \\? AND \\(`assigned_to` = \\?\\)"
	for _, tc := range []struct {
		user  string
		allow bool
	}{{"alice", true}, {"bob", false}} {
		for range 2 {
			expectation := mock.ExpectQuery(query).WithArgs("TASK-1", tc.user)
			rows := sqlmock.NewRows([]string{"title", "assigned_to", "name", "owner", "creation", "modified", "modified_by", "doc_status", "revision"})
			if tc.allow {
				rows.AddRow("Call customer", "alice", "TASK-1", "creator", nil, nil, nil, 0, 4)
			}
			expectation.WillReturnRows(rows)
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.Set("site_name", "tenant-a")
		c.Set("user", tc.user)
		setRealtimeTestExecution(c, []string{"Assignee"}, false)
		local := handler.realtimeEventVisible(c, "Task", "TASK-1", map[string]any{"assigned_to": "old-user"}, analytics.EventUpdate)
		raw := []byte(`{"type":"change","doctype":"Task","doc_name":"TASK-1","operation":"update","payload":{"assigned_to":"old-user","title":"old value"}}`)
		_, replay := handler.authorizeRealtimeEnvelope(c, raw)
		if local != tc.allow || replay != tc.allow {
			t.Errorf("user %s assignment scope local=%t replay=%t, want %t", tc.user, local, replay, tc.allow)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRealtimeExactOwnerGrantMatchesLocalAndNATSAuthorization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Invoice", Fields: []doctype.Field{{Fieldname: "amount", Fieldtype: "Currency"}}})
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	handler := NewHandler(reg, &orm.TxManager{DB: database, Dialect: db.Resolve("mysql")})
	execution, err := policy.NewVerifiedExecutionWithGrants("tenant-a", contract.ActorContext{
		PrincipalID: "extension:invoice-reader", PrincipalType: contract.PrincipalService, Site: "tenant-route",
	}, []policy.Grant{{Resource: "Invoice", Operation: "read", OwnerOnly: true}})
	if err != nil {
		t.Fatal("create verified owner-scoped execution:", err)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Set("site_name", "tenant-route")
	c.Set("user", "forged-user@example.test")
	c.Set("user_roles", []string{doctype.AdminRole})
	c.Set("policy_execution", execution)
	query := "SELECT amount, name, owner, creation, modified, modified_by, doc_status, revision FROM .* WHERE name = \\? AND owner = \\?"
	for range 2 {
		mock.ExpectQuery(query).WithArgs("INV-1", "extension:invoice-reader").WillReturnRows(
			sqlmock.NewRows([]string{"amount", "name", "owner", "creation", "modified", "modified_by", "doc_status", "revision"}).AddRow("125", "INV-1", "extension:invoice-reader", nil, nil, nil, 0, 1))
	}
	// Historical event ownership and caller-populated identity must not change
	// current-record owner filtering on either local or NATS delivery.
	data := map[string]any{"owner": "forged-user@example.test", "amount": 125}
	localAllowed := handler.realtimeEventVisible(c, "Invoice", "INV-1", data, analytics.EventUpdate)
	raw := []byte(`{"type":"change","doctype":"Invoice","doc_name":"INV-1","operation":"update","payload":{"owner":"forged-user@example.test","amount":125}}`)
	_, natsAllowed := handler.authorizeRealtimeEnvelope(c, raw)
	if !localAllowed || !natsAllowed {
		t.Fatalf("exact owner grant mismatch: local=%v nats=%v, want both allowed for current verified principal", localAllowed, natsAllowed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
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
