package contract

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestErrorCodesAreStable freezes the machine-readable error codes. Any rename
// here is a breaking contract change and must be accompanied by a version bump.
func TestErrorCodesAreStable(t *testing.T) {
	tests := []struct {
		code Code
		want string
	}{
		{CodePermissionDenied, "PERMISSION_DENIED"},
		{CodeValidationFailed, "VALIDATION_FAILED"},
		{CodeNotFound, "NOT_FOUND"},
		{CodeConflict, "CONFLICT"},
		{CodeDeadlineExceeded, "DEADLINE_EXCEEDED"},
		{CodeDependencyUnavailable, "DEPENDENCY_UNAVAILABLE"},
		{CodeIdempotencyKeyReused, "IDEMPOTENCY_KEY_REUSED"},
		{CodeUnauthenticated, "UNAUTHENTICATED"},
		{CodeInternal, "INTERNAL_ERROR"},
	}
	for _, tt := range tests {
		if string(tt.code) != tt.want {
			t.Errorf("code %v = %q, want %q", tt.code, string(tt.code), tt.want)
		}
	}
}

// TestStatusesAreStable freezes the operation result states.
func TestStatusesAreStable(t *testing.T) {
	tests := []struct {
		s    Status
		want string
	}{
		{StatusCompleted, "completed"},
		{StatusAccepted, "accepted"},
		{StatusRejected, "rejected"},
		{StatusConflict, "conflict"},
		{StatusFailed, "failed"},
		{StatusPending, "pending"},
	}
	for _, tt := range tests {
		if string(tt.s) != tt.want {
			t.Errorf("status %v = %q, want %q", tt.s, string(tt.s), tt.want)
		}
	}
}

// TestCommandEnvelopeJSONRoundTrip verifies the frozen command envelope wire shape.
func TestCommandEnvelopeJSONRoundTrip(t *testing.T) {
	deadline := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	in := CommandEnvelope{
		Type:           "document.submit",
		Version:        1,
		ID:             "id-1",
		Site:           "acme.example.com",
		Actor:          ActorContext{PrincipalID: "usr-1", PrincipalType: PrincipalHuman, Site: "acme.example.com", Roles: []string{"Accounts User"}, AuthenticatedAt: deadline},
		CorrelationID:  "req-123",
		IdempotencyKey: "client-op-456",
		Deadline:       deadline,
		Data:           json.RawMessage(`{"doctype":"Sales Invoice","name":"SINV-0001"}`),
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Freeze the required field names.
	for _, key := range []string{"type", "version", "id", "site", "actor", "correlation_id", "idempotency_key", "deadline", "data"} {
		if !strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("marshaled command missing key %q: %s", key, b)
		}
	}

	var out CommandEnvelope
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Type != in.Type || out.Version != in.Version || out.ID != in.ID || out.Site != in.Site {
		t.Errorf("round-trip mismatch: %+v", out)
	}
	if out.Actor.PrincipalID != "usr-1" || out.Actor.PrincipalType != PrincipalHuman {
		t.Errorf("actor round-trip mismatch: %+v", out.Actor)
	}
	if string(out.Data) != string(in.Data) {
		t.Errorf("data round-trip mismatch: %s vs %s", out.Data, in.Data)
	}
}

// TestEventEnvelopeJSONRoundTrip verifies the frozen event envelope wire shape.
func TestEventEnvelopeJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	in := EventEnvelope{
		ID:            NewEventID(),
		Type:          "kora.document.sales_invoice.submitted",
		Version:       1,
		Source:        "kora.kernel",
		Site:          "acme.example.com",
		AggregateType: "Sales Invoice",
		AggregateID:   "SINV-0001",
		OccurredAt:    now,
		CorrelationID: "req-123",
		Data:          json.RawMessage(`{"projection":"changed_fields","changed_fields":["status"]}`),
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, key := range []string{"id", "type", "version", "source", "site", "aggregate_type", "aggregate_id", "occurred_at", "data"} {
		if !strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("marshaled event missing key %q: %s", key, b)
		}
	}

	var out EventEnvelope
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.AggregateType != "Sales Invoice" || out.AggregateID != "SINV-0001" {
		t.Errorf("aggregate round-trip mismatch: %+v", out)
	}
}

