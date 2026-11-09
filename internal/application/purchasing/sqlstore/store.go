// Package sqlstore implements the tenant-backed data reads for purchasing
// approval policy. It does not decide who is eligible; that remains in the
// purchasing application package.
package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/application/purchasing"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
)

type Store struct {
	database *sql.DB
	dialect  db.Dialect
}

func New(database *sql.DB, dialect db.Dialect) (*Store, error) {
	if database == nil || dialect == nil {
		return nil, fmt.Errorf("purchasing approval store requires database and dialect")
	}
	return &Store{database: database, dialect: dialect}, nil
}

var _ purchasing.ApprovalStore = (*Store)(nil)

func (s *Store) AuthorizationRevisions(ctx context.Context) (contract.Revision, contract.Revision, error) {
	if ctx == nil {
		return "", "", fmt.Errorf("load purchasing authorization revisions requires a context")
	}
	var policyRevision, membershipRevision int64
	if err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT policy_revision, membership_revision FROM _kora_auth_revision WHERE id = 1`)).Scan(&policyRevision, &membershipRevision); err != nil {
		return "", "", fmt.Errorf("load purchasing authorization revisions: %w", err)
	}
	if policyRevision < 1 || membershipRevision < 1 {
		return "", "", contract.NewError(contract.CodeInternal, "tenant authorization revision is invalid")
	}
	return contract.Revision(fmt.Sprint(policyRevision)), contract.Revision(fmt.Sprint(membershipRevision)), nil
}

func (s *Store) UserRoles(ctx context.Context, site string, principal contract.PrincipalID) ([]string, bool, error) {
	if ctx == nil || strings.TrimSpace(site) == "" || principal == "" {
		return nil, false, contract.NewError(contract.CodeValidationFailed, "purchasing membership lookup is invalid")
	}
	var enabled bool
	var raw string
	err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT enabled, roles FROM _kora_user WHERE site = ? AND name = ?`), site, string(principal)).Scan(&enabled, &raw)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load current purchasing membership: %w", err)
	}
	return parseRoles(raw), enabled, nil
}

func (s *Store) UsersWithRoles(ctx context.Context, site string, required []string, limit int) ([]purchasing.UserRoles, error) {
	if ctx == nil || strings.TrimSpace(site) == "" || len(required) == 0 || limit < 1 || limit > 51 {
		return nil, contract.NewError(contract.CodeValidationFailed, "purchasing reviewer query bounds are invalid")
	}
	driver, ok := s.dialect.(interface{ DriverName() string })
	if !ok {
		return nil, fmt.Errorf("purchasing reviewer lookup requires a named SQL dialect")
	}
	conditions := make([]string, 0, len(required))
	args := []any{site, db.BoolParam(s.dialect, true)}
	for _, role := range required {
		switch driver.DriverName() {
		case "mysql":
			conditions = append(conditions, `FIND_IN_SET(?, REPLACE(roles, ', ', ',')) > 0`)
		case "postgres":
			conditions = append(conditions, `POSITION(',' || ? || ',' IN ',' || REPLACE(roles, ', ', ',') || ',') > 0`)
		case "libsql":
			conditions = append(conditions, `instr(',' || REPLACE(roles, ', ', ',') || ',', ',' || ? || ',') > 0`)
		default:
			return nil, fmt.Errorf("purchasing reviewer lookup does not support dialect %q", driver.DriverName())
		}
		args = append(args, role)
	}
	args = append(args, limit)
	query := `SELECT name, roles FROM _kora_user WHERE site = ? AND enabled = ? AND (` + strings.Join(conditions, " OR ") + `) ORDER BY name LIMIT ?`
	rows, err := s.database.QueryContext(ctx, db.Rebind(s.dialect, query), args...)
	if err != nil {
		return nil, fmt.Errorf("resolve eligible purchasing reviewers: %w", err)
	}
	defer rows.Close()
	users := make([]purchasing.UserRoles, 0, limit)
	for rows.Next() {
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, fmt.Errorf("read eligible purchasing reviewer: %w", err)
		}
		principal, err := contract.ParsePrincipalID(name)
		if err != nil {
			return nil, contract.NewError(contract.CodeInternal, "stored purchasing reviewer identity is invalid")
		}
		users = append(users, purchasing.UserRoles{Principal: principal, Roles: parseRoles(raw)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate eligible purchasing reviewers: %w", err)
	}
	return users, nil
}

func (s *Store) Subject(ctx context.Context, registry *doctype.Registry, reference contract.SubjectRef) (*doctype.Document, string, error) {
	if ctx == nil || registry == nil || reference.Kind == "" || reference.ID == "" {
		return nil, "", contract.NewError(contract.CodeValidationFailed, "purchasing subject lookup is invalid")
	}
	dt := registry.Get(reference.Kind)
	if dt == nil || dt.Name != "Purchase Request" {
		return nil, "", contract.NewError(contract.CodeNotFound, "Purchase Request subject is unavailable")
	}
	tx := &orm.TxManager{DB: s.database, Registry: registry, Dialect: s.dialect, Context: ctx}
	document, err := tx.GetDoc(dt, reference.ID, "")
	if err != nil {
		return nil, "", contract.NewError(contract.CodeNotFound, "Purchase Request subject is unavailable")
	}
	table := s.dialect.QuoteIdent(dt.RawTableName())
	var owner string
	if err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT `+s.dialect.QuoteIdent("owner")+` FROM `+table+` WHERE `+s.dialect.QuoteIdent("name")+` = ?`), reference.ID).Scan(&owner); err != nil {
		return nil, "", fmt.Errorf("read Purchase Request owner: %w", err)
	}
	return document, owner, nil
}

func parseRoles(raw string) []string {
	seen := make(map[string]struct{})
	var roles []string
	for _, item := range strings.Split(raw, ",") {
		role := strings.TrimSpace(item)
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
