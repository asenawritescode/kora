package org

import (
	"fmt"
	"sync"
)

type PackageState string

const (
	PackageDiscovered PackageState = "discovered"
	PackageInstalled  PackageState = "installed"
	PackageActive     PackageState = "active"
	PackageDisabled   PackageState = "disabled"
	PackageRemoved    PackageState = "removed"
	PackageRolledBack PackageState = "rolled_back"
)

type PackageRecord struct {
	Manifest any          `json:"manifest"`
	State    PackageState `json:"state"`
}

type PackageStore struct {
	mu    sync.RWMutex
	items map[string]PackageRecord
}

func NewPackageStore() *PackageStore { return &PackageStore{items: map[string]PackageRecord{}} }

func (s *PackageStore) Install(id string, manifest any) error {
	if id == "" || manifest == nil {
		return fmt.Errorf("package id and manifest are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.items[id]; ok && current.State != PackageRemoved && current.State != PackageRolledBack {
		return fmt.Errorf("package %q is already installed", id)
	}
	s.items[id] = PackageRecord{Manifest: manifest, State: PackageInstalled}
	return nil
}

func (s *PackageStore) Transition(id string, next PackageState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.items[id]
	if !ok {
		return fmt.Errorf("package %q not found", id)
	}
	if !validPackageTransition(current.State, next) {
		return fmt.Errorf("invalid package transition %s -> %s", current.State, next)
	}
	current.State = next
	s.items[id] = current
	return nil
}

func (s *PackageStore) Get(id string) (PackageRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.items[id]
	return r, ok
}

func validPackageTransition(from, to PackageState) bool {
	switch from {
	case PackageInstalled:
		return to == PackageActive || to == PackageDisabled || to == PackageRolledBack
	case PackageActive:
		return to == PackageDisabled || to == PackageRemoved || to == PackageRolledBack
	case PackageDisabled:
		return to == PackageActive || to == PackageRemoved || to == PackageRolledBack
	default:
		return false
	}
}
