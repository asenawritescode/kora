// Package net provides the Go-native HTTP server with TLS, multi-site routing,
// and all production middleware (security headers, CORS, rate limiting, CSRF).
package net

import (
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// TLSConfig controls TLS behavior.
type TLSConfig struct {
	Mode    string   // "auto" (Let's Encrypt), "manual" (cert files), "off" (HTTP only)
	Domains []string // All domains across all sites (for autocert whitelist)
	Email   string   // Contact email for Let's Encrypt
	CertDir string   // Directory to cache certs (default: "certs")
}

// Server wraps an http.Server with TLS and lifecycle management.
type Server struct {
	*http.Server
	tlsConfig *TLSConfig
}

// NewServer creates a production-ready HTTP server.
func NewServer(handler http.Handler, addr string, cfg *TLSConfig) *Server {
	if cfg == nil {
		cfg = &TLSConfig{Mode: "off"}
	}
	if cfg.CertDir == "" {
		cfg.CertDir = "certs"
	}

	srv := &Server{
		Server: &http.Server{
			Addr:         addr,
			Handler:      handler,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  120 * time.Second,
		},
		tlsConfig: cfg,
	}

	if cfg.Mode == "auto" && len(cfg.Domains) > 0 {
		srv.setupAutocert()
	}

	return srv
}

func (s *Server) setupAutocert() {
	certManager := &autocert.Manager{
		Cache:      autocert.DirCache(s.tlsConfig.CertDir),
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(s.tlsConfig.Domains...),
		Email:      s.tlsConfig.Email,
	}

	s.Server.TLSConfig = &tls.Config{
		GetCertificate: certManager.GetCertificate,
		MinVersion:     tls.VersionTLS13,
	}

	// Start HTTP→HTTPS redirect server on :80.
	go func() {
		slog.Info("starting HTTP→HTTPS redirect on :80")
		redirectHandler := certManager.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			target := "https://" + r.Host + r.URL.Path
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
		}))
		if err := http.ListenAndServe(":80", redirectHandler); err != nil {
			slog.Error("HTTP redirect server failed", "error", err)
		}
	}()

	slog.Info("autocert configured", "domains", s.tlsConfig.Domains, "cert_dir", s.tlsConfig.CertDir)
}

// ListenAndServe starts the server with TLS (if configured) or plain HTTP.
func (s *Server) ListenAndServe() error {
	return s.ListenAndServeReady(nil)
}

// ListenAndServeReady binds the socket before invoking onReady, allowing the
// caller to publish readiness and startup measurements only after the server
// can actually accept connections.
func (s *Server) ListenAndServeReady(onReady func()) error {
	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	slog.Info("server starting", "addr", s.Addr, "tls", s.tlsConfig.Mode)
	if onReady != nil {
		onReady()
	}
	if s.Server.TLSConfig != nil {
		// TLS configured via autocert or manual certs — certFile/keyFile are empty
		// because GetCertificate is set in TLSConfig.
		return s.Server.ServeTLS(listener, "", "")
	}
	return s.Server.Serve(listener)
}
