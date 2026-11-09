package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/site"
)

func TestSetupConfigImportUsesJournaledImporterAdmission(t *testing.T) {
	t.Setenv("KORA_DB_BUDGET_KEY", "")
	t.Setenv("KORA_DB_MAX_BUDGET_ENDPOINTS", "64")
	t.Setenv("KORA_DB_ACTIVE_WORK_BUDGET", "2")
	t.Setenv("KORA_DB_ACTIVE_WORK_WAITERS", "1")
	t.Setenv("KORA_DB_ACTIVE_WORK_WAIT_MS", "20")
	t.Setenv("KORA_DB_SERVER_MAX_HEAVY_JOBS", "1")
	t.Setenv("KORA_DB_ENGINE_REPLICAS", "1")
	t.Setenv("DB_DSN", "")

	config := &site.Config{
		DBType: "mysql", DBHost: "setup-admission.invalid", DBPort: 3306,
		DBName: "kora_setup_test", DBUser: "test",
	}
	held, err := acquireSiteCLIWork(context.Background(), config, true)
	if err != nil {
		t.Fatal("hold tenant maintenance capacity:", err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err = runConfigImportForSiteContext(waitCtx, "setup.example.invalid", t.TempDir(), config, "tenant-setup-test")
	cancel()
	if err == nil {
		t.Fatal("setup import under saturated maintenance admission succeeded, want timeout")
	}

	held()
	if err := runConfigImportForSiteContext(context.Background(), "setup.example.invalid", t.TempDir(), config, "tenant-setup-test"); err == nil || !strings.Contains(err.Error(), "connecting to database") {
		t.Fatalf("setup import after admission = %v, want database connection failure after admission", err)
	}
	probe, err := acquireSiteCLIWork(context.Background(), config, true)
	if err != nil {
		t.Fatal("setup import leaked maintenance capacity:", err)
	}
	probe()
}
