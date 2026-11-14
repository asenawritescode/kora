package engine

import (
	"log/slog"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/management"
)

// createTenantSteps are the fixed stage names site.CreateSite wraps its errors
// with. Only these reach the operation; the raw error stays in the Engine log.
var createTenantSteps = []string{
	"connecting to platform database", "initializing platform site directory", "persisting platform site provisioning intent",
	"creating database", "connecting to database", "opening libsql connection", "pinging libsql",
	"bootstrapping system tables", "creating admin user", "seeding tenant display name",
	"checking initial config version", "creating initial site config version", "inserting initial config version",
	"recovering existing initial config version", "recovering initial config version activation",
	"activating site in platform directory", "updating active flag", "verifying active flag",
}

// provisionStep names where a tenant provision stopped, and why.
type provisionStep string

const (
	stepAdmission     provisionStep = "admission"
	stepReadDirectory provisionStep = "read tenant directory"
	stepCreateTenant  provisionStep = "create tenant"
	stepActivate      provisionStep = "activate tenant"
)

// detail narrows "create tenant" to the CreateSite stage named in err.
func (s provisionStep) detail(err error) string {
	if s != stepCreateTenant || err == nil {
		return string(s)
	}
	text := err.Error()
	for _, stage := range createTenantSteps {
		if strings.Contains(text, stage+":") {
			return string(s) + ": " + stage
		}
	}
	return string(s)
}

// stopped records a provision step whose outcome is uncertain. The full error
// goes to the Engine log with the operation and tenant; the operation keeps a
// safe message naming the step, so Cloud and operators can see where it stopped.
func (s provisionStep) stopped(operation management.Operation, err error) management.FinishResult {
	step := s.detail(err)
	slog.Warn("tenant provisioning stopped", "operation_id", operation.ID, "tenant_id", operation.Tenant, "step", step, "error", err)
	return management.FinishResult{
		State: contract.OperationRecoveryRequired,
		Error: contract.NewError(contract.CodeDependencyUnavailable, "tenant provisioning stopped at step \""+step+"\"; its outcome is uncertain: read the Engine log for this operation, fix the cause and reconcile it"),
	}
}

// refused logs a definite provision failure so the Engine log shows it next to its time.
func refused(operation management.Operation, code contract.Code, message string) management.FinishResult {
	slog.Warn("tenant provisioning refused", "operation_id", operation.ID, "tenant_id", operation.Tenant, "code", code, "reason", message)
	return management.FinishResult{State: contract.OperationFailed, Error: contract.NewError(code, message)}
}
