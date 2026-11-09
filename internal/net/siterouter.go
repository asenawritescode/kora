package net

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/tenantruntime"
	"github.com/gin-gonic/gin"
)

// LoadedSite and SiteRouterConfig are compatibility aliases for the tenant
// runtime model owned by internal/tenantruntime.
type LoadedSite = tenantruntime.Runtime
type SiteRouterConfig = tenantruntime.SiteConfig
type RuntimeSnapshot = tenantruntime.Snapshot

// SiteRouter is the HTTP facade over the tenant runtime manager. It owns no
// competing tenant registry or generation state.
type SiteRouter struct {
	manager *tenantruntime.Manager

	lazyMu          sync.RWMutex
	lazyDescriptors map[string]contract.SiteDescriptor
	lazyAliases     map[string]string
	lazyAmbiguous   map[string]bool
	lazyDefaultID   string
	lazyActivate    func(context.Context, string) error
}

const defaultMaxRetiringGenerations uint16 = 32

func NewSiteRouter(sites []*LoadedSite) *SiteRouter {
	router, err := NewSiteRouterWithRetirementLimit(sites, defaultMaxRetiringGenerations)
	if err != nil {
		panic(err)
	}
	return router
}

func NewSiteRouterWithRetirementLimit(sites []*LoadedSite, maxRetiring uint16, maxResident ...uint32) (*SiteRouter, error) {
	manager, err := tenantruntime.NewManager(sites, maxRetiring, maxResident...)
	if err != nil {
		return nil, err
	}
	return &SiteRouter{manager: manager}, nil
}

func (sr *SiteRouter) RuntimeManager() *tenantruntime.Manager {
	if sr == nil {
		return nil
	}
	return sr.manager
}

// SetLazyDirectory installs the credential-free alias projection used to
// activate a tenant only after it receives routed work. The activator fetches
// tenant credentials from the authoritative Engine directory for one ID.
func (sr *SiteRouter) SetLazyDirectory(descriptors []contract.SiteDescriptor, activate func(context.Context, string) error) {
	if sr == nil {
		return
	}
	sr.lazyMu.Lock()
	defer sr.lazyMu.Unlock()
	sr.lazyDescriptors = make(map[string]contract.SiteDescriptor, len(descriptors))
	sr.lazyAliases = make(map[string]string, len(descriptors)*2)
	sr.lazyAmbiguous = make(map[string]bool)
	sr.lazyDefaultID = ""
	sr.lazyActivate = activate
	for _, descriptor := range descriptors {
		sr.addLazyDescriptorLocked(descriptor)
	}
}

func (sr *SiteRouter) SetLazyActivator(activate func(context.Context, string) error) {
	if sr == nil {
		return
	}
	sr.lazyMu.Lock()
	sr.lazyActivate = activate
	sr.lazyMu.Unlock()
}

// UpdateLazyDescriptor advances the credential-free projection without
// changing a currently published runtime generation.
func (sr *SiteRouter) UpdateLazyDescriptor(descriptor contract.SiteDescriptor) {
	if sr == nil || descriptor.SiteID == "" {
		return
	}
	sr.lazyMu.Lock()
	defer sr.lazyMu.Unlock()
	if sr.lazyDescriptors == nil {
		sr.lazyDescriptors = make(map[string]contract.SiteDescriptor)
		sr.lazyAliases = make(map[string]string)
		sr.lazyAmbiguous = make(map[string]bool)
	}
	sr.removeLazyDescriptorLocked(descriptor.SiteID)
	if descriptor.Status != "deleted" {
		sr.addLazyDescriptorLocked(descriptor)
	}
}

func (sr *SiteRouter) RemoveLazyDescriptor(siteID string) {
	if sr == nil || siteID == "" {
		return
	}
	sr.lazyMu.Lock()
	defer sr.lazyMu.Unlock()
	sr.removeLazyDescriptorLocked(siteID)
}

