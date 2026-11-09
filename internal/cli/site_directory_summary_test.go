package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/site"
)

type assignmentSummaryPager struct {
	descriptors   []site.Descriptor
	revision      uint64
	change        bool
	revisionReads int
}

func (p *assignmentSummaryPager) GetDirectoryDescriptorPageContext(_ context.Context, _ string, after string, limit uint16) ([]site.Descriptor, error) {
	start := 0
	for start < len(p.descriptors) && p.descriptors[start].SiteID <= after {
		start++
	}
	end := start + int(limit)
	if end > len(p.descriptors) {
		end = len(p.descriptors)
	}
	return append([]site.Descriptor(nil), p.descriptors[start:end]...), nil
}

func (p *assignmentSummaryPager) CurrentDirectoryRevisionContext(context.Context) (uint64, error) {
	p.revisionReads++
	if p.change && p.revisionReads > 1 {
		return p.revision + 1, nil
	}
	return p.revision, nil
}

type assignmentSummaryAdmission struct{ acquired int }

func (a *assignmentSummaryAdmission) Acquire(context.Context) (func(), error) {
	a.acquired++
	return func() {}, nil
}

func TestSummarizeDirectoryAssignmentsIsAggregateOnlyAndStable(t *testing.T) {
	descriptors := []site.Descriptor{
		{SiteID: "site-a", Aliases: []string{"private-a.example.test"}, Status: "Active"},
		{SiteID: "site-b", Aliases: []string{"private-b.example.test"}, Status: "active", RuntimeCellID: "DEPLOYMENT-East"},
		{SiteID: "site-c", Status: "provisioning", RuntimeCellID: "deployment-east"},
		{SiteID: "site-d", Status: "inactive", RuntimeCellID: "default"},
	}
	pager := &assignmentSummaryPager{descriptors: descriptors, revision: 42}
	admission := &assignmentSummaryAdmission{}
	summary, err := summarizeDirectoryAssignments(context.Background(), pager, admission)
	if err != nil {
		t.Fatal("summarize assignments:", err)
	}
	if summary.Version != 1 || summary.PlacementSource != "runtime_cell_id" || summary.DirectoryRevision != 42 || summary.TotalEntries != 4 {
		t.Fatalf("summary metadata = %+v", summary)
	}
	want := []directoryAssignmentBucket{
		{DeploymentID: "default", Status: "active", Count: 1},
		{DeploymentID: "default", Status: "inactive", Count: 1},
		{DeploymentID: "deployment-east", Status: "active", Count: 1},
		{DeploymentID: "deployment-east", Status: "provisioning", Count: 1},
	}
	if len(summary.Assignments) != len(want) {
		t.Fatalf("assignment buckets = %+v, want %+v", summary.Assignments, want)
	}
	for i := range want {
		if summary.Assignments[i] != want[i] {
			t.Fatalf("assignment bucket %d = %+v, want %+v", i, summary.Assignments[i], want[i])
		}
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal("encode summary:", err)
	}
	for _, private := range []string{"site-a", "site-b", "private-a.example.test", "private-b.example.test"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("aggregate summary leaked %q: %s", private, encoded)
		}
	}
	if admission.acquired != 3 {
		t.Fatalf("admission acquisitions=%d, want two revision reads and one bounded page read", admission.acquired)
	}
}

func TestSummarizeDirectoryAssignmentsRejectsChangingDirectory(t *testing.T) {
	pager := &assignmentSummaryPager{descriptors: []site.Descriptor{{SiteID: "site-a", Status: "active"}}, revision: 9, change: true}
	_, err := summarizeDirectoryAssignments(context.Background(), pager, &assignmentSummaryAdmission{})
	if err == nil || !strings.Contains(err.Error(), "changed during assignment inventory") {
		t.Fatalf("summary error = %v, want changing-directory rejection", err)
	}
}

func TestSummarizeDirectoryAssignmentsRejectsExcessiveBucketCardinality(t *testing.T) {
	descriptors := make([]site.Descriptor, maxDirectoryAssignmentBuckets+1)
	for i := range descriptors {
		descriptors[i] = site.Descriptor{SiteID: fmt.Sprintf("site-%05d", i), Status: "active", RuntimeCellID: fmt.Sprintf("deployment-%05d", i)}
	}
	pager := &assignmentSummaryPager{descriptors: descriptors, revision: 1}
	_, err := summarizeDirectoryAssignments(context.Background(), pager, &assignmentSummaryAdmission{})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("summary error = %v, want bounded-cardinality rejection", err)
	}
}
