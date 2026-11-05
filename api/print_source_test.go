package api

import (
	"context"
	"encoding/json"
	"time"

	"errors"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/print"
)

func TestSitePrintSource(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f, err := print.Load([]byte("name: a\nfor: {record: Sale}\npaper: a4\ntext: x\n"))
	if err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	cases := []struct {
		name string
		run  func(sitePrintSource, *gin.Context) error
		want error
	}{
		{"known format", func(s sitePrintSource, c *gin.Context) error { _, err := s.Format(c, "a"); return err }, nil},
		{"unknown format", func(s sitePrintSource, c *gin.Context) error { _, err := s.Format(c, "zz"); return err }, ErrPrintNotFound},
		{"unknown doctype", func(s sitePrintSource, c *gin.Context) error { _, err := s.Record(c, "Nope", "x"); return err }, ErrPrintNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Set("site_name", "s")
			c.Set("site_registry", reg)
			c.Set("site_runtime_services", SiteRuntimeService{PrintFormats: map[string]*print.Format{"a": f}})
			if err := tc.run(sitePrintSource{h: NewHandler(reg, nil)}, c); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}

func TestSitePrintSourceRecordDeniedWithoutExecution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Sale"})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("site_registry", reg)
	_, err := sitePrintSource{h: NewHandler(reg, nil)}.Record(c, "Sale", "S-1")
	if !errors.Is(err, ErrPrintForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestSitePrintSourceReceipt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	scope := contract.BusinessScope{TenantID: "print-rcpt", Kind: contract.ScopeKindTenant}
	in, out := contract.SchemaRef{ID: "s.in", Version: 1}, contract.SchemaRef{ID: "s.out", Version: 1}
	pub := contract.PublishedOperation{Name: "issue", Capability: "sales.invoice", Operation: "issue", ScopePolicy: contract.ScopeTenantShared, AllowedCallers: []contract.ModuleID{"sales.checkout"}}
	owner := contract.ModuleDescriptor{ID: "sales.invoices", Version: "1.0.0", PublishedOperations: []contract.PublishedOperation{pub}, Provides: []contract.CapabilityOffer{{Name: pub.Capability, Contract: out, Operations: []contract.OperationSpec{{Name: pub.Operation, Input: in, Output: out, Effect: contract.EffectLocal}}}}}
	snap := contract.ModuleSnapshot{TenantID: scope.TenantID, Revision: 1, Modules: []contract.ModuleDescriptor{owner}}
	reg := doctype.NewRegistry()
	reg.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.invoices.operation.issue", Role: "Operator", Write: true})
	ref := contract.PublishedOperationRef(owner.ID, pub.Capability, pub.Operation)
	id := contract.OperationID(contract.NewOperationID())
	var rcpt contract.OperationReceipt
	svc := NewSiteRuntimeServices()
	svc.Replace(string(scope.TenantID), SiteRuntimeService{
		LoadModuleSnapshot: func() (contract.ModuleSnapshot, error) { return snap, nil },
		LookupPublishedOperationByID: func(context.Context, policy.Execution, contract.BusinessScope, contract.OperationID) (contract.OperationReceipt, bool, error) {
			return rcpt, true, nil
		},
	})
	h := NewHandler(reg, nil)
	h.RuntimeServices = svc
	cases := []struct {
		name  string
		roles []string
		ref   string
		out   string
		want  error
		// wantRef is the .reference templates and the print endpoint read.
		wantRef string
	}{
		{"flat output", []string{"Operator"}, ref, `{"total":42,"reference":"S-1"}`, nil, "S-1"},
		{"command envelope", []string{"Operator"}, ref, `{"command":"pos.record_sale","output":{"reference":"SALE-2026-00003","total":"340"},"steps":[{"Record":"Sale","Name":"SALE-2026-00003"}]}`, nil, "SALE-2026-00003"},
		{"command envelope without declared output", []string{"Operator"}, ref, `{"command":"pos.noop","steps":[]}`, nil, ""},
		{"output key without command marker", []string{"Operator"}, ref, `{"output":{"reference":"X"},"reference":"S-2"}`, nil, "S-2"},
		{"no grant", nil, ref, `{"reference":"S-1"}`, ErrPrintForbidden, ""},
		{"wrong ref", []string{"Operator"}, "other~x~y", `{"reference":"S-1"}`, ErrPrintNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rcpt = contract.OperationReceipt{ExecutionID: id, OperationRef: ref, BusinessScope: scope, Status: contract.ReceiptCompleted, OutputSchema: &out, Output: json.RawMessage(tc.out), Revision: "1"}
			ex, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "u", PrincipalType: contract.PrincipalHuman, Site: string(scope.TenantID), Roles: tc.roles, AuthenticatedAt: time.Now().UTC()})
			if err != nil {
				t.Fatal(err)
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("GET", "/", nil)
			c.Set("site_name", string(scope.TenantID))
			c.Set("site_registry", reg)
			c.Set("policy_execution", ex)
			r, _, err := sitePrintSource{h: h}.Receipt(c, tc.ref, string(id))
			if !errors.Is(err, tc.want) || (tc.want == nil && (r["reference"] != tc.wantRef && !(tc.wantRef == "" && r["reference"] == nil))) {
				t.Fatalf("got %v %v", r, err)
			}
		})
	}
}
