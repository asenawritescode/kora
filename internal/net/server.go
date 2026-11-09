// Package net provides the Go-native HTTP server with TLS, multi-site routing,
// and all production middleware (security headers, CORS, rate limiting, CSRF).
package net

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
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
	tlsConfig         *TLSConfig
	redirectHandler   http.Handler
	redirectMu        sync.Mutex
	redirectServer    *http.Server
	redirectDone      chan struct{}
	redirectStopErr   error
	redirectStopBegun bool
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

	s.redirectHandler = certManager.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := "https://" + r.Host + r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	}))

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
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.stopRedirect(ctx, false); err != nil {
			slog.Error("HTTP redirect server shutdown failed", "error", err)
		}
	}()
	if s.redirectHandler != nil {
		redirectListener, listenErr := net.Listen("tcp", ":80")
		if listenErr != nil {
			slog.Error("HTTP redirect listener failed", "error", listenErr)
		} else if startErr := s.startRedirectServer(redirectListener); startErr != nil {
			slog.Error("HTTP redirect server failed to start", "error", startErr)
		}
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

// Shutdown gracefully stops the main server and the owned HTTP redirect
// server, joining the latter before returning.
func (s *Server) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("server shutdown requires a context")
	}
	serverErr := s.Server.Shutdown(ctx)
	redirectErr := s.stopRedirect(ctx, false)
	return errors.Join(serverErr, redirectErr)
}

// Close force-closes both listeners. The redirect serve loop is joined before
// returning so callers have one shutdown boundary for the complete server.
func (s *Server) Close() error {
	serverErr := s.Server.Close()
	redirectErr := s.stopRedirect(context.Background(), true)
	return errors.Join(serverErr, redirectErr)
}

func (s *Server) startRedirectServer(listener net.Listener) error {
	if s.redirectHandler == nil {
		_ = listener.Close()
		return nil
	}
	s.redirectMu.Lock()
	if s.redirectServer != nil {
		s.redirectMu.Unlock()
		_ = listener.Close()
		return errors.New("HTTP redirect server already started")
	}
	redirect := &http.Server{Handler: s.redirectHandler, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})
	s.redirectServer = redirect
	s.redirectDone = done
	s.redirectStopBegun = false
	s.redirectMu.Unlock()
	go func() {
		defer close(done)
		if err := redirect.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP redirect server failed", "error", err)
		}
	}()
	return nil
}

func (s *Server) stopRedirect(ctx context.Context, force bool) error {
	s.redirectMu.Lock()
	redirect, done := s.redirectServer, s.redirectDone
	if redirect == nil || s.redirectStopBegun {
		s.redirectMu.Unlock()
		if done != nil {
			<-done
		}
		return s.redirectStopErr
	}
	if ctx == nil && !force {
		s.redirectMu.Unlock()
		return errors.New("redirect server shutdown requires a context")
	}
	s.redirectStopBegun = true
	s.redirectMu.Unlock()
	var err error
	if force {
		err = redirect.Close()
	} else {
		err = redirect.Shutdown(ctx)
		if err != nil {
			err = errors.Join(err, redirect.Close())
		}
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	<-done
	s.redirectMu.Lock()
	s.redirectStopErr = err
	s.redirectMu.Unlock()
	return s.redirectStopErr
}
