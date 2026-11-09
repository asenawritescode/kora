package api

import (
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/asenawritescode/kora/internal/script"
	"github.com/gin-gonic/gin"
)

func TestSiteRuntimeServicesConcurrentReplaceGetRemove(t *testing.T) {
	services := NewSiteRuntimeServices()
	const workers = 12
	const iterations = 1000
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				site := fmt.Sprintf("site-%d", i%8)
				if worker%3 == 0 {
					services.Replace(site, SiteRuntimeService{ScriptStore: &script.Store{}})
				} else if worker%3 == 1 {
					_, _ = services.Get(site)
				} else {
					services.Remove(site)
				}
			}
		}()
	}
	wg.Wait()
}

func TestRuntimeServiceContextPinsRequestSnapshot(t *testing.T) {
	registry := NewSiteRuntimeServices()
	oldStore := &script.Store{}
	newStore := &script.Store{}
	registry.Replace("site-a", SiteRuntimeService{ScriptStore: newStore})
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Set("site_runtime_services", SiteRuntimeService{ScriptStore: oldStore})
	got := RuntimeServiceForContext(ginCtx, registry, "site-a")
	if got.ScriptStore != oldStore {
		t.Fatal("request did not retain the runtime services captured at routing time")
	}
}

func TestHandlerRuntimeServicePrefersGenerationSnapshotOverCompatibilityRegistry(t *testing.T) {
	oldStore := &script.Store{}
	newStore := &script.Store{}
	registry := NewSiteRuntimeServices()
	registry.Replace("site-a", SiteRuntimeService{ScriptStore: oldStore})
	handler := &Handler{RuntimeServices: registry}
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Set("site_runtime_services", SiteRuntimeService{ScriptStore: newStore})

	if got := handler.runtimeService(ginCtx, "site-a").ScriptStore; got != newStore {
		t.Fatal("handler resolved mutable compatibility registry instead of routed generation snapshot")
	}
}

func TestHandlerRuntimeServiceUsesLegacyMapsOnlyWhenRegistryHasNoEntry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registryStore := &script.Store{}
	legacyStore := &script.Store{}
	registry := NewSiteRuntimeServices()
	handler := &Handler{
		RuntimeServices:  registry,
		SiteScriptStores: map[string]*script.Store{"site-a": legacyStore},
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	services := handler.runtimeService(ctx, "site-a")
	if services.ScriptStore != legacyStore {
		t.Fatal("missing registry entry hid the configured legacy map")
	}

	registry.Replace("site-a", SiteRuntimeService{ScriptStore: registryStore})
	services = handler.runtimeService(ctx, "site-a")
	if services.ScriptStore != registryStore {
		t.Fatal("registry snapshot should take precedence over legacy maps")
	}

	registry.Replace("site-a", SiteRuntimeService{})
	services = handler.runtimeService(ctx, "site-a")
	if services.ScriptStore != nil {
		t.Fatal("an explicit empty registry snapshot resurrected a retired legacy service")
	}
}

func TestRuntimeServiceContextFallsBackWhenSnapshotIsAbsent(t *testing.T) {
	registry := NewSiteRuntimeServices()
	store := &script.Store{}
	registry.Replace("site-a", SiteRuntimeService{ScriptStore: store})
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	got := RuntimeServiceForContext(ginCtx, registry, "site-a")
	if got.ScriptStore != store {
		t.Fatal("legacy caller without a routed snapshot lost the registry fallback")
	}
}
