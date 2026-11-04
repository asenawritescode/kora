package recon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

// BusinessRun is the durable result of comparing one immutable rule version at
// a cutoff. Tenant and entity scope are part of the persisted identity.
type BusinessRun struct {
	TenantID      contract.TenantID      `json:"tenant_id"`
	OwnerModuleID contract.ModuleID      `json:"owner_module_id"`
	ID            string                 `json:"id"`
	RuleID        string                 `json:"rule_id"`
	Version       uint32                 `json:"version"`
	Scope         contract.BusinessScope `json:"scope"`
	Cutoff        time.Time              `json:"cutoff"`
	Complete      bool                   `json:"complete"`
	Cursors       BusinessRunCursor      `json:"cursors,omitempty"`
	Watermarks    BusinessRunWatermarks  `json:"watermarks,omitempty"`
	Findings      []BusinessFinding      `json:"findings,omitempty"`
	CreatedAt     time.Time              `json:"created_at"`
}

// BusinessRunStore persists reconciliation evidence through the existing SQL
// dialect abstraction. Run IDs are immutable: retries use a new run ID and
// finding identities remain stable across those runs. This preserves prior
// evidence and audit state.
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

type BusinessRunReader interface {
	CanReadBusinessRun(context.Context, contract.TenantID, string, string) error
}

type BusinessRunAuthorizer interface {
	CanRunBusinessReconciliation(context.Context, contract.TenantID, contract.BusinessScope, string) error
}

type BusinessFindingSummaryAuthorizer interface {
	CanInspectBusinessFindings(context.Context, contract.BusinessScope, string) error
}

var ErrBusinessFindingSummaryDenied = errors.New("business finding summary authorization denied")

type BusinessOwnerRepairer interface {
	RepairBusinessObligation(context.Context, BusinessObligation, contract.IdempotencyKey) (string, error)
}

type BusinessRunRepository interface {
	Save(context.Context, BusinessRun) error
}

type BusinessReconciliationService struct {
	Runs     BusinessRunRepository
	Store    *BusinessRunStore
	ReadPage BusinessQueryPageReader
}

func (s BusinessReconciliationService) GetRun(ctx context.Context, tenant contract.TenantID, runID, actor string, auth BusinessRunReader) (BusinessRun, error) {
	if s.Store == nil {
		return BusinessRun{}, fmt.Errorf("business reconciliation store is unavailable")
	}
	return s.Store.LoadAuthorized(ctx, tenant, runID, actor, auth)
}

func (s BusinessReconciliationService) WaiveFinding(ctx context.Context, tenant contract.TenantID, runID, findingID, actor, reason string, auth BusinessWaiverAuthorizer) error {
	if s.Store == nil {
		return fmt.Errorf("business reconciliation store is unavailable")
	}
	return s.Store.Waive(ctx, tenant, runID, findingID, actor, reason, auth)
}

func (s BusinessReconciliationService) RepairFinding(ctx context.Context, tenant contract.TenantID, runID, findingID, actor string, auth BusinessRepairAuthorizer, owner BusinessOwnerRepairer) error {
	if s.Store == nil {
		return fmt.Errorf("business reconciliation store is unavailable")
	}
	return s.Store.Repair(ctx, tenant, runID, findingID, actor, auth, owner)
}

func (s BusinessReconciliationService) CountUnresolvedFindings(ctx context.Context, scope contract.BusinessScope, actor string, auth BusinessFindingSummaryAuthorizer) (uint32, error) {
	if s.Store == nil || ctx == nil || actor == "" || auth == nil {
		return 0, fmt.Errorf("business reconciliation store is unavailable")
	}
	if err := auth.CanInspectBusinessFindings(ctx, scope, actor); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrBusinessFindingSummaryDenied, err)
	}
	return s.Store.CountUnresolvedFindingsInScope(ctx, scope)
}

func (s *BusinessRunStore) LoadAuthorized(ctx context.Context, tenant contract.TenantID, runID, actor string, auth BusinessRunReader) (BusinessRun, error) {
	if ctx == nil || auth == nil || actor == "" {
		return BusinessRun{}, fmt.Errorf("business reconciliation read requires context, actor, and authorizer")
	}
	if err := auth.CanReadBusinessRun(ctx, tenant, runID, actor); err != nil {
		return BusinessRun{}, fmt.Errorf("business reconciliation read denied: %w", err)
	}
	return s.Load(ctx, tenant, runID)
}

