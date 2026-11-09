package sqlstore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/workflow"
)

var _ workflow.PreviewHistoryRepository = (*DefinitionStore)(nil)

func (s *DefinitionStore) RecordPreview(ctx context.Context, tenant contract.TenantID, actor contract.PrincipalID, run workflow.PreviewRun) error {
	if s == nil || s.database == nil || ctx == nil {
		return fmt.Errorf("workflow preview store is not configured")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return err
	}
	if _, err := contract.ParsePrincipalID(string(actor)); err != nil {
		return contract.NewError(contract.CodeUnauthenticated, "workflow preview actor is invalid")
	}
	if _, err := contract.ParseDefinitionID(string(run.DefinitionID)); err != nil || run.DefinitionVersion == 0 {
		return contract.NewError(contract.CodeValidationFailed, "workflow preview definition identity is invalid")
	}
	if run.ID == "" || len(run.DefinitionDigest) != 64 || run.Status != "succeeded" && run.Status != "failed" {
		return contract.NewError(contract.CodeValidationFailed, "workflow preview identity is incomplete")
	}
	if _, err := hex.DecodeString(run.DefinitionDigest); err != nil {
		return contract.NewError(contract.CodeValidationFailed, "workflow preview definition digest is invalid")
	}
	steps, err := json.Marshal(run.Steps)
	if err != nil {
		return fmt.Errorf("encode workflow preview steps: %w", err)
	}
	stamp := "CURRENT_TIMESTAMP"
	if s.dialect.DriverName() == "mysql" {
		stamp = "CURRENT_TIMESTAMP(6)"
	}
	query := `INSERT INTO _kora_workflow_preview (id,tenant_id,actor_id,definition_id,definition_version,definition_digest,status,outcome,failure,steps_json,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,` + stamp + `)`
	if _, err := s.database.ExecContext(ctx, db.Rebind(s.dialect, query), run.ID, string(tenant), string(actor), string(run.DefinitionID), run.DefinitionVersion, run.DefinitionDigest, run.Status, run.Outcome, run.Failure, string(steps)); err != nil {
		return fmt.Errorf("persist workflow preview run: %w", err)
	}
	return nil
}

func (s *DefinitionStore) ListPreviews(ctx context.Context, tenant contract.TenantID, limit uint16) ([]workflow.PreviewRun, error) {
	if s == nil || s.database == nil || ctx == nil {
		return nil, fmt.Errorf("workflow preview store is not configured")
	}
	if limit == 0 || limit > 100 {
		return nil, contract.NewError(contract.CodeValidationFailed, "workflow preview history limit must be between 1 and 100")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return nil, err
	}
	query := `SELECT id,definition_id,definition_version,definition_digest,status,outcome,failure,steps_json,created_at FROM _kora_workflow_preview WHERE tenant_id = ? ORDER BY created_at DESC,id DESC LIMIT ?`
	rows, err := s.database.QueryContext(ctx, db.Rebind(s.dialect, query), string(tenant), int64(limit))
	if err != nil {
		return nil, fmt.Errorf("list workflow preview runs: %w", err)
	}
	defer rows.Close()
	runs := make([]workflow.PreviewRun, 0)
	for rows.Next() {
		var run workflow.PreviewRun
		var steps string
		if err := rows.Scan(&run.ID, &run.DefinitionID, &run.DefinitionVersion, &run.DefinitionDigest, &run.Status, &run.Outcome, &run.Failure, &steps, &run.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan workflow preview run: %w", err)
		}
		if err := json.Unmarshal([]byte(steps), &run.Steps); err != nil {
			return nil, fmt.Errorf("decode workflow preview steps: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow preview runs: %w", err)
	}
	return runs, nil
}

// HasSuccessfulPreview checks the exact immutable definition content that is
// about to be published. Sample record values are never stored or consulted.
func (s *DefinitionStore) HasSuccessfulPreview(ctx context.Context, tenant contract.TenantID, id contract.DefinitionID, version uint32, digest string) (bool, error) {
	if s == nil || s.database == nil || ctx == nil {
		return false, fmt.Errorf("workflow preview store is not configured")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return false, err
	}
	if _, err := contract.ParseDefinitionID(string(id)); err != nil || version == 0 || len(digest) != 64 {
		return false, contract.NewError(contract.CodeValidationFailed, "workflow preview identity is invalid")
	}
	var exists int
	query := `SELECT 1 FROM _kora_workflow_preview WHERE tenant_id=? AND definition_id=? AND definition_version=? AND definition_digest=? AND status='succeeded' LIMIT 1`
	err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, query), string(tenant), string(id), version, digest).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check successful workflow preview: %w", err)
	}
	return true, nil
}
