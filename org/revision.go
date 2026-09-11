package org

import (
	"fmt"
	"sync"
	"time"
)

type Change struct {
	Resource string `json:"resource"`
	Before   any    `json:"before,omitempty"`
	After    any    `json:"after,omitempty"`
}

type RevisionState string

const (
	RevisionDraft      RevisionState = "draft"
	RevisionActive     RevisionState = "active"
	RevisionRolledBack RevisionState = "rolled_back"
)

type Revision struct {
	ID          string        `json:"id"`
	ParentID    string        `json:"parent_id,omitempty"`
	Author      string        `json:"author"`
	Reason      string        `json:"reason"`
	Changes     []Change      `json:"changes"`
	State       RevisionState `json:"state"`
	CreatedAt   time.Time     `json:"created_at"`
	ActivatedAt time.Time     `json:"activated_at,omitempty"`
}

type RevisionStore struct {
	mu        sync.RWMutex
	revisions map[string]Revision
	active    string
}

func NewRevisionStore() *RevisionStore { return &RevisionStore{revisions: make(map[string]Revision)} }

func (s *RevisionStore) Create(r Revision) (Revision, error) {
	if r.ID == "" || r.Author == "" || r.Reason == "" {
		return Revision{}, fmt.Errorf("revision id, author, and reason are required")
	}
	if len(r.Changes) == 0 {
		return Revision{}, fmt.Errorf("revision must contain at least one change")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.revisions[r.ID]; ok {
		return Revision{}, fmt.Errorf("revision %q already exists", r.ID)
	}
	if r.ParentID == "" {
		r.ParentID = s.active
	}
	r.State = RevisionDraft
	r.CreatedAt = time.Now().UTC()
	s.revisions[r.ID] = r
	return r, nil
}

func (s *RevisionStore) Preview(id string) (Revision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.revisions[id]
	if !ok {
		return Revision{}, fmt.Errorf("revision %q not found", id)
	}
	return r, nil
}

func (s *RevisionStore) Activate(id string) (Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.revisions[id]
	if !ok {
		return Revision{}, fmt.Errorf("revision %q not found", id)
	}
	if r.State != RevisionDraft {
		return Revision{}, fmt.Errorf("revision %q is not draft", id)
	}
	if s.active != "" {
		old := s.revisions[s.active]
		old.State = RevisionRolledBack
		s.revisions[s.active] = old
	}
	r.State = RevisionActive
	r.ActivatedAt = time.Now().UTC()
	s.revisions[id] = r
	s.active = id
	return r, nil
}

func (s *RevisionStore) Active() (Revision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == "" {
		return Revision{}, fmt.Errorf("no active revision")
	}
	return s.revisions[s.active], nil
}

func (s *RevisionStore) Rollback() (Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == "" {
		return Revision{}, fmt.Errorf("no active revision")
	}
	current := s.revisions[s.active]
	current.State = RevisionRolledBack
	s.revisions[current.ID] = current
	if current.ParentID == "" {
		s.active = ""
		return current, nil
	}
	parent, ok := s.revisions[current.ParentID]
	if !ok {
		return Revision{}, fmt.Errorf("parent revision %q not found", current.ParentID)
	}
	parent.State = RevisionActive
	parent.ActivatedAt = time.Now().UTC()
	s.revisions[parent.ID] = parent
	s.active = parent.ID
	return parent, nil
}
