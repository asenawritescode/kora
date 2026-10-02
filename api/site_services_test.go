package api

import (
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/asenawritescode/kora/script"
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
