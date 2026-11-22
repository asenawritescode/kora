package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/api/ai"
	"github.com/asenawritescode/kora/internal/auth"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
)

const (
	maxExtensionReadConcurrency = 4
	extensionReadTimeout        = 5 * time.Second
)

// ExtensionReader answers the MCP read tools for one tenant-scoped extension credential. Every call authenticates the
// credential again and runs as the extension, so the grants are the ones the API applies to that extension.
type ExtensionReader struct {
	database *sql.DB
	dialect  kdb.Dialect
	registry *doctype.Registry
	tenantID contract.TenantID
	siteName string
	token    string
	slots    chan struct{}
	metrics  *api.MetricsReader
}

// ExtensionReaderOptions names the tenant, the site and the extension credential one reader serves.
type ExtensionReaderOptions struct {
	Database *sql.DB
	Dialect  kdb.Dialect
	Registry *doctype.Registry
	TenantID contract.TenantID
	Site     string
	Token    string
}

// NewExtensionReader validates the tenant route and credential shape and returns a bounded reader. It does not
// contact the database; the credential is checked on each call.
func NewExtensionReader(opts ExtensionReaderOptions) (*ExtensionReader, error) {
	if opts.Database == nil || opts.Dialect == nil || opts.Registry == nil {
		return nil, errors.New("MCP read executor requires database, dialect, and registry")
	}
	if _, err := contract.ParseTenantID(string(opts.TenantID)); err != nil {
		return nil, fmt.Errorf("MCP read executor tenant: %w", err)
	}
	if strings.TrimSpace(opts.Site) == "" || opts.Site != strings.TrimSpace(opts.Site) {
		return nil, errors.New("MCP read executor requires a canonical tenant route")
	}
	if opts.Token == "" || opts.Token != strings.TrimSpace(opts.Token) || len(opts.Token) > 4096 {
		return nil, errors.New("MCP read executor requires a valid extension credential")
	}
	return &ExtensionReader{
		database: opts.Database,
		dialect:  opts.Dialect,
		registry: opts.Registry,
		tenantID: opts.TenantID,
		siteName: opts.Site,
		token:    opts.Token,
		slots:    make(chan struct{}, maxExtensionReadConcurrency),
		metrics:  api.NewMetricsReader(opts.Registry, opts.Database, opts.Dialect),
	}, nil
}

// Execute runs one DocType read tool. It is the VerifiedToolExecutor of the server.
func (r *ExtensionReader) Execute(ctx context.Context, tool ai.ToolDescriptor, args map[string]any) (string, error) {
	if ctx == nil {
		return "", errors.New("MCP request context is required")
	}
	// The fixed record tools name their record type in the arguments, so the read is authorized
	// against the type the call actually names.
	_, permission, ok := ai.RecordToolTarget(r.registry, tool.Name, args)
	if tool.Source != "tenant" || tool.ID != "tenant."+tool.Name || !isReadOperation(tool.Operation) || !ok || permission != "read" {
		return "", errors.New("MCP tool is not an authorized DocType read operation")
	}

	queryCtx, release, err := r.admit(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	execution, err := r.authenticate(queryCtx)
	if err != nil {
		return "", err
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
	result := ai.ExecuteTool(tx, r.registry, ai.ToolRequest{Name: tool.Name, Args: args, Owner: owner, Site: r.siteName, Execution: execution}) //nolint:contextcheck // the context travels in tx.Context
	if strings.HasPrefix(result, "Error") {
		return "", errors.New("MCP read operation failed")
	}
	return result, nil
}

// Metrics is the MetricsFactory of the server. It authenticates the credential for one metrics call and returns the
// runner of that caller; each read it runs takes a read slot of its own.
func (r *ExtensionReader) Metrics(ctx context.Context) (ai.MetricsRunner, error) {
	if ctx == nil {
		return nil, errors.New("MCP request context is required")
	}
	authCtx, release, err := r.admit(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	execution, err := r.authenticate(authCtx)
	if err != nil {
		return nil, err
	}
	return func(route ai.MetricsRoute, body []byte) ai.MetricsResponse {
		callCtx, done, err := r.admit(ctx)
		if err != nil {
			return ai.MetricsResponse{Status: http.StatusServiceUnavailable, Body: []byte(`{"error":"MCP read operation timed out"}`)}
		}
		defer done()
		caller := api.MetricsCaller{Context: callCtx, Site: r.siteName, TenantID: string(r.tenantID), Execution: execution}
		return r.metrics.Runner(caller)(route, body) //nolint:contextcheck // the caller context travels in the request the runner clones
	}, nil
}

// InsightsAllowed is the InsightsGate of the reader's credential: it authenticates the credential and applies the
// rule the HTTP Insights routes apply to the roles it carries.
func (r *ExtensionReader) InsightsAllowed(ctx context.Context) (bool, error) {
	if ctx == nil {
		return false, errors.New("MCP request context is required")
	}
	authCtx, release, err := r.admit(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	execution, err := r.authenticate(authCtx)
	if err != nil {
		return false, err
	}
	return r.registry.InsightsAllowed(execution.Actor().Roles), nil
}

// admit takes one read slot and bounds the call by extensionReadTimeout. The returned function releases both.
func (r *ExtensionReader) admit(ctx context.Context) (context.Context, func(), error) {
	queryCtx, cancel := context.WithTimeout(ctx, extensionReadTimeout)
	select {
	case r.slots <- struct{}{}:
		return queryCtx, func() {
			<-r.slots
			cancel()
		}, nil
	case <-queryCtx.Done():
		cancel()
		return nil, nil, queryCtx.Err()
	}
}

// authenticate verifies the credential against the tenant's extension grants.
func (r *ExtensionReader) authenticate(ctx context.Context) (policy.Execution, error) {
	execution, _, err := auth.AuthenticateExtensionExecution(ctx, r.database, r.dialect, r.tenantID, r.siteName, r.token)
	if err != nil {
		if ctx.Err() != nil {
			return policy.Execution{}, ctx.Err()
		}
		// Authentication backends can expose database details in their cause; keep those internal to logs and
		// return a stable MCP error.
		return policy.Execution{}, errors.New("MCP extension authentication failed")
	}
	return execution, nil
}
