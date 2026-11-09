package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/site"
	"github.com/spf13/cobra"
)

type assignmentRegistryStub struct {
	descriptor  site.Descriptor
	updated     site.Descriptor
	getErr      error
	setErr      error
	setCalls    int
	setRevision uint64
}

func (r *assignmentRegistryStub) GetDescriptorByIDContext(context.Context, string) (site.Descriptor, error) {
	return r.descriptor, r.getErr
}

func (r *assignmentRegistryStub) SetRuntimeCellIDAtRevisionContext(_ context.Context, _, _ string, expectedRevision uint64) (site.Descriptor, error) {
	r.setCalls++
	r.setRevision = expectedRevision
	return r.updated, r.setErr
}

func TestApplySiteCellAssignmentPreviewsWithoutMutation(t *testing.T) {
	registry := &assignmentRegistryStub{descriptor: site.Descriptor{
		SiteID: "site-1", Aliases: []string{"shop.example.test"}, Status: "active", ConfigRevision: 4,
	}}
	report, err := applySiteCellAssignment(context.Background(), registry, "site-1", "cell-b", false, nil, "cell")
	if err != nil {
		t.Fatal("preview assignment:", err)
	}
	if report.Applied || report.RuntimeCell != "cell-b" || report.ConfigRevision != 4 || registry.setCalls != 0 {
		t.Fatalf("preview report/calls = %+v/%d", report, registry.setCalls)
	}
}

func TestApplySiteCellAssignmentRequiresConfiguredEndpointAndIsIdempotent(t *testing.T) {
	registry := &assignmentRegistryStub{descriptor: site.Descriptor{SiteID: "site-1", RuntimeCellID: "cell-a"}}
	if _, err := applySiteCellAssignment(context.Background(), registry, "site-1", "cell-b", true, nil, "cell"); err == nil || !strings.Contains(err.Error(), "target cell") {
		t.Fatalf("apply without target endpoint error = %v, want cell placement diagnostic", err)
	}
	if registry.setCalls != 0 {
		t.Fatal("assignment mutated despite missing endpoint")
	}
	registry.descriptor.RuntimeCellID = ""
	report, err := applySiteCellAssignment(context.Background(), registry, "site-1", "default", true, nil, "cell")
	if err != nil {
		t.Fatal("blank assignment should already mean default:", err)
	}
	if report.Applied || registry.setCalls != 0 {
		t.Fatalf("default idempotence report/calls = %+v/%d", report, registry.setCalls)
	}
}

func TestApplySiteCellAssignmentWritesRevisionedRegistryOperation(t *testing.T) {
	registry := &assignmentRegistryStub{
		descriptor: site.Descriptor{SiteID: "site-1", RuntimeCellID: "cell-a", ConfigRevision: 4},
		updated:    site.Descriptor{SiteID: "site-1", Aliases: []string{"shop.example.test"}, Status: "active", RuntimeCellID: "cell-b", ConfigRevision: 5},
	}
	report, err := applySiteCellAssignment(context.Background(), registry, "site-1", "cell-b", true, map[string]string{"cell-b": "http://127.0.0.1:8002"}, "cell")
	if err != nil {
		t.Fatal("apply assignment:", err)
	}
	if !report.Applied || report.RuntimeCell != "cell-b" || report.ConfigRevision != 5 || registry.setCalls != 1 || registry.setRevision != 4 {
		t.Fatalf("applied report/calls = %+v/%d", report, registry.setCalls)
	}
}

func TestApplySiteCellAssignmentPropagatesRegistryErrors(t *testing.T) {
	want := errors.New("registry unavailable")
	registry := &assignmentRegistryStub{getErr: want}
	if _, err := applySiteCellAssignment(context.Background(), registry, "site-1", "cell-b", false, nil, "cell"); !errors.Is(err, want) {
		t.Fatalf("read error = %v, want %v", err, want)
	}
	registry.getErr = nil
	registry.descriptor = site.Descriptor{SiteID: "site-1", RuntimeCellID: "cell-a"}
	registry.setErr = want
	if _, err := applySiteCellAssignment(context.Background(), registry, "site-1", "cell-b", true, map[string]string{"cell-b": "http://127.0.0.1:8002"}, "cell"); !errors.Is(err, want) {
		t.Fatalf("write error = %v, want %v", err, want)
	}
}

func TestApplySiteDeploymentAssignmentUsesDeploymentDiagnostic(t *testing.T) {
	registry := &assignmentRegistryStub{descriptor: site.Descriptor{SiteID: "site-1", RuntimeCellID: "deployment-a"}}
	_, err := applySiteCellAssignment(context.Background(), registry, "site-1", "deployment-b", true, nil, "deployment")
	if err == nil || !strings.Contains(err.Error(), "target deployment \"deployment-b\"") {
		t.Fatalf("deployment assignment error = %v, want deployment placement diagnostic", err)
	}
	if registry.setCalls != 0 {
		t.Fatal("assignment mutated despite missing deployment endpoint")
	}
}

func TestWriteSiteDeploymentAssignmentReportUsesDeploymentContract(t *testing.T) {
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	err := writeSiteDeploymentAssignmentReport(cmd, siteCellAssignmentReport{
		SiteID: "site-1", Aliases: []string{"shop.example.test"}, Status: "active",
		PreviousRuntimeCell: "deployment-a", RuntimeCell: "deployment-b",
		ConfigRevision: 9, Applied: true,
	})
	if err != nil {
		t.Fatal("write deployment report:", err)
	}
	var got map[string]any
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal("decode deployment report:", err)
	}
	if got["deployment_id"] != "deployment-b" || got["previous_deployment_id"] != "deployment-a" || got["applied"] != true {
		t.Fatalf("deployment report = %#v", got)
	}
	if _, legacy := got["runtime_cell_id"]; legacy {
		t.Fatalf("deployment report exposes legacy placement key: %#v", got)
	}
}
