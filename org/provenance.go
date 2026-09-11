package org

import (
	"fmt"
	"sync"

	"github.com/asenawritescode/kora/contract"
)

// ProvenanceStore is the runtime's inspectable explanation index. Records are
// append-only and keyed by event so a material result can be traced without
// replaying the entire event stream.
type ProvenanceStore struct {
	mu      sync.RWMutex
	records map[string]contract.ProvenanceRecord
	byEvent map[string][]string
}

func NewProvenanceStore() *ProvenanceStore {
	return &ProvenanceStore{records: map[string]contract.ProvenanceRecord{}, byEvent: map[string][]string{}}
}

func (s *ProvenanceStore) Append(record contract.ProvenanceRecord) error {
	if record.ID == "" || record.EventID == "" || record.Resource.Name == "" || record.Actor.PrincipalID == "" {
		return fmt.Errorf("provenance requires id, event, resource, and actor")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.records[record.ID]; exists {
		return fmt.Errorf("provenance %q already exists", record.ID)
	}
	s.records[record.ID] = record
	s.byEvent[record.EventID] = append(s.byEvent[record.EventID], record.ID)
	return nil
}

func (s *ProvenanceStore) Get(id string) (contract.ProvenanceRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.records[id]
	return r, ok
}

func (s *ProvenanceStore) ForEvent(eventID string) []contract.ProvenanceRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.byEvent[eventID]
	out := make([]contract.ProvenanceRecord, 0, len(ids))
	for _, id := range ids {
		if record, ok := s.records[id]; ok {
			out = append(out, record)
		}
	}
	return out
}
