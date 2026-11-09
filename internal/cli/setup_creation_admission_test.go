package cli

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/site"
)

func TestSetupSiteCreationAdmitsAndDeduplicatesDatabaseEndpoints(t *testing.T) {
	t.Setenv("KORA_DB_BUDGET_KEY", "")
	t.Setenv("KORA_DB_MAX_BUDGET_ENDPOINTS", "64")
	t.Setenv("KORA_DB_ACTIVE_WORK_BUDGET", "2")
	t.Setenv("KORA_DB_ACTIVE_WORK_WAITERS", "1")
	t.Setenv("KORA_DB_ACTIVE_WORK_WAIT_MS", "20")
	t.Setenv("KORA_DB_SERVER_MAX_HEAVY_JOBS", "1")
	t.Setenv("KORA_DB_ENGINE_REPLICAS", "1")
	t.Setenv("DB_DSN", "")

	for _, separateEndpoint := range []bool{false, true} {
		name := "shared-endpoint"
		if separateEndpoint {
			name = "separate-endpoints"
		}
		t.Run(name, func(t *testing.T) {
			tenantHost := "setup-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".invalid"
			platformHost := tenantHost
			if separateEndpoint {
				platformHost = "platform-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".invalid"
			}
			config := &site.Config{DBType: "mysql", DBHost: tenantHost, DBPort: 3306, DBName: "tenant", DBUser: "test"}
			platformDSN := fmt.Sprintf("test@tcp(%s:3306)/platform", platformHost)
			var held func()
			var err error
			if separateEndpoint {
				held, err = acquireCLIWork(context.Background(), "mysql", platformDSN, true)
			} else {
				held, err = acquireSiteCLIWork(context.Background(), config, true)
			}
			if err != nil {
				t.Fatal("hold setup maintenance capacity:", err)
			}
			called := false
			waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			_, err = runSetupSiteCreation(waitCtx, config, "mysql", platformDSN, func() (*site.CreateSiteResult, error) {
				called = true
				return &site.CreateSiteResult{SiteID: "created"}, nil
			})
			cancel()
			if err == nil || called {
				t.Fatalf("site creation under saturated maintenance admission = (err %v, called %t), want rejection before operation", err, called)
			}

			held()
			result, err := runSetupSiteCreation(context.Background(), config, "mysql", platformDSN, func() (*site.CreateSiteResult, error) {
				called = true
				return &site.CreateSiteResult{SiteID: "created"}, nil
			})
			if err != nil || result == nil || result.SiteID != "created" || !called {
				t.Fatalf("site creation after capacity release = (%+v, %v), called=%t", result, err, called)
			}
			probe, err := acquireSiteCreationWork(context.Background(), config, "mysql", platformDSN)
			if err != nil {
				t.Fatal("site creation leaked endpoint maintenance capacity:", err)
			}
			probe()
		})
	}
}
