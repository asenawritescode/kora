package api

import (
	"sync"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/natsprovider"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/script"
	"github.com/asenawritescode/kora/secret"
	"github.com/asenawritescode/kora/storage"
	"github.com/asenawritescode/kora/webhook"
	"github.com/gin-gonic/gin"
)

// SiteRuntimeServices is the concurrency-safe, replaceable set of per-site
// services used by HTTP handlers. Directory updates publish a complete service
// bundle under one lock; request handlers never read a map while it is being
// mutated. It intentionally contains process-local services, not registry data.
type SiteRuntimeServices struct {
	mu    sync.RWMutex
	sites map[string]SiteRuntimeService
}

// SiteRuntimeService is the process-local service bundle for one loaded site.
type SiteRuntimeService struct {
	EventBus       analytics.EventBus
	AnalyticsRelay *analytics.CloudRelay
	Realtime       *natsprovider.Provider
	ScriptStore    *script.Store
	SecretStore    *secret.Store
	WebhookWorker  *webhook.Worker
	Outbox         outbox.Writer
	Storage        storage.Backend
}

func NewSiteRuntimeServices() *SiteRuntimeServices {
	return &SiteRuntimeServices{sites: make(map[string]SiteRuntimeService)}
}

// Replace publishes the complete bundle for a site atomically.
func (s *SiteRuntimeServices) Replace(site string, services SiteRuntimeService) {
	if s == nil || site == "" {
		return
	}
	s.mu.Lock()
	if s.sites == nil {
		s.sites = make(map[string]SiteRuntimeService)
	}
	s.sites[site] = services
	s.mu.Unlock()
}

// Remove atomically retires the handler-visible service bundle for a site.
func (s *SiteRuntimeServices) Remove(site string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.sites, site)
	s.mu.Unlock()
}

func (s *SiteRuntimeServices) Get(site string) (SiteRuntimeService, bool) {
	if s == nil {
		return SiteRuntimeService{}, false
	}
	s.mu.RLock()
	services, ok := s.sites[site]
	s.mu.RUnlock()
	return services, ok
}

func (s *SiteRuntimeServices) SetStorage(site string, backend storage.Backend) {
	if s == nil || site == "" {
		return
	}
	s.mu.Lock()
	if s.sites == nil {
		s.sites = make(map[string]SiteRuntimeService)
	}
	services := s.sites[site]
	services.Storage = backend
	s.sites[site] = services
	s.mu.Unlock()
}

// RuntimeServiceForContext returns the immutable bundle captured by routing
// middleware for this request, falling back to the live registry for system
// handlers that are not bound to a tenant runtime.
func RuntimeServiceForContext(c *gin.Context, registry *SiteRuntimeServices, site string) SiteRuntimeService {
	if c != nil {
		if value, ok := c.Get("site_runtime_services"); ok {
			if services, ok := value.(SiteRuntimeService); ok {
				return services
			}
		}
	}
	if services, ok := registry.Get(site); ok {
		return services
	}
	return SiteRuntimeService{}
}
