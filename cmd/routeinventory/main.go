// Command routeinventory records the routes registered by the Engine's public
// API composition. It uses the same route registration functions as the
// running server and keeps CLI-conditional routes explicit in the artifact.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/auth"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/gin-gonic/gin"
)

type route struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Handler string `json:"handler,omitempty"`
}

type conditionalRoute struct {
	Method    string `json:"method"`
	Path      string `json:"path"`
	Condition string `json:"condition"`
	Source    string `json:"source"`
}

type fallback struct {
	Behavior  string `json:"behavior"`
	Condition string `json:"condition"`
	Source    string `json:"source"`
}

type inventory struct {
	Format           int                `json:"format"`
	Generator        string             `json:"generator"`
	RouteComposition string             `json:"route_composition"`
	RegisteredRoutes []route            `json:"registered_routes"`
	Conditional      []conditionalRoute `json:"conditional_routes"`
	Fallbacks        []fallback         `json:"fallback_routes"`
}

func main() {
	write := flag.Bool("write", false, "write the generated inventory")
	check := flag.Bool("check", false, "fail if the committed inventory is stale")
	flag.Parse()
	if *write == *check {
		fatal(errors.New("choose exactly one of -write or -check"))
	}

	root := repositoryRoot()
	path := filepath.Join(root, "docs/architecture/route-inventory.json")
	want, err := render()
	if err != nil {
		fatal(err)
	}
	if *write {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			fatal(err)
		}
		fmt.Printf("wrote %s\n", path)
		return
	}
	have, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}
	if !bytes.Equal(have, want) {
		fatal(fmt.Errorf("route inventory is stale: run go run ./cmd/routeinventory -write"))
	}
	fmt.Printf("route inventory is current: %s\n", path)
}

func repositoryRoot() string {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		fatal(errors.New("cannot locate route inventory source"))
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
}

func render() ([]byte, error) {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	auth.RegisterAuthRoutes(router, nil, nil, nil)
	tx := &orm.TxManager{}
	for _, prefix := range []string{"/api/v1", "/api"} {
		api.RegisterRoutesOnGroupWithRuntimeServices(router.Group(prefix), nil, tx, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		api.RegisterPublicRoutesOnGroupWithRuntimeServices(router.Group(prefix), nil, tx, nil, nil)
	}
	api.RegisterRequestRoutes(router.Group("/api/v2"), nil)

	routes := router.Routes()
	registered := make([]route, 0, len(routes))
	for _, current := range routes {
		registered = append(registered, route{
			Method:  current.Method,
			Path:    current.Path,
			Handler: current.Handler,
		})
	}
	sort.Slice(registered, func(i, j int) bool {
		if registered[i].Path != registered[j].Path {
			return registered[i].Path < registered[j].Path
		}
		if registered[i].Method != registered[j].Method {
			return registered[i].Method < registered[j].Method
		}
		return registered[i].Handler < registered[j].Handler
	})
	for i := 1; i < len(registered); i++ {
		if registered[i].Path == registered[i-1].Path && registered[i].Method == registered[i-1].Method {
			return nil, fmt.Errorf("duplicate registered route: %s %s", registered[i].Method, registered[i].Path)
		}
	}

	result := inventory{
		Format:           1,
		Generator:        "cmd/routeinventory",
		RouteComposition: "cli/serve.go API/auth/public/request registration functions; DocType and host routing are data/runtime dependent",
		RegisteredRoutes: registered,
		Conditional: []conditionalRoute{
			{Method: "POST", Path: "/_kora/admin/reload-site", Condition: "always mounted; requires KORA_RELOAD_TOKEN at request time", Source: "cli/serve.go"},
			{Method: "GET", Path: "/api/v1/ping", Condition: "always mounted", Source: "cli/serve.go"},
			{Method: "GET", Path: "/api/ping", Condition: "always mounted", Source: "cli/serve.go"},
			{Method: "GET", Path: "/health", Condition: "always mounted", Source: "cli/serve.go"},
			{Method: "GET", Path: "/readyz", Condition: "always mounted", Source: "cli/serve.go"},
			{Method: "POST", Path: "/api/console/login", Condition: "system guard configured", Source: "cli/serve.go"},
			{Method: "POST", Path: "/api/console/change-password", Condition: "system guard configured", Source: "cli/serve.go"},
			{Method: "GET", Path: "/api/console/sites", Condition: "system guard configured", Source: "cli/serve.go"},
			{Method: "PUT", Path: "/api/console/sites/:name", Condition: "system guard configured", Source: "cli/serve.go"},
			{Method: "DELETE", Path: "/api/console/sites/:name", Condition: "system guard configured", Source: "cli/serve.go"},
			{Method: "GET", Path: "/api/management/v2/tenants/:id", Condition: "KORA_ENGINE_MANAGEMENT_TOKEN and valid KORA_ENGINE_DEPLOYMENT_ID configured", Source: "api/management_inspect.go; cli/serve.go"},
			{Method: "GET", Path: "/api/management/v2/tenants/resolve", Condition: "KORA_ENGINE_MANAGEMENT_TOKEN and valid KORA_ENGINE_DEPLOYMENT_ID configured", Source: "api/management_inspect.go; cli/serve.go"},
		},
		Fallbacks: []fallback{
			{Behavior: "Serve SPA for /workspace, /console, /assets and /s/:site/*; unmatched paths return 404", Condition: "embedded SPA index.html is present", Source: "workspace/spa.go; cli/serve.go"},
			{Behavior: "Register server-rendered /workspace routes and path-site routing", Condition: "embedded SPA index.html is absent", Source: "workspace/handler.go; net/site_router.go; cli/serve.go"},
		},
	}
	for i := range result.Conditional {
		result.Conditional[i].Path = filepath.ToSlash(result.Conditional[i].Path)
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
