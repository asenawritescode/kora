package cli

import (
	"database/sql"
	"testing"

	"github.com/asenawritescode/kora/internal/auth"
	"github.com/asenawritescode/kora/internal/engine"
	knet "github.com/asenawritescode/kora/internal/net"
)

func TestPublishedGenerationTakesOwnershipOfSharedTenantResources(t *testing.T) {
	database := &sql.DB{}
	manager := &auth.SessionManager{}
	retired := &knet.LoadedSite{DB: database, SessionManager: manager}
	active := &knet.LoadedSite{DB: database, SessionManager: manager}
	if !engine.TransferStableTenantResources(retired, active) {
		t.Fatal("published generation should take ownership of shared tenant resources")
	}
	if retired.DB != nil || retired.SessionManager != nil || active.DB != database || active.SessionManager != manager {
		t.Fatal("retired generation still owns a shared resource or active handles changed")
	}
	other := &knet.LoadedSite{DB: &sql.DB{}}
	if engine.TransferStableTenantResources(other, active) {
		t.Fatal("distinct DB handles cannot transfer ownership")
	}
}
