package org

import (
	"context"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

type testServiceProvider struct {
	ref    contract.ResourceRef
	opened int
	closed int
}

func (p *testServiceProvider) Ref() contract.ResourceRef { return p.ref }
func (p *testServiceProvider) Open(context.Context, contract.ServiceScope, string) (ServiceHandle, error) {
	p.opened++
	return testServiceHandle{provider: p}, nil
}
func (p *testServiceProvider) CloseService(string) error { p.closed++; return nil }

type testServiceHandle struct{ provider *testServiceProvider }

func (testServiceHandle) Name() string                 { return "notify" }
func (testServiceHandle) Scope() contract.ServiceScope { return contract.ScopeExecution }
func (h testServiceHandle) Close() error               { h.provider.closed++; return nil }

func TestServiceManagerActivatesAndRemovesPluginOwnedServices(t *testing.T) {
	m := NewServiceManager()
	provider := &testServiceProvider{ref: contract.ResourceRef{Namespace: "core", Name: "notifications", Version: 1}}
	if err := m.RegisterProvider(provider); err != nil {
		t.Fatal(err)
	}
	plugin := contract.ResourceRef{Namespace: "tenant-a", Name: "inventory", Version: 1}
	service := contract.PluginService{Name: "notify", Provider: provider.ref, Scope: contract.ScopeExecution, Cleanup: []string{"delivery_attempts"}}
	if err := m.Activate(plugin, []contract.PluginService{service}, contract.ScopeExecution, "execution-1"); err != nil {
		t.Fatal(err)
	}
	if len(m.Active(plugin)) != 1 || provider.opened != 1 {
		t.Fatalf("service was not activated: %+v", m.Active(plugin))
	}
	if err := m.Remove(plugin); err != nil {
		t.Fatal(err)
	}
	if len(m.Active(plugin)) != 0 || provider.closed == 0 {
		t.Fatalf("plugin effects were not cleaned up: closed=%d", provider.closed)
	}
}

func TestServiceManagerFailsBeforeActivationWhenProviderMissing(t *testing.T) {
	m := NewServiceManager()
	plugin := contract.ResourceRef{Namespace: "tenant-a", Name: "inventory", Version: 1}
	err := m.Activate(plugin, []contract.PluginService{{Name: "sms", Provider: contract.ResourceRef{Namespace: "core", Name: "sms", Version: 1}, Scope: contract.ScopeExecution}}, contract.ScopeExecution, "run-1")
	if err == nil {
		t.Fatal("missing provider activated")
	}
}
