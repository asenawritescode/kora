package engine

import (
	"errors"
	"net/http"
	"time"

	knet "github.com/asenawritescode/kora/net"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// HTTPServerConfig contains the HTTP listener settings assembled by Engine.
type HTTPServerConfig struct {
	Handler       http.Handler
	ListenAddress string
	TLS           knet.TLSConfig
	ReadTimeout   time.Duration
	WriteTimeout  time.Duration
	IdleTimeout   time.Duration
}

// BuildHTTPServer creates the production Engine listener with request tracing
// and only overrides the established defaults when a timeout is configured.
func BuildHTTPServer(config HTTPServerConfig) (*knet.Server, error) {
	if config.Handler == nil {
		return nil, errors.New("Engine HTTP handler is required")
	}
	if config.ListenAddress == "" {
		return nil, errors.New("Engine HTTP listen address is required")
	}
	server := knet.NewServer(otelhttp.NewHandler(config.Handler, "kora-engine"), config.ListenAddress, &config.TLS)
	if config.ReadTimeout > 0 {
		server.ReadTimeout = config.ReadTimeout
	}
	if config.WriteTimeout > 0 {
		server.WriteTimeout = config.WriteTimeout
	}
	if config.IdleTimeout > 0 {
		server.IdleTimeout = config.IdleTimeout
	}
	return server, nil
}
