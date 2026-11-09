package engine

import (
	"context"
	"testing"

	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
)

func TestAssembleHTTPProcessRegistersResourcesBeforeLateRouteFailure(t *testing.T) {
	resources := &ProcessResources{Tenants: knet.NewSiteRouter(nil)}
	_, err := AssembleHTTPProcess(HTTPProcessAssemblyConfig{
		ProcessContext: context.Background(), Resources: resources,
		Application: ApplicationConfig{
			Common:  &site.CommonConfig{DBType: "mysql", AdminRole: "System Manager", SessionLifetimeHours: 1},
			Tenants: resources.Tenants,
		},
		ReloadController: &TenantReloadController{},
		PlatformRoutes:   PlatformRouteConfig{ManagementToken: "configured-but-invalid"},
	})
	if err == nil {
		t.Fatal("AssembleHTTPProcess succeeded with invalid management route configuration")
	}
	if resources.SessionManager == nil || resources.AsyncHookWorkerDone == nil {
		t.Fatalf("late-stage failure did not leave acquired resources with owner: session=%v worker_done=%v", resources.SessionManager, resources.AsyncHookWorkerDone)
	}
	if resources.CloseRealtimeStreams == nil {
		t.Fatal("HTTP process assembly did not register realtime stream shutdown with process owner")
	}
	if err := resources.Shutdown(context.Background()); err != nil {
		t.Fatalf("rollback HTTP process assembly: %v", err)
	}
	select {
	case <-resources.AsyncHookWorkerDone:
	default:
		t.Fatal("rollback returned before async hook worker joined")
	}
	if err := resources.SessionManager.Close(); err != nil {
		t.Fatalf("session-manager close is not idempotent after rollback: %v", err)
	}
}
