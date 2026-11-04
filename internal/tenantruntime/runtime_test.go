package tenantruntime

import (
	"context"
	"errors"
	"testing"
)

func TestHandlePinsImmutableTenantSnapshot(t *testing.T) {
	runtime := &Runtime{
		SiteID: "tenant-17", ConfigRevision: 8, DirectoryRevision: 21,
		DatabaseIdentity: "db-resource-v3",
		Config:           SiteConfig{Hostname: "tenant.example", Domains: []string{"tenant.example"}},
	}
	handle, ok := runtime.AcquireRuntime()
	if !ok {
		t.Fatal("runtime handle acquisition failed")
	}
	defer handle.Release()
	snapshot := handle.Snapshot()
	if snapshot == nil {
		t.Fatal("handle did not capture a tenant snapshot")
	}
	runtime.ConfigRevision = 9
	runtime.DirectoryRevision = 22
	runtime.Config.Domains[0] = "changed.example"
	if snapshot.Activation.Tenant != "tenant-17" || snapshot.Activation.ConfigRevision != "8" || snapshot.Activation.DirectoryRevision != 21 {
		t.Fatalf("snapshot activation changed after publication: %+v", snapshot.Activation)
	}
	if snapshot.Config.Domains[0] != "tenant.example" {
		t.Fatalf("snapshot domains changed: %v", snapshot.Config.Domains)
	}
	if snapshot.Generation != handle.Generation() {
		t.Fatalf("snapshot generation=%d handle generation=%d", snapshot.Generation, handle.Generation())
	}
	if err := runtime.WaitForRequests(context.Background()); !errors.Is(err, ErrGenerationNotRetired) {
		t.Fatalf("wait on active runtime error = %v, want ErrGenerationNotRetired", err)
	}
}
