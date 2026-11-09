package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/site"
)

func TestTenantSiteProvisioningAdmissionReusesSharedEndpointPermit(t *testing.T) {
	configureSiteProvisioningAdmissionTest(t)
	host := "console-shared-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".invalid"
	tenant := &site.Config{DBType: "mysql", DBHost: host, DBPort: 3306, DBName: "tenant", DBUser: "test"}
	platformDSN := fmt.Sprintf("test@tcp(%s:3306)/platform", host)
	admission, err := site.ConnectionAdmission(tenant)
	if err != nil {
		t.Fatal("resolve shared endpoint admission:", err)
	}
	held, err := admission.AcquireMaintenance(context.Background())
	if err != nil {
		t.Fatal("hold shared endpoint maintenance capacity:", err)
	}
	release, err := tenantSiteProvisioningAdmission("mysql", platformDSN)(context.Background(), tenant)
	if err != nil {
		held()
		t.Fatal("shared endpoint should reuse existing platform admission:", err)
	}
	release()
	if snapshot := admission.Snapshot(); snapshot.MaintenanceActive != 1 {
		t.Fatalf("shared endpoint maintenance active = %d after no-op tenant reservation; want existing permit only", snapshot.MaintenanceActive)
	}
	held()
}

func TestTenantSiteProvisioningAdmissionBoundsSeparateTenantEndpoint(t *testing.T) {
	configureSiteProvisioningAdmissionTest(t)
	platformHost := "console-platform-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".invalid"
	tenantHost := "console-tenant-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".invalid"
	tenant := &site.Config{DBType: "mysql", DBHost: tenantHost, DBPort: 3306, DBName: "tenant", DBUser: "test"}
	platformDSN := fmt.Sprintf("test@tcp(%s:3306)/platform", platformHost)
	acquire := tenantSiteProvisioningAdmission("mysql", platformDSN)
	release, err := acquire(context.Background(), tenant)
	if err != nil {
		t.Fatal("admit separate tenant endpoint:", err)
	}
	admission, err := site.ConnectionAdmission(tenant)
	if err != nil {
		release()
		t.Fatal("resolve tenant endpoint admission:", err)
	}
	if snapshot := admission.Snapshot(); snapshot.MaintenanceActive != 1 {
		release()
		t.Fatalf("tenant maintenance active = %d while provisioning, want 1", snapshot.MaintenanceActive)
	}
	release()
	if snapshot := admission.Snapshot(); snapshot.MaintenanceActive != 0 {
		t.Fatalf("tenant maintenance active = %d after release, want 0", snapshot.MaintenanceActive)
	}
}

func TestTenantSiteProvisioningAdmissionClassifiesTemporaryCapacity(t *testing.T) {
	configureSiteProvisioningAdmissionTest(t)
	platformHost := "console-platform-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".invalid"
	tenantHost := "console-tenant-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".invalid"
	tenant := &site.Config{DBType: "mysql", DBHost: tenantHost, DBPort: 3306, DBName: "tenant", DBUser: "test"}
	platformDSN := fmt.Sprintf("test@tcp(%s:3306)/platform", platformHost)
	admission, err := site.ConnectionAdmission(tenant)
	if err != nil {
		t.Fatal("resolve tenant endpoint admission:", err)
	}
	held, err := admission.AcquireMaintenance(context.Background())
	if err != nil {
		t.Fatal("hold tenant maintenance capacity:", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := tenantSiteProvisioningAdmission("mysql", platformDSN)(ctx, tenant); !errors.Is(err, api.ErrTenantProvisioningCapacity) {
		t.Fatalf("saturated provisioning admission error = %v, want temporary capacity classification", err)
	}
	held()
}

func configureSiteProvisioningAdmissionTest(t *testing.T) {
	t.Helper()
	t.Setenv("KORA_DB_BUDGET_KEY", "")
	t.Setenv("KORA_DB_MAX_BUDGET_ENDPOINTS", "64")
	t.Setenv("KORA_DB_ACTIVE_WORK_BUDGET", "2")
	t.Setenv("KORA_DB_ACTIVE_WORK_WAITERS", "1")
	t.Setenv("KORA_DB_ACTIVE_WORK_WAIT_MS", "10")
	t.Setenv("KORA_DB_SERVER_MAX_HEAVY_JOBS", "1")
	t.Setenv("KORA_DB_ENGINE_REPLICAS", "1")
	t.Setenv("DB_DSN", "")
}
