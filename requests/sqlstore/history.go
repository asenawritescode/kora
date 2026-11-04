package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/requests"
)

// History returns a stable sequence page only when the verified visibility
// scope contains the requester, a current assigned participant, or a manager.
// A missing and an unauthorized request both return NotFound.
func (s *Store) History(ctx context.Context, scope requests.VisibilityScope, id contract.RequestID, page contract.PageRequest) (contract.Page[contract.RequestHistoryEntry], error) {
	if ctx == nil {
		return contract.Page[contract.RequestHistoryEntry]{}, fmt.Errorf("request history requires a context")
	}
	if _, err := contract.ParseTenantID(string(scope.Tenant)); err != nil {
		return contract.Page[contract.RequestHistoryEntry]{}, fmt.Errorf("request tenant: %w", err)
	}
	if _, err := contract.ParsePrincipalID(string(scope.Actor)); err != nil {
		return contract.Page[contract.RequestHistoryEntry]{}, fmt.Errorf("request reader: %w", err)
	}
	if _, err := contract.ParseRequestID(string(id)); err != nil {
		return contract.Page[contract.RequestHistoryEntry]{}, err
	}
	if err := contract.ValidatePageRequest(page); err != nil {
		return contract.Page[contract.RequestHistoryEntry]{}, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	cursor := int64(0)
	if page.Cursor != "" {
		parsed, err := contract.ParseRevision(string(page.Cursor))
		if err != nil {
			return contract.Page[contract.RequestHistoryEntry]{}, contract.NewError(contract.CodeValidationFailed, "history cursor is invalid")
		}
		cursor, err = strconv.ParseInt(string(parsed), 10, 64)
		if err != nil || cursor < 0 {
			return contract.Page[contract.RequestHistoryEntry]{}, contract.NewError(contract.CodeValidationFailed, "history cursor is invalid")
		}
	}
	visibleQuery := `SELECT EXISTS (SELECT 1 FROM _kora_request r WHERE r.tenant_id = ? AND r.id = ?`
	visibleArgs := []any{string(scope.Tenant), string(id)}
	if !scope.CanManageAll {
		visibleQuery += ` AND (r.requester_id = ? OR EXISTS (SELECT 1 FROM _kora_request_assignment a WHERE a.tenant_id = r.tenant_id AND a.request_id = r.id AND a.assignee_kind = ? AND a.assignee_id = ?))`
		visibleArgs = append(visibleArgs, string(scope.Actor), string(contract.AssigneeUser), string(scope.Actor))
	}
	visibleQuery += `)`
	var visible bool
	if err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, visibleQuery), visibleArgs...).Scan(&visible); err != nil {
		return contract.Page[contract.RequestHistoryEntry]{}, fmt.Errorf("check request history visibility: %w", err)
	}
	if !visible {
		return contract.Page[contract.RequestHistoryEntry]{}, contract.NewError(contract.CodeNotFound, "request not found")
	}
	query := `SELECT h.sequence, h.kind, h.actor_id, h.assignment_id, h.response, h.comment, h.previous_assignee_json, h.new_assignee_json, h.created_at
		FROM _kora_request_history h WHERE h.tenant_id = ? AND h.request_id = ? AND h.sequence > ? AND EXISTS (
		SELECT 1 FROM _kora_request r WHERE r.tenant_id = h.tenant_id AND r.id = h.request_id`
	args := []any{string(scope.Tenant), string(id), cursor}
	if !scope.CanManageAll {
		query += ` AND (r.requester_id = ? OR EXISTS (SELECT 1 FROM _kora_request_assignment a WHERE a.tenant_id = r.tenant_id AND a.request_id = r.id AND a.assignee_kind = ? AND a.assignee_id = ?))`
		args = append(args, string(scope.Actor), string(contract.AssigneeUser), string(scope.Actor))
	}
	query += `) ORDER BY h.sequence LIMIT ?`
	args = append(args, int64(page.Limit)+1)
	rows, err := s.database.QueryContext(ctx, db.Rebind(s.dialect, query), args...)
	if err != nil {
		return contract.Page[contract.RequestHistoryEntry]{}, fmt.Errorf("query request history: %w", err)
	}
	defer rows.Close()
	items := make([]contract.RequestHistoryEntry, 0, page.Limit+1)
	for rows.Next() {
		var sequence int64
		var kind string
		var actor, assignmentID, response, previousJSON, nextJSON, at sql.NullString
		var comment string
		if err := rows.Scan(&sequence, &kind, &actor, &assignmentID, &response, &comment, &previousJSON, &nextJSON, &at); err != nil {
			return contract.Page[contract.RequestHistoryEntry]{}, fmt.Errorf("scan request history: %w", err)
		}
		entry := contract.RequestHistoryEntry{Sequence: contract.Revision(strconv.FormatInt(sequence, 10)), Kind: contract.RequestHistoryKind(kind), Comment: comment}
		if sequence < 1 || !validHistoryKind(entry.Kind) {
			return contract.Page[contract.RequestHistoryEntry]{}, contract.NewError(contract.CodeInternal, "stored request history sequence or kind is invalid")
		}
		if actor.Valid {
			value := contract.PrincipalID(actor.String)
			if _, err := contract.ParsePrincipalID(actor.String); err != nil {
				return contract.Page[contract.RequestHistoryEntry]{}, contract.NewError(contract.CodeInternal, "stored history actor is invalid")
			}
			entry.Actor = &value
		}
		if assignmentID.Valid {
			value := contract.AssignmentID(assignmentID.String)
			if _, err := contract.ParseAssignmentID(assignmentID.String); err != nil {
				return contract.Page[contract.RequestHistoryEntry]{}, contract.NewError(contract.CodeInternal, "stored history assignment ID is invalid")
			}
			entry.AssignmentID = value
		}
		if response.Valid {
			value := contract.ResponseKind(response.String)
			if !validResponseKind(value) {
				return contract.Page[contract.RequestHistoryEntry]{}, contract.NewError(contract.CodeInternal, "stored history response is invalid")
			}
			entry.Response = &value
		}
		if previousJSON.Valid || nextJSON.Valid {
			if !previousJSON.Valid || !nextJSON.Valid || entry.Kind != contract.HistoryReassigned {
				return contract.Page[contract.RequestHistoryEntry]{}, contract.NewError(contract.CodeInternal, "stored history transfer is incomplete")
			}
			previous, err := decodeHistoryAssignee(previousJSON.String)
			if err != nil {
				return contract.Page[contract.RequestHistoryEntry]{}, err
			}
			next, err := decodeHistoryAssignee(nextJSON.String)
			if err != nil {
				return contract.Page[contract.RequestHistoryEntry]{}, err
			}
			entry.PreviousAssignee, entry.NewAssignee = &previous, &next
		}
		if entry.Kind == contract.HistoryReassigned && (entry.PreviousAssignee == nil || entry.NewAssignee == nil) {
			return contract.Page[contract.RequestHistoryEntry]{}, contract.NewError(contract.CodeInternal, "stored reassignment history lacks transfer details")
		}
		if !at.Valid {
			return contract.Page[contract.RequestHistoryEntry]{}, contract.NewError(contract.CodeInternal, "stored history timestamp is missing")
		}
		parsedAt, err := parseDBTime(at.String)
		if err != nil {
			return contract.Page[contract.RequestHistoryEntry]{}, fmt.Errorf("parse request history time: %w", err)
		}
		entry.At = parsedAt.UTC()
		items = append(items, entry)
	}
	if err := rows.Err(); err != nil {
		return contract.Page[contract.RequestHistoryEntry]{}, fmt.Errorf("read request history: %w", err)
	}
	result := contract.Page[contract.RequestHistoryEntry]{Items: items}
	if len(items) > int(page.Limit) {
		result.Items = items[:page.Limit]
		result.NextCursor = contract.PageCursor(result.Items[len(result.Items)-1].Sequence)
	}
	return result, nil
}

func validHistoryKind(value contract.RequestHistoryKind) bool {
	switch value {
	case contract.HistoryCreated, contract.HistoryAssigned, contract.HistoryResponded, contract.HistoryReassigned, contract.HistoryResolved, contract.HistoryCancelled, contract.HistoryExpired, contract.HistoryStale:
		return true
	default:
		return false
	}
}

func validResponseKind(value contract.ResponseKind) bool {
	switch value {
	case contract.ResponseApprove, contract.ResponseReject, contract.ResponseReturn, contract.ResponseProvide, contract.ResponseAcknowledge, contract.ResponseComplete:
		return true
	default:
		return false
	}
}

func decodeHistoryAssignee(raw string) (contract.Assignee, error) {
	var value contract.Assignee
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return contract.Assignee{}, contract.NewError(contract.CodeInternal, "stored history assignee JSON is invalid")
	}
	if value.Kind != contract.AssigneeUser {
		return contract.Assignee{}, contract.NewError(contract.CodeInternal, "stored history assignee must be an expanded user")
	}
	if _, err := contract.ParsePrincipalID(value.ID); err != nil {
		return contract.Assignee{}, contract.NewError(contract.CodeInternal, "stored history assignee is invalid")
	}
	return value, nil
}
