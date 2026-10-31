package ingress

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestGatewayForwardsHostAndPathRoutesToAssignedCell(t *testing.T) {
	type observed struct {
		Path  string `json:"path"`
		Host  string `json:"host"`
		Query string `json:"query"`
		Hop   string `json:"hop"`
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(observed{Path: r.URL.Path, Host: r.Host, Query: r.URL.RawQuery, Hop: r.Header.Get(ingressHopHeader)})
	}))
	defer upstream.Close()

	router, err := New("cell-a", map[string]string{"cell-b": upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := contract.SiteDescriptor{SiteID: "site-1", Aliases: []string{"shop.example.test", "store.example.test"}, Status: "active", RuntimeCellID: "cell-b"}
	router.ReplaceDirectory([]contract.SiteDescriptor{descriptor})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := router.Wrap(next)

	for _, test := range []struct {
		name, target, host, path, query string
	}{
		{"canonical host", "/api/v1/ping?source=host", "SHOP.EXAMPLE.TEST:8443", "/api/v1/ping", "source=host"},
		{"path selected alias", "/s/store.example.test/api/v1/ping?source=path", "ingress.example.test", "/s/store.example.test/api/v1/ping", "source=path"},
		{"path selected stable site id", "/s/site-1/api/v1/ping?source=site-id", "ingress.example.test", "/s/site-1/api/v1/ping", "source=site-id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, test.target, nil)
			request.Host = test.host
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
			}
			var got observed
			if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
				t.Fatal("decode upstream request:", err)
			}
			if got.Path != test.path || got.Query != test.query {
				t.Fatalf("upstream path/query = %q/%q, want %q/%q", got.Path, got.Query, test.path, test.query)
			}
			if got.Host != test.host {
				t.Fatalf("upstream Host = %q, want original %q", got.Host, test.host)
			}
			if got.Hop != "1" {
				t.Fatalf("upstream ingress hop = %q, want gateway marker", got.Hop)
			}
		})
	}
}

