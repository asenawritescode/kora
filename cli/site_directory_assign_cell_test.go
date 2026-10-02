package cli

import (
	"errors"
	"testing"

	"github.com/asenawritescode/kora/site"
)

type assignmentRegistryStub struct {
	descriptor site.SiteDescriptor
	updated    site.SiteDescriptor
	getErr     error
	setErr     error
	setCalls   int
}

func (r *assignmentRegistryStub) GetDescriptorByID(string) (site.SiteDescriptor, error) {
	return r.descriptor, r.getErr
}

func (r *assignmentRegistryStub) SetRuntimeCellID(_, _ string) (site.SiteDescriptor, error) {
	r.setCalls++
	return r.updated, r.setErr
}

func TestApplySiteCellAssignmentPreviewsWithoutMutation(t *testing.T) {
	registry := &assignmentRegistryStub{descriptor: site.SiteDescriptor{
		SiteID: "site-1", Aliases: []string{"shop.example.test"}, Status: "active", ConfigRevision: 4,
	}}
	report, err := applySiteCellAssignment(registry, "site-1", "cell-b", false, nil)
	if err != nil {
		t.Fatal("preview assignment:", err)
	}
	if report.Applied || report.RuntimeCell != "cell-b" || report.ConfigRevision != 4 || registry.setCalls != 0 {
		t.Fatalf("preview report/calls = %+v/%d", report, registry.setCalls)
	}
}

func TestApplySiteCellAssignmentRequiresConfiguredEndpointAndIsIdempotent(t *testing.T) {
	registry := &assignmentRegistryStub{descriptor: site.SiteDescriptor{SiteID: "site-1", RuntimeCellID: "cell-a"}}
	if _, err := applySiteCellAssignment(registry, "site-1", "cell-b", true, nil); err == nil {
		t.Fatal("apply without target endpoint succeeded")
	}
	if registry.setCalls != 0 {
		t.Fatal("assignment mutated despite missing endpoint")
	}
	registry.descriptor.RuntimeCellID = ""
	report, err := applySiteCellAssignment(registry, "site-1", "default", true, nil)
	if err != nil {
		t.Fatal("blank assignment should already mean default:", err)
	}
	if report.Applied || registry.setCalls != 0 {
		t.Fatalf("default idempotence report/calls = %+v/%d", report, registry.setCalls)
	}
}

func TestApplySiteCellAssignmentWritesRevisionedRegistryOperation(t *testing.T) {
	registry := &assignmentRegistryStub{
		descriptor: site.SiteDescriptor{SiteID: "site-1", RuntimeCellID: "cell-a", ConfigRevision: 4},
		updated:    site.SiteDescriptor{SiteID: "site-1", Aliases: []string{"shop.example.test"}, Status: "active", RuntimeCellID: "cell-b", ConfigRevision: 5},
	}
	report, err := applySiteCellAssignment(registry, "site-1", "cell-b", true, map[string]string{"cell-b": "http://127.0.0.1:8002"})
	if err != nil {
		t.Fatal("apply assignment:", err)
	}
	if !report.Applied || report.RuntimeCell != "cell-b" || report.ConfigRevision != 5 || registry.setCalls != 1 {
		t.Fatalf("applied report/calls = %+v/%d", report, registry.setCalls)
	}
}

func TestApplySiteCellAssignmentPropagatesRegistryErrors(t *testing.T) {
	want := errors.New("registry unavailable")
	registry := &assignmentRegistryStub{getErr: want}
	if _, err := applySiteCellAssignment(registry, "site-1", "cell-b", false, nil); !errors.Is(err, want) {
		t.Fatalf("read error = %v, want %v", err, want)
	}
	registry.getErr = nil
	registry.descriptor = site.SiteDescriptor{SiteID: "site-1", RuntimeCellID: "cell-a"}
	registry.setErr = want
	if _, err := applySiteCellAssignment(registry, "site-1", "cell-b", true, map[string]string{"cell-b": "http://127.0.0.1:8002"}); !errors.Is(err, want) {
		t.Fatalf("write error = %v, want %v", err, want)
	}
}
