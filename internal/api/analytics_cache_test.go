package api

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/analytics"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/gin-gonic/gin"
)

func TestAnalyticsQueryCacheBoundsRetainedBytesAndSkipsOversizedValue(t *testing.T) {
	cache := newAnalyticsQueryCacheWithBudget(time.Minute, 8, 1024, 256)
	large := &analytics.SemanticQueryResponse{DataSource: "test", Queries: []analytics.SemanticQueryResult{{Rows: []map[string]any{{"value": strings.Repeat("x", 300)}}}}}
	if cache.put("large", large) {
		t.Fatal("cache retained a value larger than its per-entry byte cap")
	}
	if _, ok := cache.get("large"); ok {
		t.Fatal("oversized value unexpectedly appeared in the cache")
	}

	value := &analytics.SemanticQueryResponse{DataSource: "test", Queries: []analytics.SemanticQueryResult{{Rows: []map[string]any{{"value": "small"}}}}}
	if !cache.put("small", value) {
		t.Fatal("cache rejected a value within its byte caps")
	}
	if cache.usedBytes == 0 || cache.usedBytes > cache.maxBytes {
		t.Fatalf("retained cache bytes = %d, cap %d", cache.usedBytes, cache.maxBytes)
	}
	cached, ok := cache.get("small")
	if !ok {
		t.Fatal("expected bounded value to be cached")
	}
	var decoded analytics.SemanticQueryResponse
	if err := json.Unmarshal(cached, &decoded); err != nil || decoded.DataSource != "test" {
		t.Fatalf("cached response decode = (%+v, %v)", decoded, err)
	}
}

func TestAnalyticsQueryCacheCountsValuesThatCannotBeSerializedAsRejected(t *testing.T) {
	cache := newAnalyticsQueryCache(time.Minute, 4)
	value := &analytics.SemanticQueryResponse{Queries: []analytics.SemanticQueryResult{{Rows: []map[string]any{{"value": math.NaN()}}}}}
	if cache.put("invalid", value) {
		t.Fatal("cache accepted a non-JSON value")
	}
	if snapshot := cache.snapshot(); snapshot.Rejected != 1 || snapshot.Entries != 0 || snapshot.Bytes != 0 {
		t.Fatalf("cache snapshot after serialization rejection = %+v", snapshot)
	}
}

func TestAnalyticsQueryCacheEvictsLeastRecentlyUsedWithinByteBudget(t *testing.T) {
	cache := newAnalyticsQueryCacheWithBudget(time.Minute, 8, 500, 500)
	value := func(s string) *analytics.SemanticQueryResponse {
		return &analytics.SemanticQueryResponse{DataSource: s}
	}
	if !cache.put("first", value("first")) || !cache.put("second", value("second")) {
		t.Fatal("failed to populate small cache entries")
	}
	if _, ok := cache.get("first"); !ok {
		t.Fatal("expected first cache entry")
	}
	firstElement := cache.items["first"]
	firstBytes := firstElement.Value.(*analyticsQueryCacheEntry).bytes
	secondBytes := cache.items["second"].Value.(*analyticsQueryCacheEntry).bytes
	cache.maxBytes = firstBytes + secondBytes - 1
	if !cache.put("third", value("third")) {
		t.Fatal("expected cache to admit entry after eviction")
	}
	if _, ok := cache.get("second"); ok {
		t.Fatal("least recently used entry was not evicted")
	}
	if _, ok := cache.get("first"); !ok {
		t.Fatal("recently used entry was evicted")
	}
	if cache.usedBytes > cache.maxBytes {
		t.Fatalf("retained cache bytes = %d, cap %d", cache.usedBytes, cache.maxBytes)
	}
}

func TestAnalyticsQueryCacheExpiresAndReleasesByteAccounting(t *testing.T) {
	cache := newAnalyticsQueryCache(time.Millisecond, 8)
	if !cache.put("expires", &analytics.SemanticQueryResponse{DataSource: "test"}) {
		t.Fatal("cache rejected value")
	}
	time.Sleep(5 * time.Millisecond)
	if _, ok := cache.get("expires"); ok {
		t.Fatal("expired entry was returned")
	}
	if cache.usedBytes != 0 || len(cache.items) != 0 {
		t.Fatalf("expired entry retained bytes: %d across %d items", cache.usedBytes, len(cache.items))
	}
}