func (sr *SiteRouter) addLazyDescriptorLocked(descriptor contract.SiteDescriptor) {
	if descriptor.SiteID == "" || descriptor.Status == "deleted" {
		return
	}
	descriptor.Aliases = append([]string(nil), descriptor.Aliases...)
	sr.lazyDescriptors[descriptor.SiteID] = descriptor
	if sr.lazyDefaultID == "" && lazyDescriptorAcceptsTraffic(descriptor) {
		sr.lazyDefaultID = descriptor.SiteID
	}
	aliases := append([]string{descriptor.SiteID}, descriptor.Aliases...)
	for _, alias := range aliases {
		alias = normalizeSiteHost(alias)
		if alias == "" {
			continue
		}
		if current := sr.lazyAliases[alias]; current != "" && current != descriptor.SiteID {
			delete(sr.lazyAliases, alias)
			sr.lazyAmbiguous[alias] = true
			continue
		}
		if !sr.lazyAmbiguous[alias] {
			sr.lazyAliases[alias] = descriptor.SiteID
		}
	}
}

func (sr *SiteRouter) removeLazyDescriptorLocked(siteID string) {
	delete(sr.lazyDescriptors, siteID)
	for alias, owner := range sr.lazyAliases {
		if owner == siteID {
			delete(sr.lazyAliases, alias)
		}
	}
	// Rebuild aliases because a formerly ambiguous name may become unique.
	ids := make([]string, 0, len(sr.lazyDescriptors))
	for id := range sr.lazyDescriptors {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	sr.lazyAliases = make(map[string]string, len(ids)*2)
	sr.lazyAmbiguous = make(map[string]bool)
	sr.lazyDefaultID = ""
	for _, id := range ids {
		sr.addLazyDescriptorLocked(sr.lazyDescriptors[id])
	}
}

func lazyDescriptorAcceptsTraffic(descriptor contract.SiteDescriptor) bool {
	return descriptor.Status == "" || descriptor.Status == "active" || descriptor.Status == "degraded"
}

// EnsureHost activates a known cold tenant before routing. It returns known
// false when the host is not in the Engine's credential-free directory view.
func (sr *SiteRouter) EnsureHost(ctx context.Context, host string, fallback bool) (known bool, err error) {
	if sr == nil {
		return false, nil
	}
	host = normalizeSiteHost(host)
	if runtime, _ := sr.manager.ByHost(host); runtime != nil {
		return true, nil
	}
	sr.lazyMu.RLock()
	siteID := sr.lazyAliases[host]
	if siteID == "" && fallback && (net.ParseIP(host) != nil || host == "localhost" || host == "127.0.0.1") {
		siteID = sr.lazyDefaultID
	}
	descriptor, found := sr.lazyDescriptors[siteID]
	activate := sr.lazyActivate
	ambiguous := sr.lazyAmbiguous[host]
	sr.lazyMu.RUnlock()
	if ambiguous {
		return true, fmt.Errorf("site alias conflict")
	}
	if siteID == "" || !found {
		return false, nil
	}
	return true, sr.activateLazyDescriptor(ctx, descriptor, activate)
}

// EnsureName activates a known cold tenant selected by its stable ID or alias.
func (sr *SiteRouter) EnsureName(ctx context.Context, name string) (known bool, err error) {
	if sr == nil {
		return false, nil
	}
	if runtime := sr.manager.ByName(name); runtime != nil {
		return true, nil
	}
	name = normalizeSiteHost(name)
	sr.lazyMu.RLock()
	siteID := sr.lazyAliases[name]
	descriptor, found := sr.lazyDescriptors[siteID]
	activate := sr.lazyActivate
	ambiguous := sr.lazyAmbiguous[name]
	sr.lazyMu.RUnlock()
	if ambiguous {
		return true, fmt.Errorf("site alias conflict")
	}
	if siteID == "" || !found {
		return false, nil
	}
	return true, sr.activateLazyDescriptor(ctx, descriptor, activate)
}

func (sr *SiteRouter) activateLazyDescriptor(ctx context.Context, descriptor contract.SiteDescriptor, activate func(context.Context, string) error) error {
	if !lazyDescriptorAcceptsTraffic(descriptor) {
		return fmt.Errorf("tenant is not accepting traffic")
	}
	if activate == nil {
		return fmt.Errorf("tenant activation is unavailable")
	}
	if err := activate(ctx, descriptor.SiteID); err != nil {
		slog.Error("tenant runtime activation failed", "site_id", descriptor.SiteID, "error", err)
		return err
	}
	return nil
}

func (sr *SiteRouter) IsLazyAliasAmbiguous(host string) bool {
	if sr == nil {
		return false
	}
	sr.lazyMu.RLock()
	defer sr.lazyMu.RUnlock()
	return sr.lazyAmbiguous[normalizeSiteHost(host)]
}
func (sr *SiteRouter) ResourceCounts() (active, retiring int) {
	if sr == nil || sr.manager == nil {
		return 0, 0
	}
	return sr.manager.ResourceCounts()
}
func (sr *SiteRouter) ResidentCapacity() int {
	if sr == nil || sr.manager == nil {
		return 0
	}
	return sr.manager.ResidentCapacity()
}
func (sr *SiteRouter) AllSites() []*LoadedSite {
	if sr == nil {
		return nil
	}
	return sr.manager.All()
}
func (sr *SiteRouter) AllSnapshots() []*RuntimeSnapshot {
	if sr == nil || sr.manager == nil {
		return nil
	}
	return sr.manager.AllSnapshots()
}
func (sr *SiteRouter) SnapshotByID(siteID string) *RuntimeSnapshot {
	if sr == nil || sr.manager == nil {
		return nil
	}
	return sr.manager.SnapshotByID(contract.TenantID(siteID))
}
func (sr *SiteRouter) SnapshotByName(name string) *RuntimeSnapshot {
	if sr == nil || sr.manager == nil {
		return nil
	}
	return sr.manager.SnapshotByName(name)
}
func (sr *SiteRouter) AddSite(site *LoadedSite) error {
	return sr.manager.Add(context.Background(), site)
}
func (sr *SiteRouter) ReplaceSite(site *LoadedSite) (*LoadedSite, error) {
	return sr.ReplaceSiteContext(context.Background(), site)
}
func (sr *SiteRouter) ReplaceSiteContext(ctx context.Context, site *LoadedSite) (*LoadedSite, error) {
	if site == nil {
		return nil, fmt.Errorf("cannot add nil site")
	}
	return sr.manager.Activate(ctx, runtimeActivation(site), site)
}
func runtimeActivation(site *LoadedSite) tenantruntime.Activation {
	return tenantruntime.Activation{
		Tenant:            contract.TenantID(site.SiteID),
		ConfigRevision:    contract.Revision(strconv.FormatUint(site.ConfigRevision, 10)),
		DirectoryRevision: site.DirectoryRevision,
		ResourceRevision:  contract.Revision(site.DatabaseIdentity),
	}
}
func (sr *SiteRouter) RemoveSiteByID(siteID string) *LoadedSite {
	site, _ := sr.RemoveSiteByIDContext(context.Background(), siteID)
	return site
}
func (sr *SiteRouter) RemoveSiteByIDContext(ctx context.Context, siteID string) (*LoadedSite, error) {
	return sr.manager.Retire(ctx, contract.TenantID(siteID))
}
func (sr *SiteRouter) EvictLeastRecentlyUsedIdle(ctx context.Context) (*LoadedSite, error) {
	if sr == nil || sr.manager == nil {
		return nil, tenantruntime.ErrManagerClosed
	}
	return sr.manager.EvictLeastRecentlyUsedIdle(ctx)
}
func (sr *SiteRouter) FinishRetirement(site *LoadedSite) {
	if sr != nil {
		sr.manager.FinishRetirement(site)
	}
}
func (sr *SiteRouter) ValidateAliases() error { return sr.manager.ValidateAliases() }
func (sr *SiteRouter) UpdateSiteConfig(name string, config SiteRouterConfig) *LoadedSite {
	return sr.manager.UpdateConfig(name, config)
}
func (sr *SiteRouter) RemoveSite(name string) *LoadedSite         { return sr.manager.RemoveByName(name) }
func (sr *SiteRouter) SiteByName(name string) *LoadedSite         { return sr.manager.ByName(name) }
func (sr *SiteRouter) SiteByID(siteID string) *LoadedSite         { return sr.manager.ByID(siteID) }
func (sr *SiteRouter) SiteByHost(host string) (*LoadedSite, bool) { return sr.manager.ByHost(host) }
func (sr *SiteRouter) ApplySiteStatus(siteID, status string, revision uint64) error {
	return sr.manager.SetStatus(contract.TenantID(siteID), status, revision)
}
func (sr *SiteRouter) ApplySiteDescriptor(descriptor contract.SiteDescriptor) error {
	return sr.manager.ApplyDescriptor(contract.TenantID(descriptor.SiteID), descriptor.DirectoryRevision, descriptor.ConfigRevision, descriptor.Status, descriptor.Aliases)
}
func (sr *SiteRouter) AdvanceDirectoryRevision(siteID string, revision uint64) error {
	return sr.manager.AdvanceDirectoryRevision(contract.TenantID(siteID), revision)
}
func (sr *SiteRouter) WaitForTenantIdle(ctx context.Context, siteID string) error {
	return sr.manager.WaitForIdle(ctx, contract.TenantID(siteID))
}
func (sr *SiteRouter) RetireUnavailable(ctx context.Context, siteID string) (*LoadedSite, error) {
	return sr.manager.RetireUnavailable(ctx, contract.TenantID(siteID))
}
func (sr *SiteRouter) AllDomains() []string {
	if sr == nil {
		return nil
	}
	seen := make(map[string]struct{})
	domains := sr.manager.Domains()
	for _, domain := range domains {
		seen[normalizeSiteHost(domain)] = struct{}{}
	}
	sr.lazyMu.RLock()
	for _, descriptor := range sr.lazyDescriptors {
		for _, alias := range descriptor.Aliases {
			alias = normalizeSiteHost(alias)
			if alias == "" {
				continue
			}
			if _, ok := seen[alias]; !ok {
				seen[alias] = struct{}{}
				domains = append(domains, alias)
			}
		}
	}
	sr.lazyMu.RUnlock()
	return domains
}
func (sr *SiteRouter) IsAmbiguousHost(host string) bool {
	return sr != nil && (sr.manager.IsAmbiguousHost(host) || sr.IsLazyAliasAmbiguous(host))
}
func (sr *SiteRouter) AcquireByName(ctx context.Context, name string) (*LoadedSite, *tenantruntime.Handle, bool) {
	_, _ = sr.EnsureName(ctx, name)
	return sr.manager.AcquireByName(ctx, name)
}
func (sr *SiteRouter) PinExistingByID(ctx context.Context, siteID string) (*LoadedSite, *tenantruntime.Handle, error) {
	if sr == nil || sr.manager == nil {
		return nil, nil, tenantruntime.ErrTenantNotActive
	}
	return sr.manager.PinExisting(ctx, contract.TenantID(siteID))
}
func (sr *SiteRouter) AcquireByID(ctx context.Context, siteID string) (*tenantruntime.Handle, error) {
	if sr == nil || sr.manager == nil {
		return nil, tenantruntime.ErrTenantNotActive
	}
	_, _ = sr.EnsureName(ctx, siteID)
	return sr.manager.Acquire(ctx, contract.TenantID(siteID))
}
func (sr *SiteRouter) AcquireByHost(ctx context.Context, host string, fallback bool) (*LoadedSite, *tenantruntime.Handle, bool, bool) {
	_, _ = sr.EnsureHost(ctx, host, fallback)
	return sr.manager.AcquireByHost(ctx, host, fallback)
}

func siteAcceptsTrafficSnapshot(snapshot *tenantruntime.Snapshot) bool {
	return snapshot != nil && (snapshot.Status == "" || snapshot.Status == "active" || snapshot.Status == "degraded")
}

func normalizeSiteHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

type pathRoutedSiteContextKey struct{}
type pathRoutedRuntime struct {
	site     *LoadedSite
	snapshot *tenantruntime.Snapshot
}

// InjectSiteRuntimeContext restores the tenant-scoped request state from the
// pinned runtime snapshot. Shared path-based consumers such as the embedded
// workspace use it before re-dispatching a request through Gin.
func InjectSiteRuntimeContext(c *gin.Context, snapshot *tenantruntime.Snapshot) {
	if snapshot == nil {
		return
	}
	c.Set("site_runtime_snapshot", snapshot)
	c.Set("site_name", snapshot.Name)
	c.Set("site_id", string(snapshot.Activation.Tenant))
	c.Set("site_status", snapshot.Status)
	c.Set("site_config_revision", snapshot.ConfigRevision)
	c.Set("site_directory_revision", snapshot.Activation.DirectoryRevision)
	c.Set("site_db_type", snapshot.DBType)
	c.Set("site_db", snapshot.DB)
	c.Set("site_registry", snapshot.Registry)
	if snapshot.SessionManager != nil {
		c.Set("site_session_manager", snapshot.SessionManager)
	}
	c.Set("site_analytics_worker", snapshot.AnalyticsWorker)
	if snapshot.HasServices {
		c.Set("site_runtime_services", snapshot.Services)
	}
}

func injectSiteRuntimeContext(c *gin.Context, snapshot *tenantruntime.Snapshot) {
	InjectSiteRuntimeContext(c, snapshot)
}

// Middleware returns a Gin middleware that resolves the Host header to a site.
func (sr *SiteRouter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Path-based routing has already resolved and pinned the runtime before
		// Gin re-dispatches a rewritten /s/<id>/api request. The typed context key
		// cannot be supplied by a client and deliberately bypasses Host checks for
		// the shared-ingress hostname while restoring tenant context.
		if routed, ok := c.Request.Context().Value(pathRoutedSiteContextKey{}).(pathRoutedRuntime); ok && routed.site != nil {
			injectSiteRuntimeContext(c, routed.snapshot)
			c.Next()
			return
		}

		// Skip site resolution for console, health, and other system paths.
		path := c.Request.URL.Path
		if path == "/health" || path == "/readyz" || strings.HasPrefix(path, "/_kora/") || strings.HasPrefix(path, "/api/console") || strings.HasPrefix(path, "/api/management/v2/") || strings.HasPrefix(path, "/console") || strings.HasPrefix(path, "/assets/") || path == "/api/ping" || strings.HasPrefix(path, "/s/") {
			c.Next()
			return
		}

		// If already set by path-based routing, skip.
		if _, exists := c.Get("site_db"); exists {
			c.Next()
			return
		}

		host := normalizeSiteHost(stripPort(c.Request.Host))
		if sr.IsAmbiguousHost(host) {
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "site_alias_conflict", "message": "Host maps to more than one site"})
			return
		}
		// A site cookie selects an alias only after the manager pins the active
		// snapshot; authorization then evaluates the same snapshot as the handler.
		if siteName, _ := c.Cookie("kora_site"); siteName != "" {
			known, err := sr.EnsureName(c.Request.Context(), siteName)
			if err != nil {
				if errors.Is(err, tenantruntime.ErrResidentCapacity) || errors.Is(err, tenantruntime.ErrActivationCapacity) {
					c.Header("Retry-After", "1")
					code, message := tenantCapacityError(err)
					c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": code, "message": message})
					return
				}
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "site_unavailable", "message": "Site runtime could not be activated"})
				return
			}
			// An explicit tenant cookie is an identity selector, not a hint. If
			// it is stale or unknown, falling through to localhost's default site
			// can expose a different tenant's branding and data under this URL.
			if !known {
				c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "site_not_found", "message": "No site configured for the selected tenant"})
				return
			}
			if site, handle, acquired := sr.manager.AcquireByName(c.Request.Context(), siteName); site != nil {
				if !acquired {
					c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "site_draining", "message": "Site runtime is being replaced"})
					return
				}
				snapshot := handle.Snapshot()
				if !sr.isHostAllowedForSite(host, snapshot.Config) {
					handle.Release()
					c.AbortWithStatusJSON(403, gin.H{"error": "site_access_denied", "message": fmt.Sprintf("Site %q cannot be accessed via host %q", siteName, host)})
					return
				}
				if !siteAcceptsTrafficSnapshot(snapshot) {
					handle.Release()
					c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "site_unavailable", "message": "Site is not accepting traffic"})
					return
				}
				injectSiteRuntimeContext(c, snapshot)
				defer handle.Release()
				c.Next()
				return
			}
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "site_unavailable", "message": "The selected tenant runtime is unavailable"})
			return
		}

		if _, err := sr.EnsureHost(c.Request.Context(), host, true); err != nil {
			if errors.Is(err, tenantruntime.ErrResidentCapacity) || errors.Is(err, tenantruntime.ErrActivationCapacity) {
				c.Header("Retry-After", "1")
				code, message := tenantCapacityError(err)
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": code, "message": message})
				return
			}
			if sr.IsAmbiguousHost(host) {
				c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "site_alias_conflict", "message": "Host maps to more than one site"})
				return
			}
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "site_unavailable", "message": "Site runtime could not be activated"})
			return
		}
		site, handle, ambiguous, acquired := sr.manager.AcquireByHost(c.Request.Context(), host, true)
		if ambiguous {
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "site_alias_conflict", "message": "Host maps to more than one site"})
			return
		}
		if site == nil {
			c.AbortWithStatusJSON(404, gin.H{"error": "site_not_found", "message": fmt.Sprintf("No site configured for host: %s", host)})
			return
		}
		if !acquired {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "site_draining", "message": "Site runtime is being replaced"})
			return
		}
		snapshot := handle.Snapshot()
		if !siteAcceptsTrafficSnapshot(snapshot) {
			handle.Release()
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "site_unavailable", "message": "Site is not accepting traffic"})
			return
		}
		injectSiteRuntimeContext(c, snapshot)
		if existing, _ := c.Cookie("kora_site"); existing != site.Name {
			SetSecureCookie(c, "kora_site", site.Name, 86400, "/", false)
		}
		defer handle.Release()
		c.Next()
	}
}

