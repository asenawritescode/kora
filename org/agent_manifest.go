package org

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type AgentManifest struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Roles and direct permissions are the canonical agent authorization policy.
	// The capability fields below remain readable for migration only.
	Roles               []string          `json:"roles,omitempty"`
	DirectPermissions   []AgentPermission `json:"direct_permissions,omitempty"`
	DeniedPermissions   []AgentPermission `json:"denied_permissions,omitempty"`
	ApprovalRules       []AgentPermission `json:"approval_rules,omitempty"`
	GrantedCapabilities []string          `json:"granted_capabilities"`
	ProhibitedActions   []string          `json:"prohibited_actions,omitempty"`
	ApprovalGates       []string          `json:"approval_gates,omitempty"`
	UpdatedAt           time.Time         `json:"updated_at"`
}

// AgentPermission assigns one DocType operation to an agent policy. It uses
// the same operation vocabulary as doctype.Permission and is deliberately
// independent of the removed capability authorization model.
type AgentPermission struct {
	Doctype   string `json:"doctype"`
	Operation string `json:"operation"`
	IfOwner   bool   `json:"if_owner,omitempty"`
	Scope     string `json:"scope,omitempty"`
}
type AgentRun struct {
	ID        string `json:"id"`
	AgentID   string `json:"agent_id"`
	Status    string `json:"status"`
	Doctype   string `json:"doctype,omitempty"`
	Operation string `json:"operation,omitempty"`
	// Capability is read-only compatibility data for old run records.
	Capability       string    `json:"capability,omitempty"`
	ApprovalRequired bool      `json:"approval_required"`
	HandoffTask      string    `json:"handoff_task,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}
type AgentStore struct {
	mu        sync.RWMutex
	manifests map[string]AgentManifest
	runs      map[string][]AgentRun
	db        *sql.DB
}

func NewAgentStore() *AgentStore {
	return &AgentStore{manifests: map[string]AgentManifest{}, runs: map[string][]AgentRun{}}
}

func NewSQLAgentStore(db *sql.DB) (*AgentStore, error) {
	s := &AgentStore{manifests: map[string]AgentManifest{}, runs: map[string][]AgentRun{}, db: db}
	rows, err := db.Query(`SELECT manifest_json FROM _kora_agent_manifest`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var m AgentManifest
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			return nil, err
		}
		s.manifests[m.ID] = m
	}
	return s, rows.Err()
}
func (s *AgentStore) SaveManifest(m AgentManifest) (AgentManifest, error) {
	if m.ID == "" || m.Name == "" {
		return AgentManifest{}, fmt.Errorf("agent manifest requires id and name")
	}
	m.UpdatedAt = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.manifests[m.ID] = m
	if s.db != nil {
		data, _ := json.Marshal(m)
		result, err := s.db.Exec(`UPDATE _kora_agent_manifest SET manifest_json = ?, updated_at = ? WHERE id = ?`, string(data), m.UpdatedAt, m.ID)
		if err != nil {
			return AgentManifest{}, err
		}
		affected, _ := result.RowsAffected()
		if affected == 0 {
			if _, err := s.db.Exec(`INSERT INTO _kora_agent_manifest (id, manifest_json, updated_at) VALUES (?, ?, ?)`, m.ID, string(data), m.UpdatedAt); err != nil {
				return AgentManifest{}, err
			}
		}
	}
	return m, nil
}
func (s *AgentStore) GetManifest(id string) (AgentManifest, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.manifests[id]
	return m, ok
}
func (s *AgentStore) RecordRun(r AgentRun) (AgentRun, error) {
	if r.ID == "" || r.AgentID == "" || (r.Capability == "" && (r.Doctype == "" || r.Operation == "")) {
		return AgentRun{}, fmt.Errorf("agent run requires id, agent, and doctype operation")
	}
	if r.Status == "" {
		r.Status = "requested"
	}
	r.CreatedAt = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[r.AgentID] = append(s.runs[r.AgentID], r)
	if s.db != nil {
		data, _ := json.Marshal(r)
		if _, err := s.db.Exec(`INSERT INTO _kora_agent_run (id, agent_id, run_json, created_at) VALUES (?, ?, ?, ?)`, r.ID, r.AgentID, string(data), r.CreatedAt); err != nil {
			return AgentRun{}, err
		}
	}
	return r, nil
}
func (s *AgentStore) Runs(id string) []AgentRun {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db != nil {
		rows, err := s.db.Query(`SELECT run_json FROM _kora_agent_run WHERE agent_id = ? ORDER BY created_at DESC`, id)
		if err == nil {
			defer rows.Close()
			result := []AgentRun{}
			for rows.Next() {
				var data string
				var run AgentRun
				if rows.Scan(&data) == nil && json.Unmarshal([]byte(data), &run) == nil {
					result = append(result, run)
				}
			}
			return result
		}
	}
	return append([]AgentRun(nil), s.runs[id]...)
}
