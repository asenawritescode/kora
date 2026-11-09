package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/asenawritescode/kora/internal/site"
)

func TestSiteProbePercentileUsesNearestRank(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := percentile(values, .50); got != 5 {
		t.Fatalf("p50=%v, want 5", got)
	}
	if got := percentile(values, .95); got != 10 {
		t.Fatalf("p95=%v, want 10", got)
	}
}

type directoryPagerStub struct {
	pages   map[string][]site.SiteDescriptor
	cursors []string
	err     error
}

func (s *directoryPagerStub) GetDirectoryDescriptorPageContext(_ context.Context, _ string, after string, limit uint16) ([]site.SiteDescriptor, error) {
	s.cursors = append(s.cursors, after)
	if s.err != nil {
		return nil, s.err
	}
	page := s.pages[after]
	if len(page) > int(limit) {
		return nil, fmt.Errorf("page has %d rows over limit %d", len(page), limit)
	}
	return page, nil
}

type cliAdmissionStub struct {
	acquired, released int
	err                error
}

func (s *cliAdmissionStub) Acquire(context.Context) (func(), error) {
	s.acquired++
	if s.err != nil {
		return nil, s.err
	}
	return func() { s.released++ }, nil
}

func TestWriteDirectoryDescriptorPagesBoundsQueriesAndMemory(t *testing.T) {
	firstPage := make([]site.SiteDescriptor, 500)
	for i := range firstPage {
		firstPage[i].SiteID = fmt.Sprintf("site-%03d", i)
	}
	secondPage := []site.SiteDescriptor{{SiteID: "site-final"}}
	registry := &directoryPagerStub{pages: map[string][]site.SiteDescriptor{"": firstPage, "site-499": secondPage}}
	admission := &cliAdmissionStub{}
	var output bytes.Buffer
	if err := writeDirectoryDescriptorPages(context.Background(), &output, registry, admission); err != nil {
		t.Fatal(err)
	}
	if admission.acquired != 2 || admission.released != 2 {
		t.Fatalf("admission acquired/released = %d/%d, want 2/2", admission.acquired, admission.released)
	}
	if len(registry.cursors) != 2 || registry.cursors[0] != "" || registry.cursors[1] != "site-499" {
		t.Fatalf("page cursors = %q, want [empty site-499]", registry.cursors)
	}
	var descriptors []site.SiteDescriptor
	if err := json.Unmarshal(output.Bytes(), &descriptors); err != nil {
		t.Fatalf("streamed directory JSON is invalid: %v", err)
	}
	if len(descriptors) != 501 || descriptors[0].SiteID != "site-000" || descriptors[500].SiteID != "site-final" {
		t.Fatalf("decoded descriptor count/order = %d, first=%q last=%q", len(descriptors), descriptors[0].SiteID, descriptors[len(descriptors)-1].SiteID)
	}
}

func TestWriteDirectoryDescriptorPagesReleasesAdmissionAfterQueryError(t *testing.T) {
	registry := &directoryPagerStub{err: errors.New("query failed")}
	admission := &cliAdmissionStub{}
	if err := writeDirectoryDescriptorPages(context.Background(), &bytes.Buffer{}, registry, admission); err == nil {
		t.Fatal("expected page query failure")
	}
	if admission.acquired != 1 || admission.released != 1 {
		t.Fatalf("admission acquired/released = %d/%d, want 1/1", admission.acquired, admission.released)
	}
}