// CountUnresolvedFindings returns a tenant-scoped count for operator summaries.
// It includes open and investigating business findings, and never includes
// delivery failures or any finding payload.
func (s *BusinessRunStore) CountUnresolvedFindings(ctx context.Context, tenant contract.TenantID) (uint32, error) {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil {
		return 0, fmt.Errorf("business finding summary requires context and store")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return 0, err
	}
	query := db.Rebind(s.Dialect, `SELECT COUNT(*) FROM _kora_business_recon_finding WHERE tenant_id = ? AND state IN (?, ?)`)
	var count int64
	if err := s.DB.QueryRowContext(ctx, query, string(tenant), string(BusinessFindingOpen), string(BusinessFindingInvestigating)).Scan(&count); err != nil {
		return 0, err
	}
	if count < 0 || count > int64(^uint32(0)) {
		return 0, fmt.Errorf("business finding count exceeds inspection bounds")
	}
	return uint32(count), nil
}

// CountUnresolvedFindingsInScope keeps operator summaries inside the exact
// tenant or legal-entity scope that was authorized for the request.
func (s *BusinessRunStore) CountUnresolvedFindingsInScope(ctx context.Context, scope contract.BusinessScope) (uint32, error) {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil || scope.Validate() != nil {
		return 0, fmt.Errorf("business finding summary requires context, store, and valid scope")
	}
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		return 0, err
	}
	query := db.Rebind(s.Dialect, `SELECT COUNT(*) FROM _kora_business_recon_finding f JOIN _kora_business_recon_run r ON r.tenant_id=f.tenant_id AND r.run_id=f.run_id WHERE f.tenant_id = ? AND r.scope_json = ? AND f.state IN (?, ?)`)
	var count int64
	if err := s.DB.QueryRowContext(ctx, query, string(scope.TenantID), string(scopeJSON), string(BusinessFindingOpen), string(BusinessFindingInvestigating)).Scan(&count); err != nil {
		return 0, err
	}
	if count < 0 || count > int64(^uint32(0)) {
		return 0, fmt.Errorf("business finding count exceeds inspection bounds")
	}
	return uint32(count), nil
}

