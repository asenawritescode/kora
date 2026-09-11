package org

import (
	"context"
	"fmt"
	"sync"

	"github.com/asenawritescode/kora/contract"
)

// ServiceProvider is the narrow, scoped seam exposed to plugins. A provider
// never receives the registry or database; it can only open the service it
// registered for the requested scope.
type ServiceProvider interface {
	Ref() contract.ResourceRef
	Open(context.Context, contract.ServiceScope, string) (ServiceHandle, error)
}

type ServiceHandle interface {
	Name() string
	Scope() contract.ServiceScope
	Close() error
}

type serviceBinding struct {
	service  contract.PluginService
	provider ServiceProvider
	plugin   contract.ResourceRef
	handle   ServiceHandle
}

// ServiceManager manages plugin-owned services and makes ownership explicit so
// removal can close only effects owned by the removed plugin.
type ServiceManager struct {
	mu        sync.RWMutex
	providers map[string]ServiceProvider
	binding   map[string]serviceBinding
}

func NewServiceManager() *ServiceManager {
	return &ServiceManager{providers: map[string]ServiceProvider{}, binding: map[string]serviceBinding{}}
}

func (m *ServiceManager) RegisterProvider(provider ServiceProvider) error {
	if provider == nil || provider.Ref().Name == "" || provider.Ref().Namespace == "" || provider.Ref().Version <= 0 {
		return fmt.Errorf("service provider requires a versioned resource reference")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := provider.Ref().String()
	if _, exists := m.providers[key]; exists {
		return fmt.Errorf("service provider %q already registered", key)
	}
	m.providers[key] = provider
	return nil
}

func (m *ServiceManager) Activate(plugin contract.ResourceRef, services []contract.PluginService, scope contract.ServiceScope, scopeID string) error {
	if plugin.Name == "" || plugin.Namespace == "" || plugin.Version <= 0 || scopeID == "" {
		return fmt.Errorf("plugin and scope are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	opened := make([]ServiceHandle, 0, len(services))
	for _, service := range services {
		if service.Name == "" || service.Provider.Name == "" {
			return fmt.Errorf("service name and provider are required")
		}
		provider, ok := m.providers[service.Provider.String()]
		if !ok {
			return fmt.Errorf("%w: service provider %s", contract.ErrResourceKindUnknown, service.Provider.String())
		}
		if service.Scope != scope {
			return fmt.Errorf("service %q scope mismatch: manifest=%s activation=%s", service.Name, service.Scope, scope)
		}
		handle, err := provider.Open(context.Background(), scope, scopeID)
		if err != nil {
			for i := len(opened) - 1; i >= 0; i-- {
				_ = opened[i].Close()
			}
			return fmt.Errorf("open service %q: %w", service.Name, err)
		}
		key := plugin.String() + "/" + service.Name
		if _, exists := m.binding[key]; exists {
			_ = handle.Close()
			for i := len(opened) - 1; i >= 0; i-- {
				_ = opened[i].Close()
			}
			return fmt.Errorf("service %q already active", key)
		}
		opened = append(opened, handle)
		m.binding[key] = serviceBinding{service: service, provider: provider, plugin: plugin, handle: handle}
	}
	return nil
}

// Remove closes all services owned by plugin. If a provider is shared by
// another plugin it remains registered and active.
func (m *ServiceManager) Remove(plugin contract.ResourceRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, binding := range m.binding {
		if binding.plugin != plugin {
			continue
		}
		if binding.handle != nil {
			if err := binding.handle.Close(); err != nil {
				return fmt.Errorf("cleanup service %q: %w", key, err)
			}
		}
		delete(m.binding, key)
	}
	return nil
}

func (m *ServiceManager) Active(plugin contract.ResourceRef) []contract.PluginService {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []contract.PluginService
	for _, binding := range m.binding {
		if binding.plugin == plugin {
			out = append(out, binding.service)
		}
	}
	return out
}
