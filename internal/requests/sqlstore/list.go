package sqlstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/requests"
)

type requestListCursor struct {
	Tenant    string               `json:"t"`
	Actor     string               `json:"a"`
	Manager   bool                 `json:"m"`
	View      contract.InboxView   `json:"v"`
	Kind      contract.RequestKind `json:"k,omitempty"`
	Overdue   bool                 `json:"o"`
	CreatedAt string               `json:"c"`
	ID        contract.RequestID   `json:"i"`
}

// ListVisible returns a bounded keyset page. The cursor is bound to the
// tenant, actor, manager scope and every filter; SQL independently enforces
// row visibility on every page.
func (s *Store) ListVisible(ctx context.Context, scope requests.VisibilityScope, filter contract.RequestFilter) (contract.Page[requests.RequestRecord], error) {
	if ctx == nil {
		return contract.Page[requests.RequestRecord]{}, fmt.Errorf("request listing requires a context")
	}
	if _, err := contract.ParseTenantID(string(scope.Tenant)); err != nil {
		return contract.Page[requests.RequestRecord]{}, fmt.Errorf("request tenant: %w", err)
	}
	if _, err := contract.ParsePrincipalID(string(scope.Actor)); err != nil {
		return contract.Page[requests.RequestRecord]{}, fmt.Errorf("request actor: %w", err)
	}
	if err := contract.ValidatePageRequest(filter.Page); err != nil {
		return contract.Page[requests.RequestRecord]{}, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	if filter.View != contract.InboxAction && filter.View != contract.InboxSent && filter.View != contract.InboxWaiting && filter.View != contract.InboxCompleted {
		return contract.Page[requests.RequestRecord]{}, contract.NewError(contract.CodeValidationFailed, "request inbox view is invalid")
	}
	var kind contract.RequestKind
	if filter.Kind != nil {
		kind = *filter.Kind
		if !validRequestKind(kind) {
			return contract.Page[requests.RequestRecord]{}, contract.NewError(contract.CodeValidationFailed, "request kind filter is invalid")
		}
	}
	var cursor requestListCursor
	var afterAt time.Time
	if filter.Page.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(string(filter.Page.Cursor))
		if err != nil || len(raw) > 512 {
			return contract.Page[requests.RequestRecord]{}, contract.NewError(contract.CodeValidationFailed, "request cursor is invalid")
		}
		if err := json.Unmarshal(raw, &cursor); err != nil {
			return contract.Page[requests.RequestRecord]{}, contract.NewError(contract.CodeValidationFailed, "request cursor is invalid")
		}
		if cursor.Tenant != string(scope.Tenant) || cursor.Actor != string(scope.Actor) || cursor.Manager != scope.CanManageAll || cursor.View != filter.View || cursor.Kind != kind || cursor.Overdue != filter.OverdueOnly {
			return contract.Page[requests.RequestRecord]{}, contract.NewError(contract.CodeValidationFailed, "request cursor belongs to a different scope or filter")
		}
		if _, err := contract.ParseRequestID(string(cursor.ID)); err != nil {
			return contract.Page[requests.RequestRecord]{}, contract.NewError(contract.CodeValidationFailed, "request cursor ID is invalid")
		}
		afterAt, err = time.Parse(time.RFC3339Nano, cursor.CreatedAt)
		if err != nil {
			return contract.Page[requests.RequestRecord]{}, contract.NewError(contract.CodeValidationFailed, "request cursor timestamp is invalid")
		}
	}
	query := `SELECT r.id FROM _kora_request r WHERE r.tenant_id = ?`
	args := []any{string(scope.Tenant)}
	if !scope.CanManageAll {
		query += ` AND (r.requester_id = ? OR EXISTS (SELECT 1 FROM _kora_request_assignment av WHERE av.tenant_id = r.tenant_id AND av.request_id = r.id AND av.assignee_kind = ? AND av.assignee_id = ?))`
		args = append(args, string(scope.Actor), string(contract.AssigneeUser), string(scope.Actor))
	}
	viewPredicate, viewArgs := requestListViewPredicate(filter.View, scope.Actor)
	query += ` AND ` + viewPredicate
	args = append(args, viewArgs...)
	if kind != "" {
		query += ` AND r.kind = ?`
		args = append(args, string(kind))
	}
	if filter.OverdueOnly {
		query += ` AND r.status = ? AND r.due_at IS NOT NULL AND r.due_at <= ` + s.dialect.NowTimestamp()
		args = append(args, string(contract.RequestPending))
	}
	if filter.Page.Cursor != "" {
		query += ` AND (r.created_at > ? OR (r.created_at = ? AND r.id > ?))`
		args = append(args, afterAt.UTC(), afterAt.UTC(), string(cursor.ID))
	}
	query += ` ORDER BY r.created_at, r.id LIMIT ?`
	args = append(args, int64(filter.Page.Limit)+1)
	rows, err := s.database.QueryContext(ctx, db.Rebind(s.dialect, query), args...)
	if err != nil {
		return contract.Page[requests.RequestRecord]{}, fmt.Errorf("query visible requests: %w", err)
	}
	ids := make([]contract.RequestID, 0, filter.Page.Limit+1)
	for rows.Next() {
		var id contract.RequestID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return contract.Page[requests.RequestRecord]{}, fmt.Errorf("scan visible request ID: %w", err)
		}
		if _, err := contract.ParseRequestID(string(id)); err != nil {
			_ = rows.Close()
			return contract.Page[requests.RequestRecord]{}, contract.NewError(contract.CodeInternal, "stored visible request ID is invalid")
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return contract.Page[requests.RequestRecord]{}, fmt.Errorf("read visible request IDs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return contract.Page[requests.RequestRecord]{}, fmt.Errorf("close visible request rows: %w", err)
	}
	hasMore := len(ids) > int(filter.Page.Limit)
	if hasMore {
		ids = ids[:filter.Page.Limit]
	}
	page := contract.Page[requests.RequestRecord]{Items: make([]requests.RequestRecord, 0, len(ids))}
	var lastExamined *requests.RequestRecord
	for _, id := range ids {
		record, err := s.Get(ctx, scope.Tenant, id)
		if err != nil {
			return contract.Page[requests.RequestRecord]{}, fmt.Errorf("load visible request %s: %w", id, err)
		}
		lastExamined = &record
		if (!scope.CanManageAll && !recordVisibleTo(record, scope.Actor)) || !matchesInboxView(record, scope.Actor, filter.View) || (filter.Kind != nil && record.Kind != *filter.Kind) {
			continue
		}
		page.Items = append(page.Items, record)
	}
	if hasMore && lastExamined != nil {
		last := *lastExamined
		kindValue := kind
		cursor = requestListCursor{Tenant: string(scope.Tenant), Actor: string(scope.Actor), Manager: scope.CanManageAll, View: filter.View, Kind: kindValue, Overdue: filter.OverdueOnly, CreatedAt: last.CreatedAt.UTC().Format(time.RFC3339Nano), ID: last.ID}
		encoded, err := json.Marshal(cursor)
		if err != nil {
			return contract.Page[requests.RequestRecord]{}, fmt.Errorf("encode request cursor: %w", err)
		}
		page.NextCursor = contract.PageCursor(base64.RawURLEncoding.EncodeToString(encoded))
	}
	return page, nil
}

func requestListViewPredicate(view contract.InboxView, actor contract.PrincipalID) (string, []any) {
	pending := string(contract.RequestPending)
	user := string(contract.AssigneeUser)
	switch view {
	case contract.InboxAction:
		return `r.status = ? AND EXISTS (SELECT 1 FROM _kora_request_assignment a WHERE a.tenant_id = r.tenant_id AND a.request_id = r.id AND a.assignee_kind = ? AND a.assignee_id = ? AND a.status = ?)`, []any{pending, user, string(actor), string(contract.AssignmentPending)}
	case contract.InboxSent:
		return `r.requester_id = ? AND r.status = ? AND NOT EXISTS (SELECT 1 FROM _kora_request_assignment a WHERE a.tenant_id = r.tenant_id AND a.request_id = r.id AND a.assignee_kind = ? AND a.assignee_id = ? AND a.status = ?)`, []any{string(actor), pending, user, string(actor), string(contract.AssignmentPending)}
	case contract.InboxWaiting:
		return `r.status = ? AND EXISTS (SELECT 1 FROM _kora_request_assignment a WHERE a.tenant_id = r.tenant_id AND a.request_id = r.id AND a.assignee_kind = ? AND a.assignee_id = ? AND a.status IN (?, ?))`, []any{pending, user, string(actor), string(contract.AssignmentBlocked), string(contract.AssignmentResponded)}
	case contract.InboxCompleted:
		return `r.status IN (?, ?, ?, ?)`, []any{string(contract.RequestResolved), string(contract.RequestCancelled), string(contract.RequestExpired), string(contract.RequestStale)}
	default:
		return `1 = 0`, nil
	}
}

func validRequestKind(value contract.RequestKind) bool {
	switch value {
	case contract.RequestApproval, contract.RequestReview, contract.RequestInformation, contract.RequestAcknowledgment, contract.RequestTask:
		return true
	default:
		return false
	}
}

func recordVisibleTo(record requests.RequestRecord, actor contract.PrincipalID) bool {
	if record.Requester == actor {
		return true
	}
	for _, assignment := range record.Assignments {
		if assignment.Principal == actor {
			return true
		}
	}
	return false
}

func matchesInboxView(record requests.RequestRecord, actor contract.PrincipalID, view contract.InboxView) bool {
	switch view {
	case contract.InboxAction:
		if record.Status != contract.RequestPending {
			return false
		}
		for _, assignment := range record.Assignments {
			if assignment.Principal == actor && assignment.Status == contract.AssignmentPending {
				return true
			}
		}
	case contract.InboxSent:
		if record.Requester != actor || record.Status != contract.RequestPending {
			return false
		}
		for _, assignment := range record.Assignments {
			if assignment.Principal == actor && assignment.Status == contract.AssignmentPending {
				return false
			}
		}
		return true
	case contract.InboxWaiting:
		if record.Status != contract.RequestPending {
			return false
		}
		for _, assignment := range record.Assignments {
			if assignment.Principal == actor && (assignment.Status == contract.AssignmentBlocked || assignment.Status == contract.AssignmentResponded) {
				return true
			}
		}
	case contract.InboxCompleted:
		return record.Status == contract.RequestResolved || record.Status == contract.RequestCancelled || record.Status == contract.RequestExpired || record.Status == contract.RequestStale
	}
	return false
}
