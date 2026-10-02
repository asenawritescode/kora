package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalResourceSegment(t *testing.T) {
	for input, want := range map[string]string{
		"Work Order":        "work-order",
		"  Purchase_Order ": "purchase-order",
		"Guest":             "guest",
	} {
		if got := canonicalResourceSegment(input); got != want {
			t.Fatalf("canonicalResourceSegment(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRemoteProfilesDoNotStoreTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote.json")
	cfg := remoteProfileFile{
		Current: "production",
		Profiles: map[string]remoteProfile{
			"production": {URL: "https://kora.example", Site: "acme", TokenEnv: "ACME_KORA_TOKEN"},
		},
	}
	if err := saveRemoteProfiles(path, cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("profile mode = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Bearer") || strings.Contains(string(data), "secret-token") {
		t.Fatalf("profile unexpectedly contains credential material: %s", data)
	}
	loaded, err := loadRemoteProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Current != "production" || loaded.Profiles["production"].TokenEnv != "ACME_KORA_TOKEN" {
		t.Fatalf("unexpected loaded profile: %#v", loaded)
	}
}

func TestLoadRemoteProfilesAcceptsEmptyExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote.json")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadRemoteProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profiles == nil || len(cfg.Profiles) != 0 {
		t.Fatalf("unexpected profiles: %#v", cfg.Profiles)
	}
}

func TestRemoteClientUsesSitePathAndBearerToken(t *testing.T) {
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"name":"WO-1"}]}`)
	}))
	defer server.Close()

	base, err := validatedRemoteBaseURL(server.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	client := &remoteClient{baseURL: base, site: "acme", token: "secret-token", http: server.Client()}
	var result map[string]any
	if err := client.do(t.Context(), http.MethodGet, "/api/v1/resource/work-order", nil, &result); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/s/acme/api/v1/resource/work-order" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
}

func TestRemoteClientReturnsSafeAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"permission.denied","message":"Token cannot read Work Order"}}`)
	}))
	defer server.Close()
	base, _ := validatedRemoteBaseURL(server.URL, false)
	client := &remoteClient{baseURL: base, token: "secret-token", http: server.Client()}
	err := client.do(t.Context(), http.MethodGet, "/api/v1/resource/work-order", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "Token cannot read Work Order") || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRemoteRecordsCreateCommand(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/s/acme/api/v1/kernel/record.create" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"data":{"name":"WO-1"}}`)
	}))
	defer server.Close()

	t.Setenv("TEST_KORA_TOKEN", "secret-token")
	opts := &remoteOptions{URL: server.URL, Site: "acme", TokenEnv: "TEST_KORA_TOKEN", Timeout: 0}
	cmd := newRemoteWriteCommand(opts, "create <doctype>", "Create", false)
	cmd.SetArgs([]string{"Work Order", "--data", `{"title":"Inspect generator"}`, "--idempotency-key", "create-1"})
	cmd.SetOut(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if gotBody["idempotency_key"] != "create-1" {
		t.Fatalf("body = %#v", gotBody)
	}
	payload, ok := gotBody["payload"].(map[string]any)
	if !ok || payload["doctype"] != "work-order" {
		t.Fatalf("body = %#v", gotBody)
	}
	data, ok := payload["data"].(map[string]any)
	if !ok || data["title"] != "Inspect generator" {
		t.Fatalf("body = %#v", gotBody)
	}
}

func TestValidatedRemoteBaseURLRejectsPlainHTTP(t *testing.T) {
	if _, err := validatedRemoteBaseURL("http://kora.example", false); err == nil {
		t.Fatal("expected non-local HTTP URL to be rejected")
	}
	if _, err := validatedRemoteBaseURL("http://localhost:8000", false); err != nil {
		t.Fatalf("localhost should allow HTTP: %v", err)
	}
}
