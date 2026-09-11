package inventory

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

type PurchaseRequestState string

const (
	PurchaseDraft     PurchaseRequestState = "draft"
	PurchaseSubmitted PurchaseRequestState = "submitted"
	PurchaseApproved  PurchaseRequestState = "approved"
	PurchaseRejected  PurchaseRequestState = "rejected"
	PurchaseOrdered   PurchaseRequestState = "ordered"
	PurchaseReceived  PurchaseRequestState = "received"
)

type PurchaseRequest struct {
	ID          string               `json:"id"`
	Item        string               `json:"item"`
	Location    string               `json:"location"`
	Quantity    int64                `json:"quantity"`
	Supplier    string               `json:"supplier,omitempty"`
	Reason      string               `json:"reason"`
	RequestedBy string               `json:"requested_by"`
	State       PurchaseRequestState `json:"state"`
	CreatedAt   time.Time            `json:"created_at"`
	ApprovedBy  string               `json:"approved_by,omitempty"`
	ApprovedAt  time.Time            `json:"approved_at,omitempty"`
	SourceAlert LowStockAlert        `json:"source_alert"`
}

type Procurement struct {
	mu       sync.RWMutex
	requests map[string]PurchaseRequest
}

func NewProcurement() *Procurement { return &Procurement{requests: map[string]PurchaseRequest{}} }

func (p *Procurement) CreateFromLowStock(id, item, location string, quantity int64, actor, reason string, alert LowStockAlert) (PurchaseRequest, error) {
	if id == "" || item == "" || location == "" || quantity <= 0 || actor == "" || strings.TrimSpace(reason) == "" {
		return PurchaseRequest{}, fmt.Errorf("purchase request requires id, item, location, positive quantity, actor, and reason")
	}
	if alert.Item != item || alert.Location != location || alert.Balance >= alert.Threshold {
		return PurchaseRequest{}, fmt.Errorf("purchase request source is not a low-stock alert for %s at %s", item, location)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.requests[id]; exists {
		return PurchaseRequest{}, fmt.Errorf("purchase request %q already exists", id)
	}
	request := PurchaseRequest{ID: id, Item: item, Location: location, Quantity: quantity, Reason: reason, RequestedBy: actor, State: PurchaseDraft, CreatedAt: time.Now().UTC(), SourceAlert: alert}
	p.requests[id] = request
	return request, nil
}

func (p *Procurement) Transition(id string, next PurchaseRequestState, actor string) (PurchaseRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	request, ok := p.requests[id]
	if !ok {
		return PurchaseRequest{}, fmt.Errorf("purchase request %q not found", id)
	}
	if actor == "" {
		return PurchaseRequest{}, fmt.Errorf("purchase request transition requires actor")
	}
	if !purchaseTransition(request.State, next) {
		return PurchaseRequest{}, fmt.Errorf("invalid purchase request transition %s -> %s", request.State, next)
	}
	request.State = next
	if next == PurchaseApproved {
		request.ApprovedBy, request.ApprovedAt = actor, time.Now().UTC()
	}
	p.requests[id] = request
	return request, nil
}

func (p *Procurement) Get(id string) (PurchaseRequest, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r, ok := p.requests[id]
	return r, ok
}

func purchaseTransition(from, to PurchaseRequestState) bool {
	switch from {
	case PurchaseDraft:
		return to == PurchaseSubmitted || to == PurchaseRejected
	case PurchaseSubmitted:
		return to == PurchaseApproved || to == PurchaseRejected
	case PurchaseApproved:
		return to == PurchaseOrdered || to == PurchaseRejected
	case PurchaseOrdered:
		return to == PurchaseReceived
	default:
		return false
	}
}