func tenantCapacityError(err error) (string, string) {
	if errors.Is(err, tenantruntime.ErrActivationCapacity) {
		return "activation_capacity", "Tenant activation is temporarily busy"
	}
	return "resident_capacity", "Tenant runtime capacity is temporarily full"
}

// isHostAllowedForSite checks if the given host is allowed to access a site via the kora_site cookie.
// Allowed: localhost, loopback IPs, any parsed IP (dev/testing), the KORA_HOST env var, or a domain configured for the site.
func (sr *SiteRouter) isHostAllowedForSite(host string, config SiteRouterConfig) bool {
	if net.ParseIP(host) != nil || host == "localhost" || host == "127.0.0.1" {
		return true
	}
	// Allow the configured app host (set via KORA_HOST env var).
	if appHost := os.Getenv("KORA_HOST"); appHost != "" && normalizeSiteHost(appHost) == host {
		return true
	}
	for _, d := range config.Domains {
		if normalizeSiteHost(d) == host {
			return true
		}
	}
	return false
}

// RegisterPathSiteRoutes adds a NoRoute handler that intercepts /s/:site/*
// requests, injects site context, rewrites the path, and re-dispatches API calls.
// This allows multi-site access without DNS or /etc/hosts.
//
//	localhost:8000/s/fieldwork/api/...  → fieldwork site
//
// The Engine serves no UI files; anything else under /s/:site returns 404.
func RegisterPathSiteRoutes(router *gin.Engine, sr *SiteRouter) {
	router.NoRoute(func(c *gin.Context) {
		slog.Debug("path-based site route", "path", c.Request.URL.Path, "method", c.Request.Method)
		path := c.Request.URL.Path

		// Not a path-based site URL — return 404.
		if !strings.HasPrefix(path, "/s/") {
			c.JSON(404, gin.H{"error": "not_found"})
			return
		}

		// Parse /s/:site/...
		rest := strings.TrimPrefix(path, "/s/")
		slashIdx := strings.Index(rest, "/")
		var siteName string
		if slashIdx >= 0 {
			siteName = rest[:slashIdx]
			rest = rest[slashIdx:]
		} else {
			siteName = rest
			rest = "/"
		}

		known, activationErr := sr.EnsureName(c.Request.Context(), siteName)
		if !known && activationErr == nil {
			slog.Debug("path-based site is absent from the local tenant directory", "site", siteName)
		}
		if activationErr != nil {
			if errors.Is(activationErr, tenantruntime.ErrResidentCapacity) || errors.Is(activationErr, tenantruntime.ErrActivationCapacity) {
				c.Header("Retry-After", "1")
				code, message := tenantCapacityError(activationErr)
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": code, "message": message})
				return
			}
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "site_unavailable", "message": "Site runtime could not be activated"})
			return
		}
		site, handle, acquired := sr.manager.AcquireByName(c.Request.Context(), siteName)
		if site == nil {
			c.JSON(404, gin.H{"error": "site_not_found", "message": "No site: " + siteName})
			return
		}
		if !acquired {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "site_draining", "message": "Site runtime is being replaced"})
			return
		}
		defer handle.Release()

		// Inject site context.
		snapshot := handle.Snapshot()
		if !siteAcceptsTrafficSnapshot(snapshot) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "site_unavailable", "message": "Site is not accepting traffic"})
			return
		}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), pathRoutedSiteContextKey{}, pathRoutedRuntime{site: site, snapshot: snapshot}))
		injectSiteRuntimeContext(c, snapshot)

		// Handle API paths.
		if strings.HasPrefix(rest, "/api/") || rest == "/api" {
			// API requests: rewrite path and let Gin re-dispatch.
			// We need to re-enter the router for API routes to match.
			c.Request.AddCookie(&http.Cookie{Name: "kora_site", Value: snapshot.Name, Path: "/"})
			c.Request.URL.Path = rest
			// Buffer the request body for re-dispatch — HandleContext
			// re-processes the middleware chain, so we need a fresh body reader.
			if c.Request.Body != nil && c.Request.ContentLength > 0 {
				bodyBytes, err := io.ReadAll(c.Request.Body)
				if err == nil {
					c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
					slog.Info("body buffered for re-dispatch", "path", rest, "size", len(bodyBytes))
				} else {
					slog.Warn("body buffer read failed", "path", rest, "error", err)
				}
			} else if c.Request.ContentLength > 0 && c.Request.Body == nil {
				slog.Warn("body not buffered", "path", rest, "bodyNil", c.Request.Body == nil, "contentLength", c.Request.ContentLength)
			}
			// Manually call HandleContext — but this time site context is already set
			// and the SiteRouter middleware will skip (checks for existing site_db).
			router.HandleContext(c)
			return
		}

		c.JSON(404, gin.H{"error": "not_found"})
	})
}

func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
