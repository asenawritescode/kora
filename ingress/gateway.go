// Package ingress routes site requests to the Engine cell that owns the site.
// It is deliberately independent of Kora Cloud so self-hosted deployments can
// use the same SQL-backed site directory and cell routing.
package ingress

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asenawritescode/kora/contract"
)

const defaultCellID = "default"

type routeSnapshot struct {
	byAlias map[string][]contract.SiteDescriptor
	byID    map[string]contract.SiteDescriptor
	updated time.Time
}

// Gateway is a lightweight, cache-backed request router. The directory is
// refreshed outside the request path; an Engine/SQL outage does not interrupt
// routing from the last complete snapshot.
type Gateway struct {
	cellID     string
	cellURLs   map[string]*url.URL
	proxies    map[string]*httputil.ReverseProxy
	snapshot   atomic.Pointer[routeSnapshot]
	mu         sync.Mutex
	readyMu    sync.RWMutex
	localReady map[string]struct{}
}

func New(cellID string, cellURLs map[string]string) (*Gateway, error) {
	cellID, err := normalizeCellID(cellID)
	if err != nil || cellID == "" {
		return nil, errors.New("a valid current Engine cell id is required")
	}
	endpoints := make(map[string]*url.URL, len(cellURLs))
	for id, rawURL := range cellURLs {
		normalizedID, err := normalizeCellID(id)
		if err != nil || normalizedID == "" {
			return nil, fmt.Errorf("invalid Engine cell URL map key %q", id)
		}
		target, err := parseCellURL(rawURL)
		if err != nil {
			return nil, fmt.Errorf("invalid Engine endpoint for cell %q: %w", normalizedID, err)
		}
		if _, exists := endpoints[normalizedID]; exists {
			return nil, fmt.Errorf("duplicate Engine cell URL for %q", normalizedID)
		}
		endpoints[normalizedID] = target
	}
	proxies := make(map[string]*httputil.ReverseProxy, len(endpoints))
	for id, endpoint := range endpoints {
		proxies[id] = newProxy(endpoint)
	}
	return &Gateway{cellID: cellID, cellURLs: endpoints, proxies: proxies, localReady: make(map[string]struct{})}, nil
}

// MarkLocalReady controls whether an active site assigned to this cell may be
// routed locally. Assignment metadata can arrive before hot-add has completed;
// keeping it unready returns 503 instead of a misleading Engine 404.
func (g *Gateway) MarkLocalReady(siteID string, ready bool) {
	if siteID == "" {
		return
	}
	g.readyMu.Lock()
	defer g.readyMu.Unlock()
	if ready {
		g.localReady[siteID] = struct{}{}
	} else {
		delete(g.localReady, siteID)
	}
}

func (g *Gateway) isLocalReady(siteID string) bool {
	g.readyMu.RLock()
	defer g.readyMu.RUnlock()
	_, ok := g.localReady[siteID]
	return ok
}

// ParseCellURLs accepts comma-separated cellID=URL pairs. Cell URLs must be
// direct Engine origins, never the public ingress URL, to prevent proxy loops.
func ParseCellURLs(value string) (map[string]string, error) {
	result := make(map[string]string)
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		cellID, rawURL, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(cellID) == "" || strings.TrimSpace(rawURL) == "" {
			return nil, fmt.Errorf("invalid Engine cell URL entry %q; expected cell-id=https://engine-origin", entry)
		}
		cellID, err := normalizeCellID(cellID)
		if err != nil || cellID == "" {
			return nil, fmt.Errorf("invalid Engine cell id in URL entry %q", entry)
		}
		if _, exists := result[cellID]; exists {
			return nil, fmt.Errorf("duplicate Engine cell URL for %q", cellID)
		}
		if _, err := parseCellURL(rawURL); err != nil {
			return nil, fmt.Errorf("invalid Engine endpoint for cell %q: %w", cellID, err)
		}
		result[cellID] = strings.TrimSpace(rawURL)
	}
	return result, nil
}

func parseCellURL(raw string) (*url.URL, error) {
	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" || (target.Path != "" && target.Path != "/") {
		return nil, errors.New("endpoint must be an http(s) origin without credentials, path, query, or fragment")
	}
	target.Path = ""
	return target, nil
}

func normalizeCellID(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) > 80 {
		return "", errors.New("cell id exceeds 80 characters")
	}
	for _, ch := range value {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.') {
			return "", errors.New("cell id contains an unsupported character")
		}
	}
	return value, nil
}

// ReplaceDirectory atomically swaps the complete credential-free placement
// snapshot. Non-routable lifecycle states remain represented so requests fail
// closed rather than falling through to a different site's runtime.
func (g *Gateway) ReplaceDirectory(descriptors []contract.SiteDescriptor) {
	byID := make(map[string]contract.SiteDescriptor, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor.SiteID == "" {
			continue
		}
		byID[descriptor.SiteID] = cloneDescriptor(descriptor)
	}
	g.storeSnapshot(byID, time.Now())
}

// ApplyDescriptor updates one site's routing record from the current directory
// projection. The caller should pass the authoritative descriptor, not an old
// outbox payload.
func (g *Gateway) ApplyDescriptor(descriptor contract.SiteDescriptor) {
	if descriptor.SiteID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	byID := make(map[string]contract.SiteDescriptor)
	if current := g.snapshot.Load(); current != nil {
		for id, item := range current.byID {
			byID[id] = item
		}
	}
	if descriptor.Status == "deleted" {
		delete(byID, descriptor.SiteID)
	} else {
		byID[descriptor.SiteID] = cloneDescriptor(descriptor)
	}
	g.storeSnapshotLocked(byID, time.Now())
}

