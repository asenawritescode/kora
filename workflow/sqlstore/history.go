package sqlstore

import (
	"context"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/workflow"
)

func (s *InstanceStore) ListHistory(ctx context.Context, tenant contract.TenantID, id contract.InstanceID, limit uint16) ([]workflow.HistoryEntry, error) {
	if s == nil || s.database == nil || s.dialect == nil || ctx == nil || limit == 0 || limit > 200 {
		return nil, fmt.Errorf("workflow history query is invalid")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return nil, err
	}
	if _, err := contract.ParseInstanceID(string(id)); err != nil {
		return nil, err
	}
	rows, err := s.database.QueryContext(ctx, db.Rebind(s.dialect, `SELECT sequence,step_id,kind,status,detail,created_at FROM _kora_workflow_history WHERE tenant_id=? AND instance_id=? ORDER BY sequence DESC LIMIT ?`), string(tenant), string(id), limit)
	if err != nil {
		return nil, fmt.Errorf("read workflow history: %w", err)
	}
	defer rows.Close()
	entries := make([]workflow.HistoryEntry, 0)
	for rows.Next() {
		var entry workflow.HistoryEntry
		var created any
		if err := rows.Scan(&entry.Sequence, &entry.StepID, &entry.Kind, &entry.Status, &entry.Detail, &created); err != nil {
			return nil, fmt.Errorf("scan workflow history: %w", err)
		}
		parsed, err := parseDBTime(created)
		if err != nil {
			return nil, fmt.Errorf("parse workflow history time: %w", err)
		}
		entry.CreatedAt = parsed
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow history: %w", err)
	}
	for left, right := 0, len(entries)-1; left < right; left, right = left+1, right-1 {
		entries[left], entries[right] = entries[right], entries[left]
	}
	return entries, nil
}

func (s *InstanceStore) ListRecentInstances(ctx context.Context, tenant contract.TenantID, limit uint16) ([]contract.WorkflowInstance, error) {
	if s == nil || s.database == nil || s.dialect == nil || ctx == nil || limit == 0 || limit > 100 {
		return nil, fmt.Errorf("workflow run query is invalid")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return nil, err
	}
	rows, err := s.database.QueryContext(ctx, db.Rebind(s.dialect, `SELECT id FROM _kora_workflow_instance WHERE tenant_id=? ORDER BY updated_at DESC,id DESC LIMIT ?`), string(tenant), limit)
	if err != nil {
		return nil, fmt.Errorf("list workflow runs: %w", err)
	}
	defer rows.Close()
	ids := make([]contract.InstanceID, 0, limit)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		id, err := contract.ParseInstanceID(raw)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	instances := make([]contract.WorkflowInstance, 0, len(ids))
	for _, id := range ids {
		instance, err := s.GetInstance(ctx, tenant, id)
		if err != nil {
			return nil, err
		}
		instances = append(instances, instance)
	}
	return instances, nil
}
