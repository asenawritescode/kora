package net

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestListenAndServeReadyRunsAfterSuccessfulBind(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), addr, &TLSConfig{Mode: "off"})
	ready := make(chan struct{})
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- srv.ListenAndServeReady(func() {
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				t.Errorf("readiness callback ran before listener accepted connections: %v", err)
			} else {
				_ = conn.Close()
			}
			close(ready)
		})
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not report readiness")
	}
	if err := srv.Close(); err != nil {
		t.Fatal("close server:", err)
	}
	if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("ListenAndServeReady returned %v, want ErrServerClosed", err)
	}
}

func TestServerShutdownOwnsAndJoinsRedirectListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("listen for redirect test:", err)
	}
	srv := NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "127.0.0.1:0", &TLSConfig{Mode: "off"})
	srv.redirectHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+r.Host+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
	if err := srv.startRedirectServer(listener); err != nil {
		t.Fatal("start redirect listener:", err)
	}
	done := srv.redirectDone
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get("http://" + listener.Addr().String() + "/workspace?tenant=acme")
	if err != nil {
		t.Fatal("request redirect listener:", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusMovedPermanently || response.Header.Get("Location") != "https://"+listener.Addr().String()+"/workspace?tenant=acme" {
		t.Fatalf("redirect response status=%d location=%q", response.StatusCode, response.Header.Get("Location"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal("shutdown server and redirect listener:", err)
	}
	select {
	case <-done:
	default:
		t.Fatal("server shutdown returned before redirect serve loop exited")
	}
	_, err = http.Get("http://" + strings.TrimSpace(listener.Addr().String()))
	if err == nil {
		t.Fatal("redirect listener accepted a request after shutdown")
	}
}

func TestListenAndServeReadyDoesNotReportReadinessWhenBindFails(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	srv := NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), listener.Addr().String(), &TLSConfig{Mode: "off"})
	ready := false
	if err := srv.ListenAndServeReady(func() { ready = true }); err == nil {
		t.Fatal("expected bind failure")
	}
	if ready {
		t.Fatal("readiness callback ran even though bind failed")
	}
}
