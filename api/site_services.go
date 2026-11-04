package api

import (
	"sync"

	"github.com/asenawritescode/kora/internal/tenantruntime"
	"github.com/asenawritescode/kora/storage"
	"github.com/gin-gonic/gin"
)

// SiteRuntimeServices is a source-compatible per-site service fallback for
// external API consumers. Engine tenant requests use the immutable service
// bundle pinned in request context instead of consulting this mutable map.
type SiteRuntimeServices struct {
	mu             sync.RWMutex
	sites          map[string]SiteRuntimeService
	analyticsCache *AnalyticsQueryCache
}

// AnalyticsQueryCacheSnapshot reports the process router's retained semantic
// query cache budget for Engine resource metrics.
func (s *SiteRuntimeServices) AnalyticsQueryCacheSnapshot() AnalyticsQueryCacheSnapshot {
	// Retain this method for existing resource-metrics adapters. Engine process
	// composition observes its explicitly owned cache directly.
	return s.analyticsCacheOwner().AnalyticsQueryCacheSnapshot()
}

// SiteRuntimeService is the process-local service bundle for one loaded site.
type SiteRuntimeService = tenantruntime.Services

func NewSiteRuntimeServices() *SiteRuntimeServices {
	return NewSiteRuntimeServicesWithAnalyticsQueryCache(DefaultAnalyticsQueryCache())
}

func NewSiteRuntimeServicesWithAnalyticsQueryCache(cache *AnalyticsQueryCache) *SiteRuntimeServices {
	if cache == nil {
		cache = DefaultAnalyticsQueryCache()
	}
	return &SiteRuntimeServices{sites: make(map[string]SiteRuntimeService), analyticsCache: cache}
}

func (s *SiteRuntimeServices) analyticsCacheOwner() *AnalyticsQueryCache {
	if s == nil || s.analyticsCache == nil {
		return DefaultAnalyticsQueryCache()
	}
	return s.analyticsCache
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