// InspectOpenWork implements the module lifecycle inspector using durable,
// owner-attributed reconciliation evidence. Incomplete runs and unresolved
// findings both prevent removal; examples contain opaque run/finding IDs only.
func (s *BusinessRunStore) InspectOpenWork(ctx context.Context, tenant contract.TenantID, module contract.ModuleID, limit uint16) ([]contract.ModuleBlocker, error) {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil || tenant == "" || module == "" {
		return nil, fmt.Errorf("business lifecycle inspection identity is incomplete")
	}
	if limit == 0 || limit > 20 {
		limit = 20
	}
	query := func(q string) string { return db.Rebind(s.Dialect, q) }
	blockers := make([]contract.ModuleBlocker, 0, 2)
	var count uint64
	if err := s.DB.QueryRowContext(ctx, query(`SELECT COUNT(*) FROM _kora_business_recon_finding f JOIN _kora_business_recon_run r ON r.tenant_id=f.tenant_id AND r.run_id=f.run_id WHERE f.tenant_id=? AND r.owner_module_id=? AND f.state IN (?,?)`), string(tenant), string(module), string(BusinessFindingOpen), string(BusinessFindingInvestigating)).Scan(&count); err != nil {
		return nil, fmt.Errorf("count unresolved owner findings: %w", err)
	}
	if count > 0 {
		rows, err := s.DB.QueryContext(ctx, query(`SELECT f.finding_id FROM _kora_business_recon_finding f JOIN _kora_business_recon_run r ON r.tenant_id=f.tenant_id AND r.run_id=f.run_id WHERE f.tenant_id=? AND r.owner_module_id=? AND f.state IN (?,?) ORDER BY f.finding_id LIMIT ?`), string(tenant), string(module), string(BusinessFindingOpen), string(BusinessFindingInvestigating), int(limit))
		if err != nil {
			return nil, err
		}
		examples := make([]string, 0, limit)
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			examples = append(examples, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		blockers = append(blockers, contract.ModuleBlocker{Code: "domain_obligation_open", Count: boundedReconCount(count), Examples: examples})
	}
	count = 0
	if err := s.DB.QueryRowContext(ctx, query(`SELECT COUNT(*) FROM _kora_business_recon_run WHERE tenant_id=? AND owner_module_id=? AND complete=?`), string(tenant), string(module), int64(0)).Scan(&count); err != nil {
		return nil, fmt.Errorf("count incomplete owner reconciliation runs: %w", err)
	}
	if count > 0 {
		rows, err := s.DB.QueryContext(ctx, query(`SELECT run_id FROM _kora_business_recon_run WHERE tenant_id=? AND owner_module_id=? AND complete=? ORDER BY created_at,run_id LIMIT ?`), string(tenant), string(module), int64(0), int(limit))
		if err != nil {
			return nil, err
		}
		examples := make([]string, 0, limit)
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			examples = append(examples, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		blockers = append(blockers, contract.ModuleBlocker{Code: "domain_reconciliation_incomplete", Count: boundedReconCount(count), Examples: examples})
	}
	return blockers, nil
}

// Run begins a new bounded reconciliation and persists an immutable run.
func (s BusinessReconciliationService) Run(ctx context.Context, actor, runID string, rule BusinessReconciliationRule, cutoff time.Time, auth BusinessRunAuthorizer) (BusinessRun, error) {
	return s.runWithCursor(ctx, actor, runID, rule, cutoff, BusinessRunCursor{}, auth)
}

// Continue loads continuation evidence from an authorized durable run. The
// caller supplies only its ID; row buffers and cursors are never accepted from
// untrusted request data.
func (s BusinessReconciliationService) Continue(ctx context.Context, actor, runID, previousRunID string, rule BusinessReconciliationRule, auth BusinessRunAuthorizer, readAuth BusinessRunReader) (BusinessRun, error) {
	if s.Store == nil || previousRunID == "" {
		return BusinessRun{}, fmt.Errorf("business reconciliation continuation requires a prior run")
	}
	previous, err := s.Store.LoadAuthorized(ctx, rule.Scope.TenantID, previousRunID, actor, readAuth)
	if err != nil {
		return BusinessRun{}, err
	}
	if runID == previousRunID || previous.Complete || previous.OwnerModuleID != rule.OwnerModuleID || previous.RuleID != rule.ID || previous.Version != rule.Version || previous.Scope != rule.Scope {
		return BusinessRun{}, fmt.Errorf("prior business reconciliation run cannot continue this rule and scope")
	}
	return s.runWithCursor(ctx, actor, runID, rule, previous.Cutoff, previous.Cursors, auth)
}

// runWithCursor executes only with cursor evidence constructed internally or
// loaded from the authorized durable run store.
func (s BusinessReconciliationService) runWithCursor(ctx context.Context, actor, runID string, rule BusinessReconciliationRule, cutoff time.Time, cursors BusinessRunCursor, auth BusinessRunAuthorizer) (BusinessRun, error) {
	if s.Runs == nil || s.ReadPage == nil || runID == "" || len(runID) > 160 || actor == "" || ctx == nil || rule.Scope.Validate() != nil || auth == nil {
		return BusinessRun{}, fmt.Errorf("business reconciliation service requires identity, repository, query reader, and authorizer")
	}
	if err := auth.CanRunBusinessReconciliation(ctx, rule.Scope.TenantID, rule.Scope, actor); err != nil {
		return BusinessRun{}, fmt.Errorf("business reconciliation run denied: %w", err)
	}
	result, next, watermarks, err := ExecuteBusinessReconciliation(ctx, rule, cutoff, cursors, s.ReadPage)
	if err != nil {
		return BusinessRun{}, err
	}
	run := BusinessRun{TenantID: rule.Scope.TenantID, OwnerModuleID: rule.OwnerModuleID, ID: runID, RuleID: rule.ID, Version: rule.Version, Scope: rule.Scope, Cutoff: cutoff, Complete: result.Complete, Cursors: next, Watermarks: watermarks, Findings: result.Findings, CreatedAt: time.Now().UTC()}
	if err := s.Runs.Save(ctx, run); err != nil {
		return BusinessRun{}, fmt.Errorf("persist business reconciliation run: %w", err)
	}
	return run, nil
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
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS _kora_business_recon_run (tenant_id VARCHAR(140) NOT NULL, run_id VARCHAR(160) NOT NULL, owner_module_id VARCHAR(160) NOT NULL, rule_id VARCHAR(160) NOT NULL, rule_version INTEGER NOT NULL, scope_json %s NOT NULL, cutoff_at VARCHAR(40) NOT NULL, complete SMALLINT NOT NULL, cursors_json %s NOT NULL, watermarks_json %s NOT NULL, created_at VARCHAR(40) NOT NULL, PRIMARY KEY (tenant_id, run_id))`, runPayload, runPayload, runPayload),
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
		var completedKey, completedState string
		err := tx.QueryRowContext(ctx, query(`SELECT idempotency_key, state FROM _kora_business_recon_repair WHERE tenant_id = ? AND run_id = ? AND finding_id = ?`), string(tenant), runID, findingID).Scan(&completedKey, &completedState)
		if err == nil && completedKey == string(businessRepairIdempotencyKey(tenant, runID, findingID)) && completedState == "completed" {
			return rollback(nil)
		}
		if err != nil && err != sql.ErrNoRows {
			return rollback(err)
		}
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
	result, err := tx.ExecContext(ctx, query(`UPDATE _kora_business_recon_finding SET state = ?, evidence = ? WHERE tenant_id = ? AND run_id = ? AND finding_id = ? AND state = ?`), string(BusinessFindingResolved), evidence, string(tenant), runID, findingID, string(BusinessFindingInvestigating))
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		_ = tx.Rollback()
		if err == nil {
			err = fmt.Errorf("business finding changed while owner repair was running")
		}
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err = tx.ExecContext(ctx, query(`UPDATE _kora_business_recon_repair SET state = ?, evidence = ?, updated_at = ? WHERE tenant_id = ? AND run_id = ? AND finding_id = ? AND idempotency_key = ?`), "completed", evidence, now, string(tenant), runID, findingID, string(idempotency))
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		_ = tx.Rollback()
		if err == nil {
			err = fmt.Errorf("business repair intent changed before completion")
		}
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
	if run.ID == "" || len(run.ID) > 160 || run.OwnerModuleID == "" || run.RuleID == "" || len(run.RuleID) > 160 || run.Version == 0 || run.Scope.Validate() != nil || run.Scope.TenantID != run.TenantID || run.Cutoff.IsZero() || len(run.Findings) > 10000 || !validBusinessRunProgress(run.Cursors, run.Watermarks) {
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
	complete := int64(0)
	if run.Complete {
		complete = 1
	}
	cursorsJSON, err := json.Marshal(run.Cursors)
	if err != nil {
		return rollback(err)
	}
	watermarksJSON, err := json.Marshal(run.Watermarks)
	if err != nil {
		return rollback(err)
	}
	if _, err := tx.ExecContext(ctx, query(`INSERT INTO _kora_business_recon_run (tenant_id, run_id, owner_module_id, rule_id, rule_version, scope_json, cutoff_at, complete, cursors_json, watermarks_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`), string(run.TenantID), run.ID, string(run.OwnerModuleID), run.RuleID, run.Version, string(scopeJSON), run.Cutoff.UTC().Format(time.RFC3339Nano), complete, string(cursorsJSON), string(watermarksJSON), run.CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return rollback(err)
	}
	for _, finding := range run.Findings {
		if finding.ID == "" || len(finding.ID) > 80 || finding.Key == "" || len(finding.Evidence) > 4096 {
			return rollback(fmt.Errorf("business reconciliation finding identity is incomplete"))
		}
		if finding.State == "" {
			finding.State = BusinessFindingOpen
		}
		if !validBusinessFindingState(finding.State) || !validBusinessFindingKind(finding.Kind) {
			return rollback(fmt.Errorf("business reconciliation finding state or kind is invalid"))
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
	var scopeJSON, cursorsJSON, watermarksJSON, cutoff, created string
	var complete int64
	err := s.DB.QueryRowContext(ctx, query(`SELECT owner_module_id, rule_id, rule_version, scope_json, cutoff_at, complete, cursors_json, watermarks_json, created_at FROM _kora_business_recon_run WHERE tenant_id = ? AND run_id = ?`), string(tenant), runID).Scan(&run.OwnerModuleID, &run.RuleID, &run.Version, &scopeJSON, &cutoff, &complete, &cursorsJSON, &watermarksJSON, &created)
	if err != nil {
		return BusinessRun{}, err
	}
	run.TenantID, run.ID, run.Complete = tenant, runID, complete != 0
	if err := json.Unmarshal([]byte(scopeJSON), &run.Scope); err != nil {
		return BusinessRun{}, err
	}
	if run.Scope.Validate() != nil || run.Scope.TenantID != tenant {
		return BusinessRun{}, fmt.Errorf("stored business reconciliation scope does not match tenant")
	}
	if err := json.Unmarshal([]byte(cursorsJSON), &run.Cursors); err != nil {
		return BusinessRun{}, fmt.Errorf("decode business reconciliation cursors: %w", err)
	}
	if err := json.Unmarshal([]byte(watermarksJSON), &run.Watermarks); err != nil {
		return BusinessRun{}, fmt.Errorf("decode business reconciliation watermarks: %w", err)
	}
	if !validBusinessRunProgress(run.Cursors, run.Watermarks) {
		return BusinessRun{}, fmt.Errorf("stored business reconciliation progress is invalid")
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
		if finding.ID == "" || finding.Key == "" || len(finding.Evidence) > 4096 || !validBusinessFindingKind(finding.Kind) || !validBusinessFindingState(finding.State) {
			return BusinessRun{}, fmt.Errorf("stored business reconciliation finding is invalid")
		}
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

func boundedReconCount(count uint64) uint32 {
	if count > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(count)
}
