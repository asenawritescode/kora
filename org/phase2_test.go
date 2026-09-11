package org

import (
	"github.com/asenawritescode/kora/contract"
	"testing"
)

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

func TestPackageCustomizationsArePatches(t *testing.T) {
	s := NewPackageStore()
	if err := s.Install("inventory", struct{ Name string }{"inventory"}); err != nil {
		t.Fatal(err)
	}
	patch := contract.ConfigurationPatch{Ref: contract.ResourceRef{Namespace: "tenant-a", Name: "inventory-thresholds", Version: 1}, Target: contract.ResourceRef{Namespace: "inventory", Name: "add_stock", Version: 1}, Author: "admin", Reason: "local policy", Operations: []contract.PatchOperation{{Path: "/threshold", Op: "replace", Value: 5}}}
	if err := s.AddPatch("inventory", patch); err != nil {
		t.Fatal(err)
	}
	patches, err := s.Patches("inventory")
	if err != nil || len(patches) != 1 {
		t.Fatalf("patches=%+v err=%v", patches, err)
	}
}
