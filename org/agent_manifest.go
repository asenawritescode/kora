package org

import (
	"fmt"
	"sync"
	"time"
)

type AgentManifest struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	GrantedCapabilities []string  `json:"granted_capabilities"`
	ProhibitedActions   []string  `json:"prohibited_actions,omitempty"`
	ApprovalGates       []string  `json:"approval_gates,omitempty"`
	UpdatedAt           time.Time `json:"updated_at"`
}
type AgentRun struct {
	ID               string    `json:"id"`
	AgentID          string    `json:"agent_id"`
	Status           string    `json:"status"`
	Capability       string    `json:"capability"`
	ApprovalRequired bool      `json:"approval_required"`
	HandoffTask      string    `json:"handoff_task,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}
type AgentStore struct {
	mu        sync.RWMutex
	manifests map[string]AgentManifest
	runs      map[string][]AgentRun
}

func NewAgentStore() *AgentStore {
	return &AgentStore{manifests: map[string]AgentManifest{}, runs: map[string][]AgentRun{}}
}
func (s *AgentStore) SaveManifest(m AgentManifest) (AgentManifest, error) {
	if m.ID == "" || m.Name == "" {
		return AgentManifest{}, fmt.Errorf("agent manifest requires id and name")
	}
	m.UpdatedAt = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.manifests[m.ID] = m
	return m, nil
}
func (s *AgentStore) GetManifest(id string) (AgentManifest, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.manifests[id]
	return m, ok
}
func (s *AgentStore) RecordRun(r AgentRun) (AgentRun, error) {
	if r.ID == "" || r.AgentID == "" || r.Capability == "" {
		return AgentRun{}, fmt.Errorf("agent run requires id, agent, and capability")
	}
	if r.Status == "" {
		r.Status = "requested"
	}
	r.CreatedAt = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[r.AgentID] = append(s.runs[r.AgentID], r)
	return r, nil
}
func (s *AgentStore) Runs(id string) []AgentRun {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]AgentRun(nil), s.runs[id]...)
}
