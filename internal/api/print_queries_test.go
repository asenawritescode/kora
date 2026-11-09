package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/plugin"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/print"
)

type queryFn func(ctx context.Context, input json.RawMessage) ([]map[string]any, error)

func (f queryFn) ExecutePublishedQuery(ctx context.Context, _ contract.ModuleExecution, _ contract.PublishedQuery, input json.RawMessage, _ string, _ uint16) (contract.QueryResult, error) {
	rows, err := f(ctx, input)
	if err != nil {
		return contract.QueryResult{}, err
	}
	data, _ := json.Marshal(rows)
	return contract.QueryResult{Data: data, AsOf: time.Now().UTC(), Freshness: contract.QueryFresh}, nil
}

// printQueryRequest renders a record format whose queries: block runs the
// fixture query pos.core/today_totals through the real binding plan and grant.
func printQueryRequest(t *testing.T, exec plugin.PublishedQueryExecutor, role string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	scope := contract.BusinessScope{TenantID: "print-q", Kind: contract.ScopeKindTenant}
	in, out := contract.SchemaRef{ID: "pos.totals.in", Version: 1}, contract.SchemaRef{ID: "pos.totals.out", Version: 1}
	q := contract.PublishedQuery{OwnerModuleID: "pos.core", Name: "today_totals", Capability: "pos.sales", Operation: "totals", InputSchema: in, OutputSchema: out, ScopePolicy: contract.ScopeTenantShared, AllowedFields: []string{"method", "total"}, PageLimit: 20}
	module := contract.ModuleDescriptor{ID: q.OwnerModuleID, Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: q.Capability, Contract: out, Operations: []contract.OperationSpec{{Name: q.Operation, Input: in, Output: out, Effect: contract.EffectQuery}}}}, Queries: []contract.PublishedQuery{q}}
	snapshot := contract.ModuleSnapshot{TenantID: scope.TenantID, Revision: 1, Modules: []contract.ModuleDescriptor{module}, Installations: []contract.ModuleInstallation{{TenantID: scope.TenantID, ModuleID: module.ID, Version: module.Version, ArtifactDigest: "d", State: string(contract.ModuleActive), Revision: 1}}}
	provider := plugin.ProviderRegistration{TenantID: scope.TenantID, ProviderID: "pos.internal", OwnerModuleID: module.ID, Kind: plugin.ProviderInternal, Offers: module.Provides, Queries: map[string]plugin.PublishedQueryExecutor{q.Name: exec}, ValidateSchema: func(_ contract.SchemaRef, d []byte) error {
		if !json.Valid(d) {
			return context.Canceled
		}
		return nil
	}}
	sel := plugin.ProviderSelection{Key: plugin.BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: q.Capability}, ProviderID: provider.ProviderID}
	services := NewSiteRuntimeServices()
	f, err := print.Load([]byte("name: z\nfor: {record: Till Session}\npaper: a4\nqueries: {totals: {query: pos.core/today_totals, input: {till_session: doc.name}}}\ntext: \"{{ range .queries.totals }}{{ .method }}={{ .total }};{{ end }}\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	services.Replace("print-q", SiteRuntimeService{PrintFormats: map[string]*print.Format{"z": f}, ResolveModuleBindings: func(s contract.BusinessScope) (plugin.BindingPlan, error) {
		return plugin.ResolveModules(s, snapshot.Modules, []plugin.ProviderRegistration{provider}, []plugin.ProviderSelection{sel}, snapshot.Revision)
	}})
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Till Session", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}})
	reg.Permissions.SetPermission(&doctype.Permission{Doctype: "module.pos.core.query.today_totals", Role: "Manager", Read: true})
	h := NewHandler(reg, nil)
	h.RuntimeServices = services
	exe, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "u", PrincipalType: contract.PrincipalHuman, Site: "print-q", Roles: []string{role}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	src := printRecordStub{sitePrintSource{h: h}}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("site_name", "print-q"); c.Set("policy_execution", exe) })
	r.GET("/p/:format_name", NewPrintHandler(src))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/p/z?record=Till+Session&name=T-1", nil))
	return w
}

// printRecordStub serves a fixed record so the test isolates the query path.
type printRecordStub struct{ sitePrintSource }

func (printRecordStub) Record(_ *gin.Context, _, name string) (map[string]any, error) {
	return map[string]any{"name": name}, nil
}

func TestPrintQueriesRunThroughPlanUnderViewerGrant(t *testing.T) {
	var gotInput string
	exec := queryFn(func(_ context.Context, in json.RawMessage) ([]map[string]any, error) {
		gotInput = string(in)
		return []map[string]any{{"method": "Cash", "total": "12.40"}, {"method": "Card", "total": "5.20"}}, nil
	})
	if w := printQueryRequest(t, exec, "Manager"); w.Code != 200 || w.Body.String() != "Cash=12.40;Card=5.20;" || gotInput != `{"till_session":"T-1"}` {
		t.Fatalf("allowed: %d %q input=%s", w.Code, w.Body.String(), gotInput)
	}
	gotInput = ""
	if w := printQueryRequest(t, exec, "Cashier"); w.Code != 403 || gotInput != "" || w.Body.Len() > 200 {
		t.Fatalf("denied: %d %q ran=%q", w.Code, w.Body.String(), gotInput)
	}
}

func (printRecordStub) Tenant(*gin.Context) (map[string]any, print.Options) {
	return map[string]any{"name": "Shop"}, print.Options{Precision: 2}
}
