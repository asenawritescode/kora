package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/metrics"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// TestDrillMetricNamesOneMetricOrOneMeasure checks that a drill names exactly one saved metric or one
// unsaved measure, and that the measure is added to the catalog under the name the drill reads.
func TestDrillMetricNamesOneMetricOrOneMeasure(t *testing.T) {
	sale := &doctype.DocType{Name: "Sale", Facts: &doctype.FactModel{Time: "creation"}}
	cat := metrics.NewCatalog([]metrics.Metric{{Name: "sales", Label: "Sales", Type: metrics.KindSimple, Model: "Sale",
		Measure: &metrics.Measure{Agg: "count"}}}, []*doctype.DocType{sale})
	measure := &contract.MetricAdHocMeasure{RecordType: "Sale", Agg: "count"}
	tests := []struct {
		name      string
		req       contract.MetricDrillRequest
		want      string
		wantAdHoc bool
		wantErr   bool
	}{
		{name: "a saved metric", req: contract.MetricDrillRequest{Metric: "sales"}, want: "sales"},
		{name: "an unsaved measure", req: contract.MetricDrillRequest{Measure: measure}, wantAdHoc: true},
		{name: "neither names a metric", req: contract.MetricDrillRequest{}, wantErr: true},
		{name: "both name a metric", req: contract.MetricDrillRequest{Metric: "sales", Measure: measure}, wantErr: true},
		{name: "an unknown record type", req: contract.MetricDrillRequest{Measure: &contract.MetricAdHocMeasure{RecordType: "Nope", Agg: "count"}}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, name, err := drillMetric(cat, tc.req)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if tc.wantAdHoc {
				if name == "" || name == "sales" {
					t.Fatalf("name = %q, want the unsaved measure's own name", name)
				}
				return
			}
			if name != tc.want {
				t.Fatalf("name = %q, want %q", name, tc.want)
			}
		})
	}
}

// TestDrillErrorStatus checks how a failed drill is answered. The refusals come from the engine before any
// SQL runs: an unknown metric, a bad cursor, and a path through a record type the caller may not join.
func TestDrillErrorStatus(t *testing.T) {
	sale := &doctype.DocType{Name: "Sale", Fields: []doctype.Field{{Fieldname: "supplier", Fieldtype: "Link", Options: "Supplier"}},
		Facts: &doctype.FactModel{Time: "creation", Dimensions: []doctype.FactDimension{{Field: "supplier"}}}}
	supplier := &doctype.DocType{Name: "Supplier", Fields: []doctype.Field{{Fieldname: "town", Fieldtype: "Data"}}}
	cat := metrics.NewCatalog([]metrics.Metric{{Name: "sales", Label: "Sales", Type: metrics.KindSimple, Model: "Sale",
		Measure: &metrics.Measure{Agg: "count"}}}, []*doctype.DocType{sale, supplier})
	day := tenanttime.Date{Year: 2026, Month: 11, Day: 20}
	clock := tenanttime.Clock{Location: time.UTC}
	tests := []struct {
		name  string
		req   metrics.DrillRequest
		scope metrics.Scope
		why   string
		want  int
	}{
		{"an unknown metric is refused", metrics.DrillRequest{Metric: "nope", From: day, To: day}, metrics.Scope{}, "", http.StatusUnprocessableEntity},
		{"a bad cursor is a bad request", metrics.DrillRequest{Metric: "sales", From: day, To: day}, metrics.Scope{}, "bogus", http.StatusBadRequest},
		{"a path the caller may not join is forbidden", metrics.DrillRequest{Metric: "sales", From: day, To: day,
			Group: map[string]string{"supplier.town": "Nairobi"}}, metrics.Scope{}, "", http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			_, err := metrics.Engine{Catalog: cat}.DrillRecords(context.Background(), tc.req, tc.scope, clock, tc.why)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			drillError(c, err)
			if rec.Code != tc.want {
				t.Fatalf("err %v: status = %d, want %d (%s)", err, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	t.Run("an engine failure is internal", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		drillError(c, errors.New("disk on fire"))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}
