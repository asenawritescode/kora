package engine

import (
	"net/http"
	"testing"
	"time"

	knet "github.com/asenawritescode/kora/internal/net"
)

func TestBuildHTTPServerRequiresHandlerAndAddress(t *testing.T) {
	if server, err := BuildHTTPServer(HTTPServerConfig{ListenAddress: ":8080"}); err == nil || server != nil {
		t.Fatalf("BuildHTTPServer without handler = (%v, %v), want validation error", server, err)
	}
	if server, err := BuildHTTPServer(HTTPServerConfig{Handler: http.NotFoundHandler()}); err == nil || server != nil {
		t.Fatalf("BuildHTTPServer without address = (%v, %v), want validation error", server, err)
	}
}

func TestBuildHTTPServerAppliesConfiguredTimeouts(t *testing.T) {
	wantRead, wantWrite, wantIdle := 3*time.Second, 4*time.Second, 5*time.Second
	server, err := BuildHTTPServer(HTTPServerConfig{
		Handler: http.NotFoundHandler(), ListenAddress: ":8080",
		TLS:         knet.TLSConfig{Mode: "manual", Email: "ops@example.test", Domains: []string{"example.test"}},
		ReadTimeout: wantRead, WriteTimeout: wantWrite, IdleTimeout: wantIdle,
	})
	if err != nil {
		t.Fatalf("BuildHTTPServer: %v", err)
	}
	if server.Addr != ":8080" || server.ReadTimeout != wantRead || server.WriteTimeout != wantWrite || server.IdleTimeout != wantIdle {
		t.Fatalf("server listener settings = (%q, %s, %s, %s)", server.Addr, server.ReadTimeout, server.WriteTimeout, server.IdleTimeout)
	}
}
