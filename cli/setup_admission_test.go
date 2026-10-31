package cli

import (
	"context"
	"testing"
	"time"

	"github.com/asenawritescode/kora/site"
)

func TestSetupConfigImportUsesBoundedMaintenanceAdmission(t *testing.T) {
	t.Setenv("KORA_DB_BUDGET_KEY", "")
	t.Setenv("KORA_DB_MAX_BUDGET_ENDPOINTS", "64")
	t.Setenv("KORA_DB_ACTIVE_WORK_BUDGET", "2")
	t.Setenv("KORA_DB_ACTIVE_WORK_WAITERS", "1")
	t.Setenv("KORA_DB_ACTIVE_WORK_WAIT_MS", "20")
	t.Setenv("KORA_DB_SERVER_MAX_HEAVY_JOBS", "1")
	t.Setenv("KORA_DB_ENGINE_REPLICAS", "1")
	t.Setenv("DB_DSN", "")

	config := &site.SiteConfig{
		DBType: "mysql", DBHost: "setup-admission.invalid", DBPort: 3306,
		DBName: "kora_setup_test", DBUser: "test",
	}
	held, err := acquireSiteCLIWork(context.Background(), config, true)
	if err != nil {
		t.Fatal("hold tenant maintenance capacity:", err)
	}
	called := false
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err = runSetupConfigImport(waitCtx, config, func() error {
		called = true
		return nil
	})
	cancel()
	if err == nil || called {
		t.Fatalf("setup import under saturated maintenance admission = (err %v, called %t), want rejected before import", err, called)
	}

	held()
	if err := runSetupConfigImport(context.Background(), config, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatal("setup import after maintenance capacity release:", err)
	}
	if !called {
		t.Fatal("setup import callback was not run after maintenance admission")
	}
	probe, err := acquireSiteCLIWork(context.Background(), config, true)
	if err != nil {
		t.Fatal("setup import leaked maintenance capacity:", err)
	}
	probe()
}