func cloneDescriptor(descriptor contract.SiteDescriptor) contract.SiteDescriptor {
	descriptor.Aliases = append([]string(nil), descriptor.Aliases...)
	return descriptor
}

func (g *Gateway) storeSnapshot(byID map[string]contract.SiteDescriptor, updated time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.storeSnapshotLocked(byID, updated)
}

func (g *Gateway) storeSnapshotLocked(byID map[string]contract.SiteDescriptor, updated time.Time) {
	byAlias := make(map[string][]contract.SiteDescriptor)
	for _, descriptor := range byID {
		// Site IDs are stable routing keys as well as identity keys. This lets
		// Cloud worker URLs survive hostname/domain changes; aliases remain valid
		// for customer-facing host and legacy /s/<hostname> requests.
		aliases := make(map[string]struct{}, len(descriptor.Aliases)+1)
		for _, alias := range append(append([]string(nil), descriptor.Aliases...), descriptor.SiteID) {
			if alias = normalizeAlias(alias); alias != "" {
				aliases[alias] = struct{}{}
			}
		}
		for alias := range aliases {
			byAlias[alias] = append(byAlias[alias], descriptor)
		}
	}
	g.snapshot.Store(&routeSnapshot{byAlias: byAlias, byID: byID, updated: updated})
}

// ValidateDirectory ensures this cell knows a direct endpoint for every other
// active cell before it starts accepting shared ingress traffic.
func (g *Gateway) ValidateDirectory(descriptors []contract.SiteDescriptor) error {
	for _, descriptor := range descriptors {
		if descriptor.Status != "active" && descriptor.Status != "degraded" {
			continue
		}
		cellID, valid := effectiveCellID(descriptor.RuntimeCellID)
		if !valid {
			return fmt.Errorf("site %s has an invalid runtime cell assignment", descriptor.SiteID)
		}
		if cellID != g.cellID && g.cellURLs[cellID] == nil {
			return fmt.Errorf("no direct Engine endpoint configured for assigned cell %q", cellID)
		}
	}
	return nil
}

func effectiveCellID(value string) (string, bool) {
	if strings.TrimSpace(value) == "" {
		return defaultCellID, true
	}
	cellID, err := normalizeCellID(value)
	return cellID, err == nil
}

// Wrap forwards requests whose resolved site belongs to another cell and lets
// the local Engine handle its own sites. Unknown paths are left to Engine's
// existing 404/system routing.
func (g *Gateway) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_kora/") || strings.HasPrefix(r.URL.Path, "/api/cloud/") {
			next.ServeHTTP(w, r)
			return
		}
		descriptor, found, ambiguous := g.resolve(r)
		if !found {
			next.ServeHTTP(w, r)
			return
		}
		if ambiguous {
			writeRoutingError(w, http.StatusConflict, "site_alias_conflict")
			return
		}
		if descriptor.Status != "active" && descriptor.Status != "degraded" {
			writeRoutingError(w, http.StatusServiceUnavailable, "site_unavailable")
			return
		}
		cellID, valid := effectiveCellID(descriptor.RuntimeCellID)
		if !valid {
			writeRoutingError(w, http.StatusServiceUnavailable, "site_cell_unavailable")
			return
		}
		if cellID == g.cellID {
			if !g.isLocalReady(descriptor.SiteID) {
				writeRoutingError(w, http.StatusServiceUnavailable, "site_runtime_not_ready")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		endpoint := g.cellURLs[cellID]
		if endpoint == nil {
			writeRoutingError(w, http.StatusServiceUnavailable, "site_cell_unavailable")
			return
		}
		g.proxies[cellID].ServeHTTP(w, r)
	})
}

func (g *Gateway) resolve(r *http.Request) (contract.SiteDescriptor, bool, bool) {
	snapshot := g.snapshot.Load()
	if snapshot == nil {
		return contract.SiteDescriptor{}, false, false
	}
	alias := ""
	if strings.HasPrefix(r.URL.Path, "/s/") {
		selector := strings.TrimPrefix(r.URL.Path, "/s/")
		if slash := strings.IndexByte(selector, '/'); slash >= 0 {
			selector = selector[:slash]
		}
		decoded, err := url.PathUnescape(selector)
		if err != nil {
			return contract.SiteDescriptor{}, false, false
		}
		alias = normalizeAlias(decoded)
	} else {
		alias = normalizeHost(r.Host)
	}
	if alias == "" {
		return contract.SiteDescriptor{}, false, false
	}
	matches := snapshot.byAlias[alias]
	if len(matches) == 0 {
		return contract.SiteDescriptor{}, false, false
	}
	if len(matches) > 1 {
		return contract.SiteDescriptor{}, true, true
	}
	return matches[0], true, false
}

func normalizeAlias(value string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
}

func normalizeHost(value string) string {
	value = strings.TrimSpace(value)
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	} else if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	}
	return normalizeAlias(value)
}

func newProxy(endpoint *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(endpoint)
	originalDirector := proxy.Director
	proxy.Director = func(request *http.Request) {
		host := request.Host
		originalDirector(request)
		request.Host = host
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, _ error) {
		writeRoutingError(w, http.StatusBadGateway, "site_cell_unavailable")
	}
	return proxy
}

func writeRoutingError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, code)
}

// SnapshotAge reports how long the in-memory placement view has been in use.
func (g *Gateway) SnapshotAge() time.Duration {
	snapshot := g.snapshot.Load()
	if snapshot == nil || snapshot.updated.IsZero() {
		return time.Duration(1<<63 - 1)
	}
	return time.Since(snapshot.updated)
}
