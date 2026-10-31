// Package engineadapter wires the purchasing domain into Engine's generic
// tenant-service extension points. Business policy and transition names stay
// owned by purchasing; Engine only receives validated interfaces and maps.
package engineadapter

import (
	"database/sql"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/internal/application/process"
	"github.com/asenawritescode/kora/internal/application/purchasing"
	purchasingsql "github.com/asenawritescode/kora/internal/application/purchasing/sqlstore"
	"github.com/asenawritescode/kora/internal/engine"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/requests"
	workflowstore "github.com/asenawritescode/kora/workflow/sqlstore"
)

// Configure registers the purchasing request policy and its local workflow
// transition mapping in the host's Engine configuration.
func Configure(config *engine.Config) {
	if config == nil {
		return
	}
	config.RequestAuthorizer = func(database *sql.DB, dialect db.Dialect, registry *doctype.Registry, direct requests.Authorizer) (requests.Authorizer, error) {
		store, err := purchasingsql.New(database, dialect)
		if err != nil {
			return nil, err
		}
		policies, err := workflowstore.NewDefinitionStore(database, dialect)
		if err != nil {
			return nil, err
		}
		return purchasing.NewRequestAuthorizer(direct, store, registry, policies)
	}
	config.WorkflowApprovals = map[string]process.WorkflowApproval{
		purchasing.ApprovalPolicyID: {SubjectKind: purchasing.PurchaseRequest, Action: purchasing.ApprovalAction, RejectAction: "Reject"},
	}
	if config.Workflow != nil {
		config.Workflow.ServiceGrants = append(config.Workflow.ServiceGrants,
			policy.Grant{Resource: purchasing.PurchaseRequest, Operation: "read"},
			policy.Grant{Resource: purchasing.WorkflowApprovalSubmitGrant, Operation: "submit"},
		)
	}
}
