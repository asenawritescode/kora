package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/site"
	"github.com/gin-gonic/gin"
)

type managementDirectoryStub struct {
	descriptor      site.SiteDescriptor
	descriptorErr   error
	aliasDescriptor site.SiteDescriptor
	aliasErr        error
	revision        uint64
	revisionErr     error
	revisionFn      func() uint64
}

type managementAdmissionStub struct {
	acquireErr error
	acquired   int
	released   int
}

func (s *managementAdmissionStub) Acquire(context.Context) (func(), error) {
	s.acquired++
	if s.acquireErr != nil {
		return nil, s.acquireErr
	}
	return func() { s.released++ }, nil
}

func (s managementDirectoryStub) GetDescriptorByIDContext(context.Context, string) (site.SiteDescriptor, error) {
	return s.descriptor, s.descriptorErr
}
func (s managementDirectoryStub) ResolveDescriptorByAliasContext(context.Context, string) (site.SiteDescriptor, error) {
	return s.aliasDescriptor, s.aliasErr
}
func (s managementDirectoryStub) CurrentDirectoryRevisionContext(context.Context) (uint64, error) {
	if s.revisionFn != nil {
		return s.revisionFn(), s.revisionErr
	}
	return s.revision, s.revisionErr
}

func managementInspectRouter(t *testing.T, directory ManagementDirectory) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	handler, err := NewManagementInspectHandler(directory, "separate-management-secret", "deployment-a")
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	handler.Register(router)
	return router
}

func TestManagementInspectRequiresDedicatedBearerAndReturnsV2Projection(t *testing.T) {
	directory := managementDirectoryStub{descriptor: site.SiteDescriptor{
		Version: 1, SiteID: "tenant-a", Aliases: []string{"tenant.example.test"}, Status: "active", ConfigRevision: 7, DataPlaneRef: "sha256:abcdef0123456789",
	}, revision: 12}
	router := managementInspectRouter(t, directory)

	unauthorized := httptest.NewRecorder()
	router.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/management/v2/tenants/tenant-a", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/management/v2/tenants/tenant-a", nil)
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("inspect status = %d body=%s", response.Code, response.Body.String())
	}
	if !contains(response.Body.String(), `"data":{"version":2`) {
		t.Fatalf("inspect response omitted the management success envelope: %s", response.Body.String())
	}
	for _, expected := range []string{`"version":2`, `"site_id":"tenant-a"`, `"config_revision":"7"`, `"directory_revision":"12"`, `"deployment_id":"deployment-a"`, `"data_plane_ref":"sha256:abcdef0123456789"`} {
		if !contains(response.Body.String(), expected) {
			t.Errorf("response %s does not contain %s", response.Body.String(), expected)
		}
	}
	for _, forbidden := range []string{"password", "db_host", "db_name"} {
		if contains(response.Body.String(), forbidden) {
			t.Errorf("credential-free response leaked %q: %s", forbidden, response.Body.String())
		}
	}
}

func TestManagementDirectoryResolveReturnsValidatedAssignmentAndRevision(t *testing.T) {
	read := 0
	directory := managementDirectoryStub{
		descriptor: site.SiteDescriptor{
			Version: 1, SiteID: "tenant-a", Aliases: []string{"tenant.example.test"},
			Status: "active", ConfigRevision: 7, RuntimeCellID: "deployment-b",
		},
		aliasDescriptor: site.SiteDescriptor{
			Version: 1, SiteID: "tenant-a", Aliases: []string{"tenant.example.test"},
			Status: "active", ConfigRevision: 7, RuntimeCellID: "deployment-b",
		},
		revisionFn: func() uint64 {
			read++
			return 19
		},
	}
	admission := &managementAdmissionStub{}
	handler, err := NewManagementInspectHandler(directory, "separate-management-secret", "deployment-a", admission)
	if err != nil {
		t.Fatal("construct management handler:", err)
	}
	router := gin.New()
	handler.Register(router)
	request := httptest.NewRequest(http.MethodGet, "/api/management/v2/directory/resolve?alias=TENANT.example.test", nil)
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("directory resolve status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data contract.SiteDirectoryEntryV2 `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal("decode directory entry:", err)
	}
	if err := contract.ValidateSiteDirectoryEntryV2(envelope.Data); err != nil {
		t.Fatalf("returned directory entry is invalid: %v", err)
	}
	if envelope.Data.SiteID != "tenant-a" || envelope.Data.DeploymentID != "deployment-b" || envelope.Data.DirectoryRevision != "19" {
		t.Fatalf("directory entry = %+v", envelope.Data)
	}
	if read != 2 || admission.acquired != 1 || admission.released != 1 {
		t.Fatalf("revision reads/admission acquire/release=%d/%d/%d", read, admission.acquired, admission.released)
	}
}

