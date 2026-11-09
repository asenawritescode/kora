package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/print"
)

// zReportSource serves one till's numbers; Cashier-on-own-till and Owner both
// reach it, anyone else is denied by Authorize like the site source does.
type zReportSource struct {
	fakePrint
	formats map[string]*print.Format
	allowed bool
}

func (s *zReportSource) Format(_ *gin.Context, name string) (*print.Format, error) {
	if f := s.formats[name]; f != nil {
		return f, nil
	}
	return nil, ErrPrintNotFound
}

func (s *zReportSource) Record(_ *gin.Context, dt, name string) (map[string]any, error) {
	if !s.allowed {
		return nil, ErrPrintForbidden
	}
	return map[string]any{"name": name, "opening_float": 500.0, "expected_cash": 700.0, "closing_count": 690.0, "variance": -10.0}, nil
}

func (s *zReportSource) Receipt(*gin.Context, string, string) (map[string]any, map[string]any, error) {
	if !s.allowed {
		return nil, nil, ErrPrintForbidden
	}
	return map[string]any{"reference": "TILL-1"}, nil, nil
}

func (s *zReportSource) Query(_ *gin.Context, _, name string, _ map[string]any) ([]map[string]any, error) {
	if name == "refunds_by_method" {
		return []map[string]any{{"method": "Cash", "payment_count": 1, "total": 100.0}}, nil
	}
	return []map[string]any{{"method": "Cash", "payment_count": 2, "total": 300.0}, {"method": "Card", "payment_count": 1, "total": 200.0}}, nil
}

// TestPOSZReportPrintsForBothClosingOperations (#396): the Z report renders
// the same sales, refunds, expected, counted and variance for an execution of
// close_till and of close_session.
func TestPOSZReportPrintsForBothClosingOperations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const closeTill, closeSession = "pos.core~pos.drawer~close_till", "pos.core~pos.drawer~close_session"
	formats := map[string]*print.Format{}
	for name, ref := range map[string]string{"z_report": closeTill, "z_report_close_session": closeSession} {
		body, err := os.ReadFile(filepath.Join("..", "..", "config", "pos", "print_formats", name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		f, err := print.Load(body)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		f.For.Operation = ref // the import resolves the authored name to the published ref
		formats[name] = f
	}
	numbers := []string{"KES 300.00", "KES 200.00", "KES 100.00", "KES 500.00", "KES 700.00", "KES 690.00", "-10.00", "TILL-1"}
	cases := []struct {
		name, format, ref, as string
		allowed               bool
		status                int
	}{
		{"close_till text", "z_report", closeTill, "text", true, 200},
		{"close_till html", "z_report", closeTill, "html", true, 200},
		{"close_session text", "z_report_close_session", closeSession, "text", true, 200},
		{"close_session html", "z_report_close_session", closeSession, "html", true, 200},
		{"close_session not visible to the actor", "z_report_close_session", closeSession, "text", false, 403},
		{"close_session format with the close_till ref", "z_report_close_session", closeTill, "text", true, 400},
		{"close_till format with the close_session ref", "z_report", closeSession, "text", true, 400},
	}
	bodies := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &zReportSource{formats: formats, allowed: tc.allowed}
			r := gin.New()
			r.GET("/p/:format_name", NewPrintHandler(src))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/p/"+tc.format+"?as="+tc.as+"&operation_ref="+tc.ref+"&execution_id=e1", nil))
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if tc.status != 200 {
				return
			}
			for _, want := range numbers {
				if !strings.Contains(w.Body.String(), want) {
					t.Errorf("missing %q in\n%s", want, w.Body.String())
				}
			}
			bodies[tc.name] = w.Body.String()
		})
	}
	for _, as := range []string{"text", "html"} {
		if bodies["close_till "+as] != bodies["close_session "+as] {
			t.Errorf("%s: close_session report differs from close_till\n%s\n---\n%s", as, bodies["close_session "+as], bodies["close_till "+as])
		}
	}
}

// TestPOSZReportFormatsStayInStep: the close_session format is a copy of
// z_report (the print schema has one target per format), so everything but the
// name and target must match.
func TestPOSZReportFormatsStayInStep(t *testing.T) {
	load := func(name string) *print.Format {
		body, err := os.ReadFile(filepath.Join("..", "..", "config", "pos", "print_formats", name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		f, err := print.Load(body)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	base := load("z_report")
	for _, name := range []string{"z_report_close_session"} {
		f := load(name)
		if f.For.Operation != "pos.close_session" || f.For.Doc != base.For.Doc || f.Paper != base.Paper || f.Text != base.Text || f.HTML != base.HTML || len(f.Queries) != len(base.Queries) {
			t.Fatalf("%s differs from z_report beyond name and operation", name)
		}
		for k, q := range base.Queries {
			if f.Queries[k].Query != q.Query {
				t.Errorf("%s query %s = %v, want %v", name, k, f.Queries[k], q)
			}
		}
	}
}
