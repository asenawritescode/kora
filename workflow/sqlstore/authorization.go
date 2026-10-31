package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/workflow"
)

// AuthorizationStore reads live membership, subject ownership and persisted
// workflow starter data for the application policy layer.
type AuthorizationStore struct {
	database *sql.DB
	dialect  db.Dialect
	registry *doctype.Registry
}

func NewAuthorizationStore(database *sql.DB, dialect db.Dialect, registry *doctype.Registry) (*AuthorizationStore, error) {
	if database == nil || dialect == nil || registry == nil {
		return nil, fmt.Errorf("workflow authorization store requires database, dialect and tenant registry")
	}
	return &AuthorizationStore{database: database, dialect: dialect, registry: registry}, nil
}

var _ workflow.AuthorizationStore = (*AuthorizationStore)(nil)

func (s *AuthorizationStore) UserRoles(ctx context.Context, site string, principal contract.PrincipalID) ([]string, bool, error) {
	if ctx == nil || strings.TrimSpace(site) == "" || principal == "" {
		return nil, false, contract.NewError(contract.CodeValidationFailed, "workflow membership lookup is invalid")
	}
	var enabled bool
	var raw string
	err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT enabled, roles FROM _kora_user WHERE site = ? AND name = ?`), site, string(principal)).Scan(&enabled, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load current workflow membership: %w", err)
	}
	return parseWorkflowRoles(raw), enabled, nil
}

func (s *AuthorizationStore) Subject(ctx context.Context, subject contract.SubjectRef) (workflow.SubjectAuthorization, error) {
	if ctx == nil || subject.Kind == "" || subject.ID == "" {
		return workflow.SubjectAuthorization{}, contract.NewError(contract.CodeValidationFailed, "workflow subject lookup is invalid")
	}
	dt := s.registry.Get(subject.Kind)
	if dt == nil || dt.Name != subject.Kind {
		return workflow.SubjectAuthorization{}, contract.NewError(contract.CodeNotFound, "workflow subject is unavailable")
	}
	// Keep the subject row and owner snapshot in one transaction. LibSQL's
	// supported HTTP transport rejects the read-only transaction option, so
	// this read-only unit uses the driver's default transaction mode.
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return workflow.SubjectAuthorization{}, fmt.Errorf("begin workflow subject authorization read: %w", err)
	}
	defer tx.Rollback()
	manager := &orm.TxManager{DB: s.database, Transaction: tx, Registry: s.registry, Dialect: s.dialect, Context: ctx}
	document, err := manager.GetDoc(dt, subject.ID, "")
	if err != nil {
		if errors.Is(err, orm.ErrNotFound) {
			return workflow.SubjectAuthorization{}, contract.NewError(contract.CodeNotFound, "workflow subject is unavailable")
		}
		return workflow.SubjectAuthorization{}, fmt.Errorf("load workflow subject: %w", err)
	}
	table := s.dialect.QuoteIdent(dt.RawTableName())
	name := s.dialect.QuoteIdent("name")
	ownerColumn := s.dialect.QuoteIdent("owner")
	var owner string
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT `+ownerColumn+` FROM `+table+` WHERE `+name+` = ?`), subject.ID).Scan(&owner); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return workflow.SubjectAuthorization{}, contract.NewError(contract.CodeNotFound, "workflow subject is unavailable")
		}
		return workflow.SubjectAuthorization{}, fmt.Errorf("load workflow subject owner: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return workflow.SubjectAuthorization{}, fmt.Errorf("commit workflow subject authorization read: %w", err)
	}
	userFields := make(map[string]string)
	for _, field := range dt.DataFields() {
		if field.Fieldtype == "User" {
			if value := document.GetString(field.Fieldname); value != "" {
				userFields[field.Fieldname] = value
			}
		}
	}
	return workflow.SubjectAuthorization{Owner: contract.PrincipalID(owner), Revision: contract.Revision(fmt.Sprint(document.Revision)), UserFields: userFields}, nil
}

func (s *AuthorizationStore) WorkflowStarter(ctx context.Context, tenant contract.TenantID, instance contract.InstanceID) (contract.PrincipalID, error) {
	if ctx == nil || tenant == "" || instance == "" {
		return "", contract.NewError(contract.CodeValidationFailed, "workflow starter lookup is invalid")
	}
	var actor string
	err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT actor_id FROM _kora_workflow_history WHERE tenant_id = ? AND instance_id = ? AND sequence = 1 AND kind = 'started'`), string(tenant), string(instance)).Scan(&actor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", contract.NewError(contract.CodeNotFound, "workflow starter is unavailable")
	}
	if err != nil {
		return "", fmt.Errorf("load workflow starter: %w", err)
	}
	principal, err := contract.ParsePrincipalID(actor)
	if err != nil {
		return "", contract.NewError(contract.CodeInternal, "stored workflow starter identity is invalid")
	}
	return principal, nil
}

func parseWorkflowRoles(raw string) []string {
	seen := make(map[string]struct{})
	roles := make([]string, 0)
	for _, value := range strings.Split(raw, ",") {
		role := strings.TrimSpace(value)
		if role == "" {
			continue
		}
		if _, exists := seen[role]; exists {
			continue
		}
		seen[role] = struct{}{}
		roles = append(roles, role)
	}
	return roles
}