func TestProcessAnalyticsQueryCacheExposesItsOwnMetricsSnapshot(t *testing.T) {
	cache := NewAnalyticsQueryCache()
	cache.cache = newAnalyticsQueryCacheWithBudget(time.Minute, 4, 2048, 1024)
	if !cache.cache.put("tenant\x00query", &analytics.SemanticQueryResponse{DataSource: "test"}) {
		t.Fatal("cache rejected small test value")
	}
	snapshot := cache.AnalyticsQueryCacheSnapshot()
	if snapshot.Entries != 1 || snapshot.MaxEntries != 4 || snapshot.Bytes == 0 || snapshot.MaxBytes != 2048 {
		t.Fatalf("analytics cache snapshot = %+v", snapshot)
	}
}

func TestAnalyticsRouteGroupsReuseOneProcessCache(t *testing.T) {
	cache := NewAnalyticsQueryCache()
	services := NewSiteRuntimeServicesWithAnalyticsQueryCache(cache)
	router := gin.New()
	RegisterAnalyticsRoutesWithRuntimeServices(router.Group("/canonical"), nil, nil, nil, nil, nil, services)
	RegisterAnalyticsRoutesWithRuntimeServices(router.Group("/legacy"), nil, nil, nil, nil, nil, services)
	if cache.cache == nil {
		t.Fatal("Engine-owned cache has no query cache")
	}
	if services.AnalyticsQueryCacheSnapshot() != cache.AnalyticsQueryCacheSnapshot() {
		t.Fatal("legacy metrics adapter did not observe the process-owned cache")
	}
}

func TestAnalyticsQueryCacheKeyIncludesTenantConfigAndPermissionScope(t *testing.T) {
	registry := doctype.NewRegistry()
	registry.Permissions.LoadPermissionsFromDB(
		[]*doctype.Role{{Name: "Reader"}},
		[]*doctype.Permission{{Role: "Reader", Doctype: "Invoice", Read: true}},
	)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("GET", "/", nil)
	ctx.Set("policy_execution", routerTestExecution([]string{"Reader"}, nil))
	scope := analyticsPermissionScope(ctx, registry)
	cache := newAnalyticsQueryCache(time.Minute, 4)
	request := analytics.SemanticRequest{}
	base := cache.key("tenant-a", 4, scope, request)
	if base == cache.key("tenant-b", 4, scope, request) || base == cache.key("tenant-a", 5, scope, request) {
		t.Fatal("cache key omitted tenant or config revision")
	}
	ctx.Set("policy_execution", routerTestExecution([]string{"Administrator"}, nil))
	if base == cache.key("tenant-a", 4, analyticsPermissionScope(ctx, registry), request) {
		t.Fatal("cache key omitted authenticated permission scope")
	}
	ctx.Set("policy_execution", routerTestExecution([]string{"Reader"}, nil))
	registry.Permissions.LoadPermissionsFromDB(
		[]*doctype.Role{{Name: "Reader"}},
		[]*doctype.Permission{{Role: "Reader", Doctype: "Invoice", Read: false}},
	)
	if base == cache.key("tenant-a", 4, analyticsPermissionScope(ctx, registry), request) {
		t.Fatal("cache key omitted changed permission policy")
	}
	ctx.Request.Header.Set("X-Kora-Legal-Entity-ID", "entity-a")
	entityA := cache.key("tenant-a", 4, analyticsPermissionScope(ctx, registry), request)
	ctx.Request.Header.Set("X-Kora-Legal-Entity-ID", "entity-b")
	entityB := cache.key("tenant-a", 4, analyticsPermissionScope(ctx, registry), request)
	if entityA == base || entityA == entityB {
		t.Fatal("cache key omitted legal entity scope")
	}
}

func TestAnalyticsRebuildRequiresVerifiedAnalyticsWritePermission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{{Name: "Analytics"}}, []*doctype.Role{{Name: "Administrator"}}, []*doctype.Permission{{Doctype: "Analytics", Role: "Administrator", Write: true}})
	router := gin.New()
	var activeExecution policy.Execution
	router.Use(func(c *gin.Context) {
		if activeExecution.Valid() {
			c.Set("policy_execution", activeExecution)
		}
		c.Next()
	})
	RegisterAnalyticsRoutes(router.Group("/api"), registry, nil, nil, nil, nil)
	request := func(execution policy.Execution, roles []string) *httptest.ResponseRecorder {
		activeExecution = execution
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/analytics/rebuild", strings.NewReader(`{}`))
		req.Header.Set("X-Test-Roles", strings.Join(roles, ","))
		router.ServeHTTP(recorder, req)
		return recorder
	}
	if got := request(policy.Execution{}, []string{"Administrator"}).Code; got != 403 {
		t.Fatalf("caller roles without verified identity status=%d, want 403", got)
	}
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "admin", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"Administrator"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if got := request(execution, []string{"Guest"}).Code; got == 403 {
		t.Fatal("verified Administrator execution was denied based on transport role labels")
	}
}
