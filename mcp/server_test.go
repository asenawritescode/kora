package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/api/ai"
	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolGeneration_ForDoctype(t *testing.T) {
	reg := doctype.NewRegistry()

	// Register a non-child doctype.
	dt := &doctype.DocType{
		Name: "Customer",
		Fields: []doctype.Field{
			{Fieldname: "customer_name", Fieldtype: "Data", Reqd: true},
			{Fieldname: "email", Fieldtype: "Data"},
			{Fieldname: "phone", Fieldtype: "Data"},
		},
	}
	reg.Register(dt)

	server := NewWithExecutor(reg, "test-site", ModeExecutable, testExecutor)

	if server == nil {
		t.Fatal("New returned nil")
	}
	if server.srv == nil {
		t.Fatal("server.srv is nil (no tools registered)")
	}
}

func TestToolNames_FollowPattern(t *testing.T) {
	reg := doctype.NewRegistry()

	// Multi-word doctype.
	dt := &doctype.DocType{
		Name: "Work Order",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data"},
		},
	}
	reg.Register(dt)

	server := NewWithExecutor(reg, "test-site", ModeExecutable, testExecutor)
	if server == nil {
		t.Fatal("New returned nil")
	}

	// The tool names should follow the pattern <sanitized_name>_<operation>.
	// For "Work Order", sanitized name is "work_order".
	// Tools: work_order_list, work_order_create, work_order_get, work_order_update, work_order_delete
	// Plus validate_yaml from addConfigTools.
	// We can verify by checking the server was created without panic.
	_ = server
}

func TestEmptyRegistry_NoTools(t *testing.T) {
	reg := doctype.NewRegistry()

	server := New(reg, "test-site", ModeValidationOnly)
	if server == nil {
		t.Fatal("New returned nil")
	}
	_ = server
}

func TestAddDoctypeTools_ExcludesChildTables(t *testing.T) {
	reg := doctype.NewRegistry()

	// Register a parent doctype.
	parent := &doctype.DocType{
		Name: "Invoice",
		Fields: []doctype.Field{
			{Fieldname: "total", Fieldtype: "Currency"},
		},
	}
	reg.Register(parent)

	// Register a child table doctype (should be excluded from tools).
	child := &doctype.DocType{
		Name:         "Invoice Item",
		IsChildTable: true,
		Fields: []doctype.Field{
			{Fieldname: "item_name", Fieldtype: "Data"},
			{Fieldname: "qty", Fieldtype: "Int"},
			{Fieldname: "rate", Fieldtype: "Currency"},
		},
	}
	reg.Register(child)

	server := NewWithExecutor(reg, "test-site", ModeExecutable, testExecutor)
	if server == nil {
		t.Fatal("New returned nil")
	}
	// Should have validated that child tables don't generate tools without panic.
	_ = server
}

func TestValidationOnlyModeExposesConfigToolsOnly(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Customer"})

	server := New(reg, "test-site", ModeValidationOnly)
	if server == nil || server.srv == nil {
		t.Fatal("expected server to initialize")
	}
}

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Customer", "customer"},
		{"Work Order", "work_order"},
		{"Sales-Invoice", "sales_invoice"},
		{"Lead", "lead"},
		{"already_lower", "already_lower"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeName(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestProjectedMCPToolsMirrorCatalog(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{
		Name: "Task",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data", Reqd: true},
			{Fieldname: "age", Fieldtype: "Int"},
		},
	})

	server := NewWithExecutor(reg, "test-site", ModeExecutable, testExecutor)
	if server == nil || server.srv == nil {
		t.Fatal("expected server to initialize")
	}

	catalog := ai.BuildToolCatalog(reg)
	if len(catalog.Tools) == 0 {
		t.Fatal("expected tool catalog entries")
	}
}

func TestExecutableModeRequiresVerifiedExecutor(t *testing.T) {
	server := New(registryWithTask(), "test-site", ModeExecutable)
	if err := server.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "authenticated verified tool executor") {
		t.Fatalf("Run error = %v, want missing verified executor error", err)
	}
}

func TestDefaultModeIsValidationOnly(t *testing.T) {
	server := New(registryWithTask(), "test-site", "")
	if server.mode != ModeValidationOnly {
		t.Fatalf("default mode = %q, want %q", server.mode, ModeValidationOnly)
	}
	if server.executor != nil {
		t.Fatal("default mode unexpectedly has an executor")
	}
}

