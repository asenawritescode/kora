package api

import (
	"encoding/json"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/eventstream"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

// realtimeEventVisible applies the same DocType read and owner rules as resource
// reads. Events without a document identity are denied except notifications.
func (h *Handler) realtimeEventVisible(c *gin.Context, doctypeName, docName string, data map[string]any, op eventstream.EventOp, eventScopes ...contract.BusinessScope) bool {
	execution, ok := requestExecution(c)
	if !ok {
		return false
	}
	user, hasUser := executionUserID(execution)
	if !hasUser {
		return false
	}
	if op == eventstream.EventNotification {
		return realtimeNotificationMatchesRecipient(data, user)
	}
	reg := h.siteRegistry(c)
	if reg == nil || reg.Get(doctypeName) == nil || docName == "" {
		return false
	}
	businessScope := businessScopeFromRequest(c, execution)
	if len(eventScopes) > 0 {
		if eventScopes[0].Validate() != nil && reg.Get(doctypeName).BusinessScopePolicy == contract.ScopeEntityOwned {
			return false
		}
	}
	if len(eventScopes) > 0 && eventScopes[0].Validate() == nil {
		businessScope = eventScopes[0]
		requestedScope := businessScopeFromRequest(c, execution)
		if reg.Get(doctypeName).BusinessScopePolicy == contract.ScopeEntityOwned && requestedScope.Kind == contract.ScopeKindLegalEntity && requestedScope != businessScope {
			return false
		}
	}
	decision, err := policy.DecideForBusinessScope(execution, reg, doctypeName, "read", businessScope, reg.Get(doctypeName).BusinessScopePolicy == contract.ScopeEntityOwned)
	if err != nil {
		return false
	}
	if decision.OwnerOnly || len(decision.AssignedFields) > 0 {
		user := strings.TrimSpace(user)
		tx := h.siteTx(c)
		if user == "" || tx == nil || tx.DB == nil {
			return false
		}
		// Event data can describe an older owner (especially on delayed NATS
		// delivery). Check the live record using the same owner-filtered read
		// query as resource access instead of trusting the event snapshot.
		scope := policy.RowScope{UserID: user, Owner: decision.OwnerOnly, AssignedFields: decision.AssignedFields, Business: businessScope}
		if _, err := tx.GetDocWithScope(reg.Get(doctypeName), docName, scope); err != nil {
			return false
		}
	}
	return true
}

func visibleRealtimeFields(dt *doctype.DocType, data map[string]any) map[string]any {
	if data == nil {
		return nil
	}
	visible := map[string]any{}
	for _, key := range []string{"name", "doc_status"} {
		if v, ok := data[key]; ok {
			visible[key] = v
		}
	}
	for _, field := range dt.DataFields() {
		if field.Hidden || field.Fieldtype == "Password" {
			continue
		}
		if v, ok := data[field.Fieldname]; ok {
			visible[field.Fieldname] = v
		}
	}
	return visible
}

// authorizeRealtimeEnvelope sanitizes event envelope payloads used by NATS.
// Old values are never forwarded; consumers receive only visible current fields.
func (h *Handler) authorizeRealtimeEnvelope(c *gin.Context, raw []byte) ([]byte, bool) {
	var envelope map[string]any
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, false
	}
	doctypeName, _ := envelope["aggregate_type"].(string)
	if doctypeName == "" {
		doctypeName, _ = envelope["doctype"].(string)
	}
	docName, _ := envelope["aggregate_id"].(string)
	if docName == "" {
		docName, _ = envelope["doc_name"].(string)
	}
	operation := eventstream.EventUpdate
	if eventName, ok := envelope["operation"].(string); ok {
		operation = eventstream.EventOp(eventName)
	} else if eventType, _ := envelope["type"].(string); eventType != "" {
		operation = eventstream.EventOp(realtimeOperation(eventType))
	}
	payload, hasData := envelope["data"].(map[string]any)
	if !hasData {
		payload, _ = envelope["payload"].(map[string]any)
	}
	data, nested := payload["data"].(map[string]any)
	if !nested {
		data = payload
	}
	if payload == nil {
		payload = map[string]any{}
	}
	var eventScope contract.BusinessScope
	if encodedScope, exists := envelope["business_scope"]; exists {
		encoded, _ := json.Marshal(encodedScope)
		_ = json.Unmarshal(encoded, &eventScope)
	}
	if !h.realtimeEventVisible(c, doctypeName, docName, data, operation, eventScope) {
		return nil, false
	}
	if operation == eventstream.EventNotification {
		out, err := json.Marshal(envelope)
		return out, err == nil
	}
	dt := h.siteRegistry(c).Get(doctypeName)
	delete(payload, "old_data")
	if nested {
		payload["data"] = visibleRealtimeFields(dt, data)
	} else {
		payload = visibleRealtimeFields(dt, data)
	}
	if hasData {
		envelope["data"] = payload
	} else {
		envelope["payload"] = payload
	}
	out, err := json.Marshal(envelope)
	return out, err == nil
}
