package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/inventory"
	"github.com/gin-gonic/gin"
)

func (h *Handler) inventoryLedger(c *gin.Context) (*inventory.Ledger, string, bool) {
	site := strings.TrimSpace(c.GetString("site_name"))
	if site == "" {
		badRequestError(c, "request.no_tenant_context", "no tenant context", nil)
		return nil, "", false
	}
	h.inventoryMu.Lock()
	defer h.inventoryMu.Unlock()
	if h.SiteInventoryLedgers == nil {
		h.SiteInventoryLedgers = map[string]*inventory.Ledger{}
	}
	ledger := h.SiteInventoryLedgers[site]
	if ledger == nil {
		ledger = inventory.NewLedger(inventory.Policy{})
		h.SiteInventoryLedgers[site] = ledger
	}
	return ledger, site, true
}

func (h *Handler) HandleInventoryMovement(c *gin.Context) {
	ledger, site, ok := h.inventoryLedger(c)
	if !ok {
		return
	}
	var movement inventory.Movement
	if err := c.ShouldBindJSON(&movement); err != nil {
		badRequestError(c, "validation.invalid_json", "Invalid movement format", nil)
		return
	}
	if movement.Actor == "" {
		movement.Actor = c.GetString("user")
	}
	if movement.OccurredAt.IsZero() {
		movement.OccurredAt = time.Now().UTC()
	}
	if err := ledger.Append(movement); err != nil {
		badRequestError(c, "inventory.movement_rejected", err.Error(), nil)
		return
	}
	h.emitInventoryEvent(c, site, movement)
	c.JSON(http.StatusCreated, Response{Data: movement})
}

func (h *Handler) HandleInventoryMovements(c *gin.Context) {
	ledger, _, ok := h.inventoryLedger(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, Response{Data: ledger.Movements()})
}

func (h *Handler) HandleInventoryBalance(c *gin.Context) {
	ledger, _, ok := h.inventoryLedger(c)
	if !ok {
		return
	}
	item, location := strings.TrimSpace(c.Query("item")), strings.TrimSpace(c.Query("location"))
	if item == "" || location == "" {
		badRequestError(c, "validation.required_field", "item and location are required", map[string]any{"fields": []string{"item", "location"}})
		return
	}
	c.JSON(http.StatusOK, Response{Data: gin.H{"item": item, "location": location, "balance": ledger.Balance(item, location), "reserved": ledger.Reserved(item, location), "available": ledger.Available(item, location)}})
}

func (h *Handler) emitInventoryEvent(c *gin.Context, site string, movement inventory.Movement) {
	provider := h.SiteRealtimeProviders[site]
	if provider == nil {
		return
	}
	payload, err := json.Marshal(movement)
	if err != nil {
		return
	}
	event := contract.EventEnvelope{ID: contract.NewEventID(), Type: "inventory.movement.recorded", Version: contract.CurrentVersion, Source: "inventory", Site: site, AggregateType: "stock_movement", AggregateID: movement.ID, Data: payload, OccurredAt: movement.OccurredAt, Actor: contract.ActorContext{PrincipalID: movement.Actor, PrincipalType: contract.PrincipalHuman, Site: site}}
	_ = provider.Publish(c.Request.Context(), event)
}
