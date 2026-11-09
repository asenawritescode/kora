package engine

import (
	"database/sql"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
)

func TestNewKernelCommandProviderFactoryRequiresItsDependencies(t *testing.T) {
	complete := kernelCommandProviderFactory{
		database: new(sql.DB), registry: doctype.NewRegistry(), commands: kernel.NewCommandRegistry(),
		engine: kernel.New(&db.MySQLDialect{}, nil), tenant: "tenant-01M712N7GWZ54E51480KVV4BCB",
		owner: "pos", capability: "sales", validateSchema: func(contract.SchemaRef, []byte) error { return nil },
	}
	tests := []struct {
		name    string
		mutate  func(*kernelCommandProviderFactory)
		wantErr bool
	}{
		{"complete", func(*kernelCommandProviderFactory) {}, false},
		{"no database", func(f *kernelCommandProviderFactory) { f.database = nil }, true},
		{"no registry", func(f *kernelCommandProviderFactory) { f.registry = nil }, true},
		{"no command registry", func(f *kernelCommandProviderFactory) { f.commands = nil }, true},
		{"no kernel", func(f *kernelCommandProviderFactory) { f.engine = nil }, true},
		{"no owner", func(f *kernelCommandProviderFactory) { f.owner = "" }, true},
		{"no capability", func(f *kernelCommandProviderFactory) { f.capability = "" }, true},
		{"no schema validator", func(f *kernelCommandProviderFactory) { f.validateSchema = nil }, true},
		{"invalid tenant", func(f *kernelCommandProviderFactory) { f.tenant = "" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := complete
			tt.mutate(&f)
			factory, err := newKernelCommandProviderFactory(f)
			if (err != nil) != tt.wantErr {
				t.Fatalf("newKernelCommandProviderFactory error = %v, wantErr %t", err, tt.wantErr)
			}
			if err == nil && factory.Capability() != complete.capability {
				t.Fatalf("factory capability = %q, want %q", factory.Capability(), complete.capability)
			}
		})
	}
}

func TestRegisteredKernelCommandProvidersIgnoresModulesWithoutLocalOffers(t *testing.T) {
	providers, err := registeredKernelCommandProviders(moduleProviderSource{
		tenant: "tenant-01M712N7GWZ54E51480KVV4BCB", modules: []contract.ModuleDescriptor{{ID: "pos"}},
	}, nil, nil)
	if err != nil || len(providers) != 0 {
		t.Fatalf("registeredKernelCommandProviders = (%v, %v), want no providers and no error", providers, err)
	}
}
