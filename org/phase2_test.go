package org

import "testing"

func TestPackageLifecycleRejectsUnsafeTransition(t *testing.T) {
	s := NewPackageStore()
	if err := s.Install("inventory", struct{ Name string }{"inventory"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition("inventory", PackageActive); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition("inventory", PackageInstalled); err == nil {
		t.Fatal("active package moved backwards without rollback")
	}
	if err := s.Transition("inventory", PackageRemoved); err != nil {
		t.Fatal(err)
	}
}