func TestManagementDirectoryResolveFailsClosedWhenPlacementChangesDuringRead(t *testing.T) {
	read := 0
	directory := managementDirectoryStub{
		descriptor: site.SiteDescriptor{SiteID: "tenant-a", Aliases: []string{"tenant.example.test"}, Status: "active", RuntimeCellID: "deployment-b"},
		revisionFn: func() uint64 {
			read++
			if read == 1 {
				return 19
			}
			return 20
		},
	}
	router := managementInspectRouter(t, directory)
	request := httptest.NewRequest(http.MethodGet, "/api/management/v2/directory/resolve?site_id=tenant-a", nil)
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("changed directory response=%d retry=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
}

func TestManagementDirectoryResolveRequiresExactlyOneSelector(t *testing.T) {
	router := managementInspectRouter(t, managementDirectoryStub{})
	for _, target := range []string{"/api/management/v2/directory/resolve", "/api/management/v2/directory/resolve?site_id=tenant-a&alias=tenant.example.test"} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("Authorization", "Bearer separate-management-secret")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s status=%d body=%s, want 400", target, response.Code, response.Body.String())
		}
	}
}

func TestManagementInspectMapsDirectoryErrorsSafely(t *testing.T) {
	tests := []struct {
		name string
		stub managementDirectoryStub
		want int
	}{
		{name: "missing", stub: managementDirectoryStub{descriptorErr: sql.ErrNoRows}, want: http.StatusNotFound},
		{name: "directory unavailable", stub: managementDirectoryStub{descriptorErr: errors.New("sensitive database detail")}, want: http.StatusServiceUnavailable},
		{name: "revision unavailable", stub: managementDirectoryStub{descriptor: site.SiteDescriptor{SiteID: "tenant-a", Status: "active"}, revisionErr: errors.New("sensitive database detail")}, want: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := managementInspectRouter(t, test.stub)
			request := httptest.NewRequest(http.MethodGet, "/api/management/v2/tenants/tenant-a", nil)
			request.Header.Set("Authorization", "Bearer separate-management-secret")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d body=%s, want %d", response.Code, response.Body.String(), test.want)
			}
			if !contains(response.Body.String(), `"error":{"type"`) {
				t.Fatalf("management error omitted the structured error envelope: %s", response.Body.String())
			}
			if contains(response.Body.String(), "sensitive database detail") {
				t.Fatalf("internal error leaked: %s", response.Body.String())
			}
		})
	}
}