func TestGatewayAllowsOneForwardAndRejectsSecondForward(t *testing.T) {
	var gatewayA, gatewayB *Gateway
	engineA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayA.Wrap(http.NotFoundHandler()).ServeHTTP(w, r)
	}))
	defer engineA.Close()
	engineB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayB.Wrap(http.NotFoundHandler()).ServeHTTP(w, r)
	}))
	defer engineB.Close()

	var err error
	gatewayA, err = New("cell-a", map[string]string{"cell-b": engineB.URL})
	if err != nil {
		t.Fatal("configure gateway A:", err)
	}
	gatewayB, err = New("cell-b", map[string]string{"cell-a": engineA.URL, "cell-c": "http://127.0.0.1:9003"})
	if err != nil {
		t.Fatal("configure gateway B:", err)
	}
	gatewayA.ReplaceDirectory([]contract.SiteDescriptor{{SiteID: "site-a", Aliases: []string{"loop.example.test"}, Status: "active", RuntimeCellID: "cell-b"}})
	gatewayB.ReplaceDirectory([]contract.SiteDescriptor{{SiteID: "site-b", Aliases: []string{"loop.example.test"}, Status: "active", RuntimeCellID: "cell-c"}})

	request, err := http.NewRequest(http.MethodGet, engineA.URL+"/api/v1/ping", nil)
	if err != nil {
		t.Fatal("build request:", err)
	}
	request.Host = "loop.example.test"
	response, err := engineA.Client().Do(request)
	if err != nil {
		t.Fatal("request through ingress:", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusLoopDetected {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("response = %d %s, want loop rejection", response.StatusCode, body)
	}
}

func TestGatewayStripsForwardingMarkerBeforeLocalHandler(t *testing.T) {
	router, err := New("cell-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	router.ReplaceDirectory([]contract.SiteDescriptor{{SiteID: "site-a", Aliases: []string{"local.example.test"}, Status: "active", RuntimeCellID: "cell-a"}})
	router.MarkLocalReady("site-a", true)
	handler := router.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ingressHopHeader) != "" {
			t.Errorf("forwarding marker leaked to local handler")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	request.Host = "local.example.test"
	request.Header.Set(ingressHopHeader, "1")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want local handler", recorder.Code)
	}
}

func TestGatewayPublishesV2AssignmentAtomically(t *testing.T) {
	router, err := New("deployment-a", nil)
	if err != nil {
		t.Fatal("configure gateway:", err)
	}
	valid := contract.SiteDescriptor{
		SiteID: "site_0123456789abcdef0123456789abcdef", Aliases: []string{"tenant.example.test"},
		Status: "active", RuntimeCellID: "deployment-a", ConfigRevision: 3, DirectoryRevision: 9,
	}
	if err := router.ReplaceDirectory([]contract.SiteDescriptor{valid}); err != nil {
		t.Fatal("publish valid directory:", err)
	}
	entry := router.snapshot.Load().byID[valid.SiteID]
	if entry.Version != 2 || entry.DeploymentID != "deployment-a" || entry.ConfigRevision != "3" || entry.DirectoryRevision != "9" {
		t.Fatalf("gateway stored v2 entry = %#v", entry)
	}

	invalid := valid
	invalid.RuntimeCellID = "bad/deployment"
	if err := router.ReplaceDirectory([]contract.SiteDescriptor{invalid}); err == nil {
		t.Fatal("invalid replacement unexpectedly succeeded")
	}
	if got := router.snapshot.Load().byID[valid.SiteID]; got.DeploymentID != "deployment-a" {
		t.Fatalf("invalid replacement partially changed active snapshot: %#v", got)
	}
	if err := router.ApplyDescriptor(invalid); err == nil {
		t.Fatal("invalid feed update unexpectedly succeeded")
	}
	if got := router.snapshot.Load().byID[valid.SiteID]; got.DeploymentID != "deployment-a" {
		t.Fatalf("invalid feed update changed active snapshot: %#v", got)
	}
}

// TestGatewayAgainstLiveEngine is an opt-in acceptance check for the real
// Engine HTTP stack. Set KORA_LIVE_ENGINE_URL and KORA_LIVE_ENGINE_ALIAS to
// route a harmless ping through this gateway to a running Engine process.
func TestGatewayAgainstLiveEngine(t *testing.T) {
	engineURL := os.Getenv("KORA_LIVE_ENGINE_URL")
	alias := os.Getenv("KORA_LIVE_ENGINE_ALIAS")
	if engineURL == "" || alias == "" {
		t.Skip("set KORA_LIVE_ENGINE_URL and KORA_LIVE_ENGINE_ALIAS for live acceptance")
	}
	router, err := New("acceptance-gateway", map[string]string{"acceptance-engine": engineURL})
	if err != nil {
		t.Fatal("configure gateway:", err)
	}
	router.ReplaceDirectory([]contract.SiteDescriptor{{
		SiteID: "acceptance-site", Aliases: []string{alias}, Status: "active", RuntimeCellID: "acceptance-engine",
	}})
	gateway := httptest.NewServer(router.Wrap(http.NotFoundHandler()))
	defer gateway.Close()

	request, err := http.NewRequest(http.MethodGet, gateway.URL+"/api/v1/ping", nil)
	if err != nil {
		t.Fatal("build live gateway request:", err)
	}
	request.Host = alias
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatal("request through live gateway:", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal("read live Engine response:", err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"message":"pong"`) {
		t.Fatalf("live Engine response = %d %s, want HTTP 200 pong", response.StatusCode, body)
	}
}

func TestGatewayKeepsLocalSitesAndUnknownRequestsOnEngine(t *testing.T) {
	router, err := New("cell-a", map[string]string{"cell-b": "http://127.0.0.1:9002"})
	if err != nil {
		t.Fatal(err)
	}
	router.ReplaceDirectory([]contract.SiteDescriptor{
		{SiteID: "local", Aliases: []string{"local.example.test"}, Status: "active", RuntimeCellID: "cell-a"},
		{SiteID: "remote", Aliases: []string{"remote.example.test"}, Status: "active", RuntimeCellID: "cell-b"},
	})
	router.MarkLocalReady("local", true)
	calls := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	})
	handler := router.Wrap(next)
	for _, host := range []string{"local.example.test", "unknown.example.test"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
		request.Host = host
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("host %s status = %d, want Engine handler", host, recorder.Code)
		}
	}
	if calls != 2 {
		t.Fatalf("Engine handler calls = %d, want 2", calls)
	}
}

func TestGatewayFailsClosedOnAmbiguousAliasUnavailableCellAndLifecycle(t *testing.T) {
	router, err := New("cell-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	router.ReplaceDirectory([]contract.SiteDescriptor{
		{SiteID: "one", Aliases: []string{"same.example.test"}, Status: "active", RuntimeCellID: "cell-b"},
		{SiteID: "two", Aliases: []string{"same.example.test"}, Status: "active", RuntimeCellID: "cell-b"},
		{SiteID: "paused", Aliases: []string{"paused.example.test"}, Status: "suspended", RuntimeCellID: "cell-a"},
	})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, test := range []struct {
		host string
		want int
	}{
		{"same.example.test", http.StatusConflict},
		{"paused.example.test", http.StatusServiceUnavailable},
		{"unconfigured.example.test", http.StatusNoContent},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
		request.Host = test.host
		router.Wrap(next).ServeHTTP(recorder, request)
		if recorder.Code != test.want {
			t.Errorf("host %s status = %d, want %d", test.host, recorder.Code, test.want)
		}
	}
	if err := router.ValidateDirectory([]contract.SiteDescriptor{{SiteID: "one", Status: "active", RuntimeCellID: "cell-b"}}); err == nil {
		t.Fatal("ValidateDirectory succeeded without a remote Engine endpoint")
	}
}

func TestParseCellURLsAndSnapshotRefresh(t *testing.T) {
	urls, err := ParseCellURLs("default=http://127.0.0.1:8000, CELL-B=https://engine-b.example.test")
	if err != nil {
		t.Fatal("ParseCellURLs:", err)
	}
	router, err := New("cell-a", urls)
	if err != nil {
		t.Fatal("New:", err)
	}
	if err := router.ValidateDirectory([]contract.SiteDescriptor{{SiteID: "legacy", Aliases: []string{"legacy.example.test"}, Status: "active"}}); err != nil {
		t.Fatal("default cell endpoint not accepted:", err)
	}
	if router.SnapshotAge() < 0 {
		t.Fatal("empty snapshot age must not be negative")
	}
	for _, invalid := range []string{"cell-a=ftp://engine", "cell-a=https://user:pass@engine", "cell-a=https://engine/path", "cell-a=http://engine, CELL-A=http://other"} {
		if _, err := ParseCellURLs(invalid); err == nil {
			t.Errorf("ParseCellURLs(%q) succeeded", invalid)
		}
	}
}