func TestEventEnvelopeValidateRejectsMalformedEvents(t *testing.T) {
	valid := EventEnvelope{ID: "evt-1", Type: "stock.changed", Version: 1, Source: "engine", Site: "acme", OccurredAt: time.Now(), Data: json.RawMessage(`{"quantity":1}`)}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}
	for _, event := range []EventEnvelope{valid, {Type: valid.Type, Version: 1, Source: valid.Source, Site: valid.Site, OccurredAt: valid.OccurredAt, Data: valid.Data}, {ID: "x", Type: valid.Type, Version: 1, Source: valid.Source, Site: valid.Site, OccurredAt: valid.OccurredAt, Data: json.RawMessage(`{`)}} {
		if event.ID == "evt-1" {
			event.Data = nil
		}
		if err := event.Validate(); err == nil {
			t.Fatalf("malformed event accepted: %+v", event)
		}
	}
}

// TestCommandDeadlineSemantics verifies bounded-request behavior (RFC §7).
func TestCommandDeadlineSemantics(t *testing.T) {
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		deadline time.Time
		expired  bool
	}{
		{"zero deadline is not expired", time.Time{}, false},
		{"future deadline is not expired", now.Add(time.Second), false},
		{"past deadline is expired", now.Add(-time.Second), true},
		{"equal deadline is expired", now, true},
	}

	for _, tt := range tests {
		c := CommandEnvelope{Deadline: tt.deadline}
		if got := c.Expired(now); got != tt.expired {
			t.Errorf("%s: Expired() = %v, want %v", tt.name, got, tt.expired)
		}
	}
}

// TestActorAuthenticated verifies fail-closed identity checks (RFC §7.3).
func TestActorAuthenticated(t *testing.T) {
	tests := []struct {
		name  string
		actor ActorContext
		want  bool
	}{
		{"missing identity", ActorContext{}, false},
		{"missing type", ActorContext{PrincipalID: "usr-1"}, false},
		{"missing id", ActorContext{PrincipalType: PrincipalHuman}, false},
		{"valid", ActorContext{PrincipalID: "usr-1", PrincipalType: PrincipalHuman}, true},
	}
	for _, tt := range tests {
		if got := tt.actor.Authenticated(); got != tt.want {
			t.Errorf("%s: Authenticated() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestNormalizeCode verifies the unstable-error → stable-code mapping.
func TestNormalizeCode(t *testing.T) {
	tests := []struct {
		in   string
		want Code
	}{
		{"PERMISSION_DENIED", CodePermissionDenied},
		{"VALIDATION_FAILED", CodeValidationFailed},
		{"NOT_FOUND", CodeNotFound},
		{"CONFLICT", CodeConflict},
		{"something unknown", CodeInternal},
		{"", CodeInternal},
	}
	for _, tt := range tests {
		if got := NormalizeCode(tt.in); got != tt.want {
			t.Errorf("NormalizeCode(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// TestNewErrorImplementsError verifies the structured error satisifies error.
func TestNewErrorImplementsError(t *testing.T) {
	var e error = NewError(CodeNotFound, "no such document")
	if e.Error() != "no such document" {
		t.Errorf("Error() = %q, want %q", e.Error(), "no such document")
	}
	if NewError(CodeNotFound, "x").Type != CodeNotFound {
		t.Errorf("Type mismatch")
	}
}

// TestNewIDIsULID verifies the ID helper returns a 26-char ULID.
func TestNewIDIsULID(t *testing.T) {
	id := NewID()
	if len(id) != 26 {
		t.Errorf("NewID() length = %d, want 26", len(id))
	}
	if a, b := NewID(), NewID(); a == b {
		t.Errorf("NewID() returned duplicate IDs")
	}
}

// TestContractInventoryCoversCurrentSurface verifies the package exposes a
// stable inventory for documentation and handoff use.
func TestContractInventoryCoversCurrentSurface(t *testing.T) {
	inv := ContractInventory()
	if len(inv) < 10 {
		t.Fatalf("inventory too small: %d", len(inv))
	}

	want := map[string]SurfaceStatus{
		"status":              SurfaceSupported,
		"error_codes":         SurfaceSupported,
		"actor_context":       SurfaceExperimental,
		"command_envelope":    SurfaceSupported,
		"event_envelope":      SurfaceSupported,
		"resource_descriptor": SurfacePartial,
		"ui_manifest":         SurfaceSupported,
		"offline_sync":        SurfaceSupported,
		"authentication":      SurfaceSupported,
	}

	got := map[string]SurfaceStatus{}
	for _, s := range inv {
		got[s.Name] = s.Status
	}
	for name, status := range want {
		if got[name] != status {
			t.Fatalf("surface %q status = %q, want %q", name, got[name], status)
		}
	}
}