func TestManagementInspectRejectsInvalidTenantID(t *testing.T) {
	router := managementInspectRouter(t, managementDirectoryStub{})
	request := httptest.NewRequest(http.MethodGet, "/api/management/v2/tenants/tenant-a%0A", nil)
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid tenant status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestManagementInspectAdmissionRejectsBeforeDirectoryRead(t *testing.T) {
	admission := &managementAdmissionStub{acquireErr: errors.New("full")}
	handler, err := NewManagementInspectHandler(managementDirectoryStub{}, "separate-management-secret", "deployment-a", admission)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	handler.Register(router)
	request := httptest.NewRequest(http.MethodGet, "/api/management/v2/tenants/tenant-a", nil)
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" || !contains(response.Body.String(), `"RESOURCE_EXHAUSTED"`) {
		t.Fatalf("capacity response status=%d retry-after=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
	if admission.acquired != 1 || admission.released != 0 {
		t.Fatalf("admission acquired=%d released=%d, want 1/0", admission.acquired, admission.released)
	}
}

func TestManagementInspectAdmitsEachDirectoryQuery(t *testing.T) {
	admission := &managementAdmissionStub{}
	handler, err := NewManagementInspectHandler(managementDirectoryStub{
		descriptor: site.SiteDescriptor{SiteID: "tenant-a", Status: "active"}, revision: 1,
	}, "separate-management-secret", "deployment-a", admission)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	handler.Register(router)
	request := httptest.NewRequest(http.MethodGet, "/api/management/v2/tenants/tenant-a", nil)
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("inspect status=%d body=%s", response.Code, response.Body.String())
	}
	if admission.acquired != 2 || admission.released != 2 {
		t.Fatalf("admission acquired=%d released=%d, want 2/2", admission.acquired, admission.released)
	}
}

func TestManagementInspectReturnsTenantLocalConfigRevisionSeparately(t *testing.T) {
	handler, err := NewManagementInspectHandler(managementDirectoryStub{
		descriptor: site.SiteDescriptor{SiteID: "tenant-a", Status: "active", ConfigRevision: 7}, revision: 12,
	}, "separate-management-secret", "deployment-a")
	if err != nil {
		t.Fatal("create inspect handler:", err)
	}
	handler.SetTenantConfigRevisionResolver(func(context.Context, string) (uint64, error) { return 23, nil })
	router := gin.New()
	handler.Register(router)
	request := httptest.NewRequest(http.MethodGet, "/api/management/v2/tenants/tenant-a", nil)
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !contains(response.Body.String(), `"config_revision":"7"`) || !contains(response.Body.String(), `"directory_revision":"12"`) || !contains(response.Body.String(), `"tenant_config_revision":"23"`) {
		t.Fatalf("inspect did not keep revisions distinct: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestManagementInspectResolvesAliasToCredentialFreeDescriptor(t *testing.T) {
	directory := managementDirectoryStub{aliasDescriptor: site.SiteDescriptor{
		Version: 1, SiteID: "tenant-a", Aliases: []string{"tenant.example.test"}, Status: "active", ConfigRevision: 7,
	}, revision: 12}
	router := managementInspectRouter(t, directory)
	request := httptest.NewRequest(http.MethodGet, "/api/management/v2/tenants/resolve?alias=tenant.example.test", nil)
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !contains(response.Body.String(), `"site_id":"tenant-a"`) {
		t.Fatalf("alias inspect status=%d body=%s", response.Code, response.Body.String())
	}
	for _, forbidden := range []string{"password", "db_host", "db_name"} {
		if contains(response.Body.String(), forbidden) {
			t.Fatalf("credential-free alias response leaked %q: %s", forbidden, response.Body.String())
		}
	}
}

func TestManagementPackageValidationUsesEngineValidatorAndBoundsInput(t *testing.T) {
	router := managementInspectRouter(t, managementDirectoryStub{})
	request := httptest.NewRequest(http.MethodPost, "/api/management/v2/packages/validate", strings.NewReader(`{"manifest":{"schema_version":1,"name":"inventory","version":"1.0.0","exports":{}},"files":[{"path":"../escape.yaml","content":"name: Stock","content_type":"doctype"}]}`))
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !contains(response.Body.String(), `"valid":false`) || !contains(response.Body.String(), `"unsafe_path"`) {
		t.Fatalf("validation response status=%d body=%s", response.Code, response.Body.String())
	}

	tooManyFiles := make([]contract.PackFile, 257)
	for i := range tooManyFiles {
		tooManyFiles[i] = contract.PackFile{Path: "doctypes/x.yaml", Content: "x", ContentType: "doctype"}
	}
	body, err := json.Marshal(managementPackValidationRequest{Manifest: contract.PackManifest{SchemaVersion: 1, Name: "inventory", Version: "1.0.0"}, Files: tooManyFiles})
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/management/v2/packages/validate", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("too many package files status=%d body=%s", response.Code, response.Body.String())
	}
	invalidFiles := make([]contract.PackFile, 101)
	for i := range invalidFiles {
		invalidFiles[i] = contract.PackFile{Path: fmt.Sprintf("../unsafe-%d.yaml", i), Content: "x", ContentType: "doctype"}
	}
	body, err = json.Marshal(managementPackValidationRequest{Manifest: contract.PackManifest{SchemaVersion: 1, Name: "inventory", Version: "1.0.0"}, Files: invalidFiles})
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/management/v2/packages/validate", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("too many validation findings status=%d body=%s", response.Code, response.Body.String())
	}
	largePaths := make([]contract.PackFile, 40)
	for i := range largePaths {
		largePaths[i] = contract.PackFile{Path: "../" + strings.Repeat("é", 10000) + fmt.Sprintf("-%d.yaml", i), Content: "x", ContentType: "doctype"}
	}
	body, err = json.Marshal(managementPackValidationRequest{Manifest: contract.PackManifest{SchemaVersion: 1, Name: "inventory", Version: "1.0.0"}, Files: largePaths})
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/management/v2/packages/validate", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer separate-management-secret")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.Len() > 128*1024 || !utf8.Valid(response.Body.Bytes()) {
		t.Fatalf("large validation output status=%d bytes=%d utf8=%v body prefix=%q", response.Code, response.Body.Len(), utf8.Valid(response.Body.Bytes()), response.Body.String()[:min(200, response.Body.Len())])
	}
}

func contains(value, part string) bool { return strings.Contains(value, part) }
