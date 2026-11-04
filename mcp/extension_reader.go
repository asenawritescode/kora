package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/asenawritescode/kora/api/ai"
	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
)

const (
	maxExtensionReadConcurrency = 4
	extensionReadTimeout        = 5 * time.Second
)

// NewExtensionReadExecutor creates a bounded MCP executor for a tenant-scoped
// extension credential. It refreshes the credential and its exact grants on
// every call, and executes only the catalog's read-only DocType tools.
func NewExtensionReadExecutor(database *sql.DB, dialect kdb.Dialect, registry *doctype.Registry, tenantID contract.TenantID, siteName, token string) (VerifiedToolExecutor, error) {
	if database == nil || dialect == nil || registry == nil {
		return nil, errors.New("MCP read executor requires database, dialect, and registry")
	}
	if _, err := contract.ParseTenantID(string(tenantID)); err != nil {
		return nil, fmt.Errorf("MCP read executor tenant: %w", err)
	}
	if strings.TrimSpace(siteName) == "" || siteName != strings.TrimSpace(siteName) {
		return nil, errors.New("MCP read executor requires a canonical tenant route")
	}
	if token == "" || token != strings.TrimSpace(token) || len(token) > 4096 {
		return nil, errors.New("MCP read executor requires a valid extension credential")
	}

	reader := &extensionReadExecutor{
		database: database,
		dialect:  dialect,
		registry: registry,
		tenantID: tenantID,
		siteName: siteName,
		token:    token,
		slots:    make(chan struct{}, maxExtensionReadConcurrency),
	}
	return reader.execute, nil
}

type extensionReadExecutor struct {
	database *sql.DB
	dialect  kdb.Dialect
	registry *doctype.Registry
	tenantID contract.TenantID
	siteName string
	token    string
	slots    chan struct{}
}

func (r *extensionReadExecutor) execute(ctx context.Context, tool ai.ToolDescriptor, args map[string]any) (string, error) {
	if ctx == nil {
		return "", errors.New("MCP request context is required")
	}
	dt := r.registry.Get(tool.Doctype)
	if tool.Source != "tenant" || tool.ID != "tenant."+tool.Name || !isReadOperation(tool.Operation) || dt == nil || tool.Name != sanitizeName(dt.Name)+"_"+tool.Operation {
		return "", errors.New("MCP tool is not an authorized DocType read operation")
	}

	queryCtx, cancel := context.WithTimeout(ctx, extensionReadTimeout)
	defer cancel()
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-queryCtx.Done():
		return "", queryCtx.Err()
	}

	execution, _, err := auth.AuthenticateExtensionExecution(queryCtx, r.database, r.dialect, r.tenantID, r.siteName, r.token)
	if err != nil {
		if queryCtx.Err() != nil {
			return "", queryCtx.Err()
		}
		// Authentication backends can expose database details in their cause;
		// keep those internal to logs and return a stable MCP error.
		return "", errors.New("MCP extension authentication failed")
	}
	actor := execution.Actor()
	owner := actor.SubjectUserID
	if owner == "" {
		owner = actor.PrincipalID
	}
	tx := &orm.TxManager{
		DB: r.database, Registry: r.registry, Dialect: r.dialect,
		Context: queryCtx, SiteName: r.siteName, Execution: execution,
		CurrentUser: owner, CurrentUserRoles: append([]string(nil), actor.Roles...),
	}
	result := ai.ExecuteToolWithExecution(tx, r.registry, tool.Name, args, owner, r.siteName, execution)
	if strings.HasPrefix(result, "Error") {
		return "", errors.New("MCP read operation failed")
	}
	return result, nil
}
