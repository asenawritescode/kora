package recon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

// BusinessRun is the durable result of comparing one immutable rule version at
// a cutoff. Tenant and entity scope are part of the persisted identity.
type BusinessRun struct {
	TenantID  contract.TenantID      `json:"tenant_id"`
	ID        string                 `json:"id"`
	RuleID    string                 `json:"rule_id"`
	Version   uint32                 `json:"version"`
	Scope     contract.BusinessScope `json:"scope"`
	Cutoff    time.Time              `json:"cutoff"`
	Complete  bool                   `json:"complete"`
	Findings  []BusinessFinding      `json:"findings,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
}

// BusinessRunStore persists reconciliation evidence through the existing SQL
// dialect abstraction. A run and its findings are replaced atomically so a
// retry with the same run ID cannot leave a partial result.
type BusinessRunStore struct {
	DB      *sql.DB
	Dialect db.QueryDialect
}

type BusinessWaiverAuthorizer interface {
	CanWaiveBusinessFinding(context.Context, contract.TenantID, contract.BusinessScope, string) error
}

type BusinessRepairAuthorizer interface {
	CanRepairBusinessFinding(context.Context, contract.TenantID, contract.BusinessScope, string) error
}

type BusinessOwnerRepairer interface {
	RepairBusinessObligation(context.Context, BusinessObligation, contract.IdempotencyKey) (string, error)
}

func (s *BusinessRunStore) EnsureSchema(ctx context.Context) error {
	if s == nil || s.DB == nil || s.Dialect == nil {
		return fmt.Errorf("business reconciliation store requires database and dialect")
	}
	if ctx == nil {
		return fmt.Errorf("business reconciliation schema requires context")
	}
	runPayload, findingPayload := "TEXT", "TEXT"
	if driver, ok := s.Dialect.(interface{ DriverName() string }); ok && driver.DriverName() == "mysql" {
		runPayload, findingPayload = "LONGTEXT", "LONGTEXT"
	}
	queries := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS _kora_business_recon_run (tenant_id VARCHAR(140) NOT NULL, run_id VARCHAR(160) NOT NULL, rule_id VARCHAR(160) NOT NULL, rule_version INTEGER NOT NULL, scope_json %s NOT NULL, cutoff_at VARCHAR(40) NOT NULL, complete SMALLINT NOT NULL, created_at VARCHAR(40) NOT NULL, PRIMARY KEY (tenant_id, run_id))`, runPayload),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS _kora_business_recon_finding (tenant_id VARCHAR(140) NOT NULL, run_id VARCHAR(160) NOT NULL, finding_id VARCHAR(80) NOT NULL, finding_json %s NOT NULL, state VARCHAR(32) NOT NULL, evidence %s NOT NULL, PRIMARY KEY (tenant_id, run_id, finding_id))`, findingPayload, findingPayload),
		`CREATE TABLE IF NOT EXISTS _kora_business_recon_audit (tenant_id VARCHAR(140) NOT NULL, run_id VARCHAR(160) NOT NULL, finding_id VARCHAR(80) NOT NULL, actor_id VARCHAR(160) NOT NULL, action VARCHAR(32) NOT NULL, reason VARCHAR(1000) NOT NULL, occurred_at VARCHAR(40) NOT NULL, PRIMARY KEY (tenant_id, run_id, finding_id, action))`,
		`CREATE TABLE IF NOT EXISTS _kora_business_recon_repair (tenant_id VARCHAR(140) NOT NULL, run_id VARCHAR(160) NOT NULL, finding_id VARCHAR(80) NOT NULL, idempotency_key VARCHAR(100) NOT NULL, state VARCHAR(32) NOT NULL, evidence VARCHAR(1000) NOT NULL, updated_at VARCHAR(40) NOT NULL, PRIMARY KEY (tenant_id, run_id, finding_id))`,
	}
	for _, query := range queries {
		if _, err := s.DB.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("create business reconciliation schema: %w", err)
		}
	}
	return nil
}

// Repair invokes the owning domain command with a stable idempotency key. The
// durable intent is committed first; uncertain retries reuse the same key.
// The engine never writes the missing business obligation itself.
func (s *BusinessRunStore) Repair(ctx context.Context, tenant contract.TenantID, runID, findingID, actor string, auth BusinessRepairAuthorizer, owner BusinessOwnerRepairer) error {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil || auth == nil || owner == nil {
		return fmt.Errorf("business finding repair requires context, store, authorizer, and owner command")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return err
	}
	if runID == "" || findingID == "" || actor == "" {
		return fmt.Errorf("business finding repair identity is required")
	}
	run, err := s.Load(ctx, tenant, runID)
	if err != nil {
		return err
	}
	var expected *BusinessObligation
	for i := range run.Findings {
		if run.Findings[i].ID == findingID {
			copy := run.Findings[i].Expected
			expected = &copy
			break
		}
	}
	if expected == nil || expected.Key == "" {
		return fmt.Errorf("business finding has no repairable owner obligation")
	}
	if err := auth.CanRepairBusinessFinding(ctx, tenant, run.Scope, actor); err != nil {
		return fmt.Errorf("business finding repair denied: %w", err)
	}
	idempotency := businessRepairIdempotencyKey(tenant, runID, findingID)
	query := func(q string) string { return db.Rebind(s.Dialect, q) }
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := func(cause error) error { _ = tx.Rollback(); return cause }
	var state string
	if err := tx.QueryRowContext(ctx, query(`SELECT state FROM _kora_business_recon_finding WHERE tenant_id = ? AND run_id = ? AND finding_id = ?`), string(tenant), runID, findingID).Scan(&state); err != nil {
		return rollback(err)
	}
	if state == string(BusinessFindingResolved) {
		return rollback(fmt.Errorf("business finding is already resolved"))
	}
	if state == string(BusinessFindingWaived) || state != string(BusinessFindingOpen) && state != string(BusinessFindingInvestigating) {
		return rollback(fmt.Errorf("business finding cannot be repaired from state %q", state))
	}
	var storedKey, repairState string
	err = tx.QueryRowContext(ctx, query(`SELECT idempotency_key, state FROM _kora_business_recon_repair WHERE tenant_id = ? AND run_id = ? AND finding_id = ?`), string(tenant), runID, findingID).Scan(&storedKey, &repairState)
	if err == sql.ErrNoRows {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, query(`INSERT INTO _kora_business_recon_repair (tenant_id, run_id, finding_id, idempotency_key, state, evidence, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`), string(tenant), runID, findingID, string(idempotency), "pending", "", now); err != nil {
			return rollback(err)
		}
		if _, err := tx.ExecContext(ctx, query(`INSERT INTO _kora_business_recon_audit (tenant_id, run_id, finding_id, actor_id, action, reason, occurred_at) VALUES (?, ?, ?, ?, ?, ?, ?)`), string(tenant), runID, findingID, actor, "repair_requested", "owner command requested", now); err != nil {
			return rollback(err)
		}
	} else if err != nil {
		return rollback(err)
	} else if storedKey != string(idempotency) {
		return rollback(fmt.Errorf("repair idempotency identity conflicts"))
	} else if repairState == "completed" {
		return rollback(fmt.Errorf("repair already completed; reconcile the owner result"))
	}
	if state == string(BusinessFindingOpen) {
		result, err := tx.ExecContext(ctx, query(`UPDATE _kora_business_recon_finding SET state = ? WHERE tenant_id = ? AND run_id = ? AND finding_id = ? AND state = ?`), string(BusinessFindingInvestigating), string(tenant), runID, findingID, state)
		if err != nil {
			return rollback(err)
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			if err == nil {
				err = fmt.Errorf("business finding changed during repair admission")
			}
			return rollback(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("persist business repair intent: %w", err)
	}
	evidence, err := owner.RepairBusinessObligation(ctx, *expected, idempotency)
	if err != nil {
		return fmt.Errorf("owner business repair remains pending reconciliation: %w", err)
	}
	if evidence == "" || len(evidence) > 1000 {
		return fmt.Errorf("owner repair must return bounded evidence")
	}
	tx, err = s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, query(`UPDATE _kora_business_recon_finding SET state = ?, evidence = ? WHERE tenant_id = ? AND run_id = ? AND finding_id = ? AND state = ?`), string(BusinessFindingResolved), evidence, string(tenant), runID, findingID, string(BusinessFindingInvestigating)); err != nil {
		_ = tx.Rollback()
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, query(`UPDATE _kora_business_recon_repair SET state = ?, evidence = ?, updated_at = ? WHERE tenant_id = ? AND run_id = ? AND finding_id = ? AND idempotency_key = ?`), "completed", evidence, now, string(tenant), runID, findingID, string(idempotency)); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, query(`INSERT INTO _kora_business_recon_audit (tenant_id, run_id, finding_id, actor_id, action, reason, occurred_at) VALUES (?, ?, ?, ?, ?, ?, ?)`), string(tenant), runID, findingID, actor, "repair_completed", evidence, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit owner repair evidence: %w", err)
	}
	return nil
}

func businessRepairIdempotencyKey(tenant contract.TenantID, runID, findingID string) contract.IdempotencyKey {
	hash := sha256.Sum256([]byte(string(tenant) + "\x00" + runID + "\x00" + findingID))
	return contract.IdempotencyKey("recon-repair-" + hex.EncodeToString(hash[:20]))
}

func (s *BusinessRunStore) Save(ctx context.Context, run BusinessRun) error {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil {
		return fmt.Errorf("business reconciliation save requires context and store")
	}
	if _, err := contract.ParseTenantID(string(run.TenantID)); err != nil {
		return fmt.Errorf("business reconciliation tenant: %w", err)
	}
	if run.ID == "" || run.RuleID == "" || run.Version == 0 || run.Scope.Validate() != nil || run.Scope.TenantID != run.TenantID || run.Cutoff.IsZero() || len(run.Findings) > 10000 {
		return fmt.Errorf("business reconciliation run identity, scope, cutoff, or bounds are invalid")
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = time.Now().UTC()
	}
	scopeJSON, err := json.Marshal(run.Scope)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := func(cause error) error { _ = tx.Rollback(); return cause }
	query := func(q string) string { return db.Rebind(s.Dialect, q) }
	if _, err := tx.ExecContext(ctx, query(`DELETE FROM _kora_business_recon_finding WHERE tenant_id = ? AND run_id = ?`), string(run.TenantID), run.ID); err != nil {
		return rollback(err)
	}
	if _, err := tx.ExecContext(ctx, query(`DELETE FROM _kora_business_recon_run WHERE tenant_id = ? AND run_id = ?`), string(run.TenantID), run.ID); err != nil {
		return rollback(err)
	}
	complete := int64(0)
	if run.Complete {
		complete = 1
	}
	if _, err := tx.ExecContext(ctx, query(`INSERT INTO _kora_business_recon_run (tenant_id, run_id, rule_id, rule_version, scope_json, cutoff_at, complete, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`), string(run.TenantID), run.ID, run.RuleID, run.Version, string(scopeJSON), run.Cutoff.UTC().Format(time.RFC3339Nano), complete, run.CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return rollback(err)
	}
	for _, finding := range run.Findings {
		if finding.ID == "" || finding.Key == "" {
			return rollback(fmt.Errorf("business reconciliation finding identity is incomplete"))
		}
		if finding.State == "" {
			finding.State = BusinessFindingOpen
		}
		payload, err := json.Marshal(finding)
		if err != nil {
			return rollback(err)
		}
		if _, err := tx.ExecContext(ctx, query(`INSERT INTO _kora_business_recon_finding (tenant_id, run_id, finding_id, finding_json, state, evidence) VALUES (?, ?, ?, ?, ?, ?)`), string(run.TenantID), run.ID, finding.ID, string(payload), string(finding.State), finding.Evidence); err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit business reconciliation run: %w", err)
	}
	return nil
}

func (s *BusinessRunStore) Load(ctx context.Context, tenant contract.TenantID, runID string) (BusinessRun, error) {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil {
		return BusinessRun{}, fmt.Errorf("business reconciliation load requires context and store")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return BusinessRun{}, err
	}
	if runID == "" {
		return BusinessRun{}, fmt.Errorf("business reconciliation run ID is required")
	}
	query := func(q string) string { return db.Rebind(s.Dialect, q) }
	var run BusinessRun
	var scopeJSON, cutoff, created string
	var complete int64
	err := s.DB.QueryRowContext(ctx, query(`SELECT rule_id, rule_version, scope_json, cutoff_at, complete, created_at FROM _kora_business_recon_run WHERE tenant_id = ? AND run_id = ?`), string(tenant), runID).Scan(&run.RuleID, &run.Version, &scopeJSON, &cutoff, &complete, &created)
	if err != nil {
		return BusinessRun{}, err
	}
	run.TenantID, run.ID, run.Complete = tenant, runID, complete != 0
	if err := json.Unmarshal([]byte(scopeJSON), &run.Scope); err != nil {
		return BusinessRun{}, err
	}
	if run.Cutoff, err = time.Parse(time.RFC3339Nano, cutoff); err != nil {
		return BusinessRun{}, err
	}
	if run.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return BusinessRun{}, err
	}
	rows, err := s.DB.QueryContext(ctx, query(`SELECT finding_json, state, evidence FROM _kora_business_recon_finding WHERE tenant_id = ? AND run_id = ? ORDER BY finding_id LIMIT 10000`), string(tenant), runID)
	if err != nil {
		return BusinessRun{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var payload, state, evidence string
		if err := rows.Scan(&payload, &state, &evidence); err != nil {
			return BusinessRun{}, err
		}
		var finding BusinessFinding
		if err := json.Unmarshal([]byte(payload), &finding); err != nil {
			return BusinessRun{}, err
		}
		finding.State, finding.Evidence = BusinessFindingState(state), evidence
		run.Findings = append(run.Findings, finding)
	}
	if err := rows.Err(); err != nil {
		return BusinessRun{}, err
	}
	return run, nil
}

// Waive records an authorized, reasoned decision and its evidence atomically
// with the finding state transition. Waivers do not alter source business data.
func (s *BusinessRunStore) Waive(ctx context.Context, tenant contract.TenantID, runID, findingID, actor, reason string, auth BusinessWaiverAuthorizer) error {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil || auth == nil {
		return fmt.Errorf("business finding waiver requires context, store, and authorizer")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return err
	}
	if runID == "" || findingID == "" || actor == "" || strings.TrimSpace(reason) == "" || len(reason) > 1000 {
		return fmt.Errorf("business finding waiver requires identity and a bounded reason")
	}
	run, err := s.Load(ctx, tenant, runID)
	if err != nil {
		return err
	}
	if err := auth.CanWaiveBusinessFinding(ctx, tenant, run.Scope, actor); err != nil {
		return fmt.Errorf("business finding waiver denied: %w", err)
	}
	query := func(q string) string { return db.Rebind(s.Dialect, q) }
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := func(cause error) error { _ = tx.Rollback(); return cause }
	var state string
	if err := tx.QueryRowContext(ctx, query(`SELECT state FROM _kora_business_recon_finding WHERE tenant_id = ? AND run_id = ? AND finding_id = ?`), string(tenant), runID, findingID).Scan(&state); err != nil {
		return rollback(err)
	}
	if state == string(BusinessFindingWaived) {
		return rollback(fmt.Errorf("business finding is already waived"))
	}
	if state != string(BusinessFindingOpen) && state != string(BusinessFindingInvestigating) {
		return rollback(fmt.Errorf("business finding cannot be waived from state %q", state))
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, query(`UPDATE _kora_business_recon_finding SET state = ?, evidence = ? WHERE tenant_id = ? AND run_id = ? AND finding_id = ? AND state = ?`), string(BusinessFindingWaived), reason, string(tenant), runID, findingID, state)
	if err != nil {
		return rollback(err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err == nil {
			err = fmt.Errorf("business finding changed during waiver")
		}
		return rollback(err)
	}
	if _, err := tx.ExecContext(ctx, query(`INSERT INTO _kora_business_recon_audit (tenant_id, run_id, finding_id, actor_id, action, reason, occurred_at) VALUES (?, ?, ?, ?, ?, ?, ?)`), string(tenant), runID, findingID, actor, "waive", reason, now); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit business finding waiver: %w", err)
	}
	return nil
}
