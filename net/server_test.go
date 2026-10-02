package net

import (
	"errors"
	"net"
	"net/http"
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
