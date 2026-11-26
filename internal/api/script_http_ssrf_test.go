package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/netguard"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/script"
)

func newHTTPTestProvider(allowlist []string) *scriptProvider {
	return NewScriptProvider(&orm.TxManager{}, doctype.NewRegistry(), "tenant-a", nil, allowlist).(*scriptProvider)
}

// dialFixedTarget ignores the requested address and dials target instead, so a
// request to an allowlisted public hostname reaches a loopback httptest server.
// This is required because httptest binds to 127.0.0.1, which the allowlist and
// the PublicDialer always refuse.
func dialFixedTarget(target string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
}

// TestScriptHTTPBlocksLoopback proves the dialer NewScriptProvider installs
// refuses a connection to a loopback address at connect time, which closes
// DNS-rebinding (a public name that re-resolves to 127.0.0.1).
func TestScriptHTTPBlocksLoopback(t *testing.T) {
	provider := newHTTPTestProvider([]string{"*"})
	defer provider.Close()
	transport, ok := provider.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("script provider HTTP transport has unexpected type")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	conn, err := transport.DialContext(context.Background(), "tcp", listener.Addr().String())
	if err == nil {
		_ = conn.Close()
		t.Fatal("script dialer connected to a loopback listener")
	}
	if !errors.Is(err, netguard.ErrDisallowedDestination) {
		t.Fatalf("dial error = %v, want ErrDisallowedDestination", err)
	}
}

// TestScriptHTTPBlocksUserinfo proves the allowlist parses the real host, so a
// userinfo prefix cannot smuggle a loopback target past the check even when the
// allowlist is "*".
func TestScriptHTTPBlocksUserinfo(t *testing.T) {
	provider := newHTTPTestProvider([]string{"*"})
	defer provider.Close()
	_, err := provider.DoHTTP(&script.HTTPRequest{URL: "http://allowed.example.com@127.0.0.1/"})
	if err == nil {
		t.Fatal("userinfo-prefixed loopback URL was not refused")
	}
	if !strings.Contains(err.Error(), "private/internal") {
		t.Fatalf("error = %v, want a private/internal host refusal", err)
	}
}

// TestScriptHTTPBlocksRedirect proves every redirect hop is re-validated against
// the allowlist: a 302 from an allowlisted host to a loopback or non-allowlisted
// host is refused.
func TestScriptHTTPBlocksRedirect(t *testing.T) {
	tests := []struct {
		name     string
		location string
	}{
		{"to-loopback", "http://127.0.0.1:9/internal"},
		{"to-unlisted", "http://evil.example.com/steal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Redirect(w, &http.Request{}, tt.location, http.StatusFound)
			}))
			defer server.Close()

			provider := newHTTPTestProvider([]string{"public.example.com"})
			defer provider.Close()
			provider.httpClient.Transport = &http.Transport{
				DialContext: dialFixedTarget(strings.TrimPrefix(server.URL, "http://")),
			}
			_, err := provider.DoHTTP(&script.HTTPRequest{URL: "http://public.example.com/start"})
			if err == nil {
				t.Fatal("redirect to a disallowed host was followed")
			}
		})
	}
}

// TestScriptHTTPAllowsAllowlisted proves a normal request to an allowlisted
// public host still succeeds.
func TestScriptHTTPAllowsAllowlisted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	provider := newHTTPTestProvider([]string{"public.example.com"})
	defer provider.Close()
	provider.httpClient.Transport = &http.Transport{
		DialContext: dialFixedTarget(strings.TrimPrefix(server.URL, "http://")),
	}
	resp, err := provider.DoHTTP(&script.HTTPRequest{URL: "http://public.example.com/data"})
	if err != nil {
		t.Fatalf("allowlisted request failed: %v", err)
	}
	if resp.Status != http.StatusOK || string(resp.Body) != "ok" {
		t.Fatalf("response status=%d body=%q, want 200 \"ok\"", resp.Status, resp.Body)
	}
}
