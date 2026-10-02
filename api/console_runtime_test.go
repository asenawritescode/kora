package api

import (
	"errors"
	"testing"

	"github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/site"
)

func TestPrepareCreatedRuntimeLoadsDirectoryRevisionBeforePublication(t *testing.T) {
	cfg := &site.SiteConfig{
		Hostname: "new-site.example", DBType: "postgres", DBHost: "db.internal", DBPort: 5432,
		DBName: "new_site", DBUser: "engine", DBPassword: "secret", DomainsList: []string{"new-site.example", "alias.example"},
		FileStorage: "local",
	}
	loaded := &net.LoadedSite{Name: cfg.Hostname}
	called := false
	handler := &ConsoleHandler{PrepareRuntime: func(info site.DBSiteInfo, runtime *net.LoadedSite) error {
		called = true
		if info.SiteID != "site-created" || info.ConfigRevision != 1 || info.Status != "active" {
			t.Fatalf("prepared with incomplete directory info: %#v", info)
		}
		if info.DBPassword != "secret" {
			t.Fatal("internal runtime descriptor should contain credentials needed to construct its DB config")
		}
		runtime.RuntimeServices = "installed"
		return nil
	}}
	if err := handler.prepareCreatedRuntime(loaded, "site-created", cfg); err != nil {
		t.Fatal(err)
	}
	if !called || loaded.ConfigRevision != 1 || loaded.Status != "active" || loaded.RuntimeServices != "installed" {
		t.Fatalf("runtime was not prepared before publication: called=%v loaded=%#v", called, loaded)
	}
}

func TestPrepareCreatedRuntimeDiscardsPartiallyInstalledServicesOnFailure(t *testing.T) {
	loaded := &net.LoadedSite{Name: "new-site.example"}
	discarded := false
	handler := &ConsoleHandler{
		PrepareRuntime: func(site.DBSiteInfo, *net.LoadedSite) error { return errors.New("sidecar failed") },
		DiscardRuntime: func(runtime *net.LoadedSite) {
			discarded = runtime == loaded
		},
	}
	if err := handler.prepareCreatedRuntime(loaded, "site-created", &site.SiteConfig{Hostname: loaded.Name}); err == nil {
		t.Fatal("expected runtime preparation failure")
	}
	if !discarded {
		t.Fatal("partially installed services were not discarded")
	}
}
