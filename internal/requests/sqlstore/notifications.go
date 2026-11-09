package sqlstore

import (
	"context"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
)

const maxDirectTaskNotificationRecipients = 51

// DirectTaskNotificationRecipients resolves only the small, current set of
// active direct-task participants. It returns identifiers, never request
// content; later notification delivery remains a recipient-scoped
// invalidation and the request API rechecks visibility when the client refetches.
func (s *Store) DirectTaskNotificationRecipients(ctx context.Context, tenant contract.TenantID, site string, requestID contract.RequestID, eventType string) ([]contract.PrincipalID, error) {
	if ctx == nil || site == "" {
		return nil, contract.NewError(contract.CodeValidationFailed, "notification recipient lookup requires a tenant route and context")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return nil, err
	}
	if _, err := contract.ParseRequestID(string(requestID)); err != nil {
		return nil, err
	}
	base := `r.tenant_id = ? AND r.id = ? AND r.kind = ? AND r.policy_id = ''
		AND r.subject_kind IS NULL AND r.source_instance_id = '' AND r.source_step_id = ''`
	baseArgs := []any{string(tenant), string(requestID), string(contract.RequestTask)}
	var participantSQL string
	var args []any
	switch eventType {
	case "request.created":
		participantSQL = `SELECT a.assignee_id AS principal_id FROM _kora_request r
			JOIN _kora_request_assignment a ON a.tenant_id = r.tenant_id AND a.request_id = r.id
			WHERE ` + base + ` AND a.assignee_kind = 'user' AND a.status = 'pending'`
		args = append(args, baseArgs...)
	case "request.responded", "request.reassigned":
		participantSQL = `SELECT r.requester_id AS principal_id FROM _kora_request r
			WHERE ` + base + ` UNION SELECT a.assignee_id AS principal_id FROM _kora_request r
			JOIN _kora_request_assignment a ON a.tenant_id = r.tenant_id AND a.request_id = r.id
			WHERE ` + base + ` AND a.assignee_kind = 'user' AND a.status = 'pending'`
		args = append(args, baseArgs...)
		args = append(args, baseArgs...)
	case "request.cancelled", "request.expired", "request.stale":
		participantSQL = `SELECT r.requester_id AS principal_id FROM _kora_request r
			WHERE ` + base + ` UNION SELECT a.assignee_id AS principal_id FROM _kora_request r
			JOIN _kora_request_assignment a ON a.tenant_id = r.tenant_id AND a.request_id = r.id
			WHERE ` + base + ` AND a.assignee_kind = 'user'`
		args = append(args, baseArgs...)
		args = append(args, baseArgs...)
	default:
		return nil, contract.NewError(contract.CodeValidationFailed, "request event type is not eligible for direct-task notification")
	}
	query := `SELECT DISTINCT p.principal_id FROM (` + participantSQL + `) p
		JOIN _kora_user u ON u.site = ? AND u.name = p.principal_id AND u.enabled = ?
		ORDER BY p.principal_id LIMIT ?`
	args = append(args, site, db.BoolParam(s.dialect, true), maxDirectTaskNotificationRecipients+1)
	rows, err := s.database.QueryContext(ctx, db.Rebind(s.dialect, query), args...)
	if err != nil {
		return nil, fmt.Errorf("resolve direct-task notification recipients: %w", err)
	}
	defer rows.Close()
	recipients := make([]contract.PrincipalID, 0, maxDirectTaskNotificationRecipients)
	for rows.Next() {
		var principal string
		if err := rows.Scan(&principal); err != nil {
			return nil, fmt.Errorf("read direct-task notification recipient: %w", err)
		}
		if _, err := contract.ParsePrincipalID(principal); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "stored direct-task notification recipient is invalid")
		}
		recipients = append(recipients, contract.PrincipalID(principal))
		if len(recipients) > maxDirectTaskNotificationRecipients {
			return nil, contract.NewError(contract.CodeResourceExhausted, "direct-task notification recipient count exceeds the limit")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate direct-task notification recipients: %w", err)
	}
	return recipients, nil
}