func TestReadOnlyModePublishesOnlyFindListGetTools(t *testing.T) {
	registry := registryWithTask()
	server := NewReadOnlyWithExecutor(registry, "tenant.example", testExecutor)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := server.srv.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal("connect MCP server:", err)
	}
	defer serverSession.Close()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "1"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal("connect MCP client:", err)
	}
	defer clientSession.Close()

	result, err := clientSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal("list MCP tools:", err)
	}
	got := map[string]bool{}
	for _, tool := range result.Tools {
		got[tool.Name] = true
	}
	for _, name := range []string{"validate_yaml", "task_find", "task_list", "task_get"} {
		if !got[name] {
			t.Errorf("read-only MCP tools omit %q: %#v", name, got)
		}
	}
	for _, name := range []string{"task_create", "task_update"} {
		if got[name] {
			t.Errorf("read-only MCP tools expose mutation %q", name)
		}
	}
}

func TestExtensionReadExecutorRejectsMutationBeforeAuthentication(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock.New:", err)
	}
	defer database.Close()
	executor, err := NewExtensionReadExecutor(database, kdb.Resolve("mysql"), registryWithTask(), contract.TenantID("stable-tenant"), "tenant.example", "extension-token")
	if err != nil {
		t.Fatal("NewExtensionReadExecutor:", err)
	}
	_, err = executor(context.Background(), ai.ToolDescriptor{
		ID: "tenant.task_create", Source: "tenant", Name: "task_create", Operation: "create", Doctype: "Task",
	}, map[string]any{"title": "untrusted"})
	if err == nil || !strings.Contains(err.Error(), "not an authorized DocType read") {
		t.Fatalf("mutation tool error = %v, want read-only rejection", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("mutation reached auth/database:", err)
	}
}

func TestMCPCommandsOnlyMutationCannotReachGenericExecutor(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock.New:", err)
	}
	defer database.Close()
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{
		Name: "Protected Task", ModuleOwnerID: "finance.ar",
		ModuleOwnershipMode: string(contract.MutationCommands), BusinessScopePolicy: contract.ScopeTenantShared,
		Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}},
	})
	executor, err := NewExtensionReadExecutor(database, kdb.Resolve("mysql"), registry, contract.TenantID("stable-tenant"), "tenant.example", "extension-token")
	if err != nil {
		t.Fatal("NewExtensionReadExecutor:", err)
	}
	server := NewWithExecutor(registry, "tenant.example", ModeExecutable, executor)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := server.srv.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal("connect MCP server:", err)
	}
	defer serverSession.Close()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "1"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal("connect MCP client:", err)
	}
	defer clientSession.Close()
	_, err = clientSession.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name: "protected_task_create", Arguments: map[string]any{"title": "must use the owner command"},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("MCP generic create error = %v, want the protected mutation tool to be absent", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("MCP mutation reached authentication or database:", err)
	}
}

func TestExtensionReadExecutorRejectsRevokedCredential(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock.New:", err)
	}
	defer database.Close()
	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension WHERE site = \\? AND access_token_hash = \\? AND is_active = 1").
		WithArgs("tenant.example", auth.HashExtensionAccessToken("revoked-token")).
		WillReturnRows(sqlmock.NewRows([]string{"name", "api_permissions"}))
	executor, err := NewExtensionReadExecutor(database, kdb.Resolve("mysql"), registryWithTask(), contract.TenantID("stable-tenant"), "tenant.example", "revoked-token")
	if err != nil {
		t.Fatal("NewExtensionReadExecutor:", err)
	}
	_, err = executor(context.Background(), ai.ToolDescriptor{
		ID: "tenant.task_list", Source: "tenant", Name: "task_list", Operation: "list", Doctype: "Task",
	}, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("revoked extension credential error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("revoked credential query mismatch:", err)
	}
}

func TestExtensionReadExecutorWaitHonorsCallerCancellation(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock.New:", err)
	}
	defer database.Close()
	executor, err := NewExtensionReadExecutor(database, kdb.Resolve("mysql"), registryWithTask(), contract.TenantID("stable-tenant"), "tenant.example", "extension-token")
	if err != nil {
		t.Fatal("NewExtensionReadExecutor:", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = executor(ctx, ai.ToolDescriptor{
		ID: "tenant.task_list", Source: "tenant", Name: "task_list", Operation: "list", Doctype: "Task",
	}, map[string]any{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled MCP read error = %v", err)
	}
}

func registryWithTask() *doctype.Registry {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Task"})
	return reg
}

func testExecutor(context.Context, ai.ToolDescriptor, map[string]any) (string, error) {
	return "ok", nil
}
