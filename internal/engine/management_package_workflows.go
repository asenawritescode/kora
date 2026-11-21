package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/management"
	workflowsql "github.com/asenawritescode/kora/internal/workflow/sqlstore"
)

// packageWorkflowActor publishes package workflow definitions, as the config
// import path publishes with its own system actor.
const packageWorkflowActor = "system-package-activation"

// errPackageWorkflowConflict marks a definition whose id and version already
// exist with other content. Versions are immutable: the package must bump it.
var errPackageWorkflowConflict = errors.New("package workflow definition conflicts with an immutable published version")

// packageActivationWrite is everything the activation transaction needs.
type packageActivationWrite struct {
	database *sql.DB
	dialect  db.Dialect
	tenant   contract.TenantID
	site     string
	target   *doctype.ConfigSnapshot
	commit   configstore.ManagementActivationCommit
	replaced map[contract.ModuleID]bool
}

// commitPackageActivation commits config, modules, the journal and the target's
// workflow definitions in one transaction. A failed publish rolls the activation
// back, and a committed activation always has its workflows. A retry replays the
// same rows, which the batch accepts when id, version and digest match.
func commitPackageActivation(ctx context.Context, write packageActivationWrite) (string, error) {
	versionID, err := commitPackageActivationTx(ctx, write)
	if err != nil {
		slog.ErrorContext(ctx, "management package activation commit failed", "operation_id", write.commit.OperationID, "tenant_id", write.tenant, "error", err)
	}
	return versionID, err
}

func commitPackageActivationTx(ctx context.Context, write packageActivationWrite) (string, error) {
	tx, err := write.database.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin package activation: %w", err)
	}
	defer tx.Rollback()
	if inFlightErr := refuseReplacedModulesInFlight(ctx, tx, write.dialect, write.tenant, write.replaced); inFlightErr != nil {
		return "", inFlightErr
	}
	versionID, _, err := configstore.NewStore(write.database, write.dialect).CommitManagementActivation(ctx, tx, write.target, write.site, write.dialect, write.commit)
	if err != nil {
		return "", err
	}
	if err := publishPackageWorkflows(ctx, tx, write); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit package activation: %w", err)
	}
	return versionID, nil
}

// publishPackageWorkflows publishes every definition of the merged target, as
// the config import path does, inside the activation transaction.
func publishPackageWorkflows(ctx context.Context, tx *sql.Tx, write packageActivationWrite) error {
	definitions, err := workflowsql.NewDefinitionStore(write.database, write.dialect)
	if err != nil {
		return err
	}
	actor, err := contract.ParsePrincipalID(packageWorkflowActor)
	if err != nil {
		return err
	}
	err = definitions.PublishImportedBatchTx(ctx, tx, workflowsql.ImportedBatch{
		Tenant: write.tenant, Actor: actor, Definitions: write.target.WorkflowDefinitions, Now: time.Now().UTC(),
	})
	var conflict *contract.Error
	if errors.As(err, &conflict) && conflict.Type == contract.CodeConflict {
		return fmt.Errorf("%w: %w", errPackageWorkflowConflict, err)
	}
	return err
}

// packageActivationFailure classifies a failed activation transaction. Only a
// known refusal is final; any other error may leave the commit outcome unknown.
func packageActivationFailure(err error) management.FinishResult {
	if refusal, ok := inFlightFailure(err); ok {
		return refusal
	}
	switch {
	case errors.Is(err, configstore.ErrConfigRevisionConflict):
		return failed(contract.CodeConflict, "tenant configuration changed during package activation")
	case errors.Is(err, errPackageWorkflowConflict):
		return failed(contract.CodeConflict, truncateUTF8(err.Error(), 480))
	}
	return packageRecoveryRequired("package configuration commit needs recovery")
}
