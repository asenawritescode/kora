package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/requests"
)

// Response returns a stored decision only for a requester, current assigned
// participant or explicit manager. It deliberately leaves information schema
// validation and field-level projection to the authorized request service.
func (s *Store) Response(ctx context.Context, scope requests.VisibilityScope, requestID contract.RequestID, decisionID contract.DecisionID) (requests.DecisionRecord, error) {
	if ctx == nil {
		return requests.DecisionRecord{}, fmt.Errorf("request response lookup requires a context")
	}
	if _, err := contract.ParseTenantID(string(scope.Tenant)); err != nil {
		return requests.DecisionRecord{}, fmt.Errorf("request tenant: %w", err)
	}
	if _, err := contract.ParsePrincipalID(string(scope.Actor)); err != nil {
		return requests.DecisionRecord{}, fmt.Errorf("request reader: %w", err)
	}
	if _, err := contract.ParseRequestID(string(requestID)); err != nil {
		return requests.DecisionRecord{}, err
	}
	if _, err := contract.ParseDecisionID(string(decisionID)); err != nil {
		return requests.DecisionRecord{}, err
	}
	query := `SELECT d.id, d.tenant_id, d.request_id, d.assignment_id, d.actor_id, d.on_behalf_of_id, d.response, d.comment, d.information_json, d.created_at
		FROM _kora_request_decision d JOIN _kora_request r ON r.tenant_id = d.tenant_id AND r.id = d.request_id
		WHERE d.tenant_id = ? AND d.request_id = ? AND d.id = ?`
	args := []any{string(scope.Tenant), string(requestID), string(decisionID)}
	if !scope.CanManageAll {
		query += ` AND (r.requester_id = ? OR EXISTS (SELECT 1 FROM _kora_request_assignment a WHERE a.tenant_id = r.tenant_id AND a.request_id = r.id AND a.assignee_kind = ? AND a.assignee_id = ?))`
		args = append(args, string(scope.Actor), string(contract.AssigneeUser), string(scope.Actor))
	}
	var record requests.DecisionRecord
	var information sql.NullString
	var onBehalf sql.NullString
	var at sql.NullString
	err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, query), args...).Scan(
		&record.ID, &record.Tenant, &record.RequestID, &record.AssignmentID, &record.Actor, &onBehalf, &record.Response, &record.Comment, &information, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return requests.DecisionRecord{}, contract.NewError(contract.CodeNotFound, "request response not found")
	}
	if err != nil {
		return requests.DecisionRecord{}, fmt.Errorf("load authorized request response: %w", err)
	}
	if record.Tenant != scope.Tenant || record.RequestID != requestID || record.ID != decisionID {
		return requests.DecisionRecord{}, contract.NewError(contract.CodeInternal, "stored response identity is inconsistent")
	}
	if _, err := contract.ParseAssignmentID(string(record.AssignmentID)); err != nil {
		return requests.DecisionRecord{}, contract.NewError(contract.CodeInternal, "stored response assignment ID is invalid")
	}
	if _, err := contract.ParsePrincipalID(string(record.Actor)); err != nil {
		return requests.DecisionRecord{}, contract.NewError(contract.CodeInternal, "stored response actor is invalid")
	}
	if onBehalf.Valid && onBehalf.String != "" {
		value := contract.PrincipalID(onBehalf.String)
		if _, err := contract.ParsePrincipalID(onBehalf.String); err != nil {
			return requests.DecisionRecord{}, contract.NewError(contract.CodeInternal, "stored response delegation principal is invalid")
		}
		record.OnBehalfOf = value
	}
	if !validResponseKind(record.Response) {
		return requests.DecisionRecord{}, contract.NewError(contract.CodeInternal, "stored response kind is invalid")
	}
	if information.Valid {
		var value contract.InformationResponse
		if err := json.Unmarshal([]byte(information.String), &value); err != nil || value.SchemaID == "" || value.SchemaVersion == 0 || len(value.Values) == 0 || !json.Valid(value.Values) {
			return requests.DecisionRecord{}, contract.NewError(contract.CodeInternal, "stored information response is invalid")
		}
		record.Information = &value
	}
	if (record.Response == contract.ResponseProvide) != (record.Information != nil) {
		return requests.DecisionRecord{}, contract.NewError(contract.CodeInternal, "stored response information does not match its response kind")
	}
	if !at.Valid {
		return requests.DecisionRecord{}, contract.NewError(contract.CodeInternal, "stored response timestamp is missing")
	}
	record.At, err = parseDBTime(at.String)
	if err != nil {
		return requests.DecisionRecord{}, fmt.Errorf("parse request response timestamp: %w", err)
	}
	return record, nil
}
