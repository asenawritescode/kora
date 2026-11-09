package engine

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/api"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

type shutdownOwnerFunc func(context.Context) error

func (f shutdownOwnerFunc) Shutdown(ctx context.Context) error { return f(ctx) }

func TestEngineShutdownClosesRealtimeWebSocketBeforeJoiningResources(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	owner := api.NewRealtimeConnectionOwner()
	handler := api.NewHandler(nil, nil)
	handler.RealtimeConnections = owner
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlerDone := make(chan struct{})
	router.GET("/realtime", func(c *gin.Context) {
		c.Set("site_name", "engine-shutdown-test")
		handler.HandleSystemRealtime(c)
		close(handlerDone)
	})
	server := knet.NewServer(router, addr, nil)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.ListenAndServeReady(nil) }()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var conn *websocket.Conn
	for {
		conn, _, err = websocket.Dial(ctx, "ws://"+addr+"/realtime", nil)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("connect realtime stream to Engine: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer conn.CloseNow()
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal("read initial realtime event:", err)
	}

	resources := &ProcessResources{CloseRealtimeStreams: owner.Close}
	e := NewFromListener(server, nil, resources)
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := e.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Engine shutdown with active websocket: %v", err)
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("Engine shutdown returned while websocket handler remained active")
	}
	select {
	case err := <-serveDone:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("HTTP serve result = %v, want ErrServerClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP serve goroutine remained active after Engine shutdown")
	}
}

func TestRunBindFailureCleansRuntimeWithoutPublishingReady(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := knet.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), listener.Addr().String(), nil)
	var ready atomic.Bool
	var cleanup atomic.Int32
	var stopped atomic.Int32
	e := NewFromListener(server, func() { ready.Store(true) }, shutdownOwnerFunc(func(context.Context) error { cleanup.Add(1); return nil }))
	e.onStopped = func(stopErr error) {
		if stopErr == nil {
			t.Error("onStopped received nil for listener bind failure")
		}
		stopped.Add(1)
	}
	err = e.Run(context.Background())
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Run error = %v, want bind failure", err)
	}
	if ready.Load() {
		t.Fatal("readiness published before bind")
	}
	if cleanup.Load() != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanup.Load())
	}
	if stopped.Load() != 1 {
		t.Fatalf("onStopped calls = %d, want 1", stopped.Load())
	}
}

func TestShutdownConcurrentCallersJoinSingleCleanup(t *testing.T) {
	var calls atomic.Int32
	e := NewFromListener(nil, nil, shutdownOwnerFunc(func(context.Context) error { calls.Add(1); return nil }))
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.Shutdown(context.Background()); err != nil {
				t.Errorf("Shutdown: %v", err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("cleanup calls = %d, want 1", calls.Load())
	}
}

func TestRunRejectsEngineAfterShutdown(t *testing.T) {
	var cleanups atomic.Int32
	server := knet.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "127.0.0.1:0", nil)
	e := NewFromListener(server, nil, shutdownOwnerFunc(func(context.Context) error {
		cleanups.Add(1)
		return nil
	}))
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before Run: %v", err)
	}
	if err := e.Run(context.Background()); !errors.Is(err, ErrAlreadyShutdown) {
		t.Fatalf("Run after shutdown error = %v, want ErrAlreadyShutdown", err)
	}
	if cleanups.Load() != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanups.Load())
	}
}

func TestShutdownRetriesIncompleteCleanup(t *testing.T) {
	want := errors.New("tenant close failed")
	var calls atomic.Int32
	e := NewFromListener(nil, nil, shutdownOwnerFunc(func(context.Context) error {
		if calls.Add(1) == 1 {
			return errors.Join(ErrShutdownIncomplete, want)
		}
		return nil
	}))
	if err := e.Shutdown(context.Background()); !errors.Is(err, want) {
		t.Fatalf("first Shutdown error = %v, want cleanup failure", err)
	}
	if got := e.Readiness().Reasons; len(got) != 1 || got[0] != "shutdown_incomplete" {
		t.Fatalf("readiness after failed shutdown = %v, want incomplete", got)
	}
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("retry Shutdown: %v", err)
	}
	if got := e.Readiness().Reasons; len(got) != 1 || got[0] != "stopped" {
		t.Fatalf("readiness after successful retry = %v, want stopped", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("cleanup calls = %d, want two attempts", calls.Load())
	}
}

func TestShutdownRetryFinishesTenantDrainAndClosesSharedProviders(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectClose()
	runtime := &knet.LoadedSite{
		SiteID: "tenant-engine-shutdown-retry", Name: "retry.example", DB: database,
		Config: knet.SiteRouterConfig{Hostname: "retry.example"},
	}
	router := knet.NewSiteRouter([]*knet.LoadedSite{runtime})
	handle, err := router.AcquireByID(context.Background(), runtime.SiteID)
	if err != nil {
		t.Fatal(err)
	}
	provider := &testNATSCloser{}
	engine := NewFromListener(nil, nil, &ProcessResources{Tenants: router, NATSProvider: provider})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err = engine.Shutdown(ctx)
	cancel()
	if !errors.Is(err, ErrShutdownIncomplete) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Shutdown error = %v, want incomplete tenant drain", err)
	}
	if provider.closed.Load() != 0 {
		t.Fatal("shared provider closed while tenant request remained admitted")
	}
	if err := database.Ping(); err != nil {
		t.Fatalf("tenant database closed before its request drained: %v", err)
	}

	handle.Release()
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatalf("retry Shutdown after tenant drain: %v", err)
	}
	if provider.closed.Load() != 1 {
		t.Fatalf("shared provider close count = %d, want 1", provider.closed.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunRootCancellationClearsReadinessAndJoins(t *testing.T) {
	ready := make(chan struct{})
	var cleanups atomic.Int32
	e := NewFromListener(knet.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "127.0.0.1:0", nil), func() { close(ready) }, shutdownOwnerFunc(func(context.Context) error { cleanups.Add(1); return nil }))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not become ready")
	}
	if !e.Readiness().Ready {
		t.Fatal("readiness should be published after bind")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after root cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("engine did not join after root cancellation")
	}
	if e.Readiness().Ready {
		t.Fatal("readiness remained true after shutdown")
	}
	if got := e.Readiness().Reasons; len(got) != 1 || got[0] != "stopped" {
		t.Fatalf("readiness reasons after shutdown = %v, want [stopped]", got)
	}
	if cleanups.Load() != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanups.Load())
	}
	if err := e.Run(context.Background()); !errors.Is(err, ErrAlreadyRun) {
		t.Fatalf("second Run error = %v", err)
	}
}

func TestReadinessReasonsAreStableAndDoNotExposeErrors(t *testing.T) {
	wantErr := errors.New("sensitive cleanup detail")
	e := NewFromListener(nil, nil, shutdownOwnerFunc(func(context.Context) error { return wantErr }))
	if got := e.Readiness().Reasons; len(got) != 1 || got[0] != "not_started" {
		t.Fatalf("initial readiness reasons = %v, want [not_started]", got)
	}
	if err := e.Shutdown(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Shutdown error = %v, want cleanup failure", err)
	}
	readiness := e.Readiness()
	if readiness.Ready || len(readiness.Reasons) != 1 || readiness.Reasons[0] != "shutdown_incomplete" {
		t.Fatalf("readiness after failed shutdown = %+v, want stable incomplete reason", readiness)
	}
	if strings.Contains(strings.Join(readiness.Reasons, ","), wantErr.Error()) {
		t.Fatalf("readiness leaked raw cleanup error: %v", readiness.Reasons)
	}
}

func TestShutdownTimeoutDoesNotCloseRuntimeUnderActiveRequest(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	var cleanups atomic.Int32
	server := knet.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "done")
	}), addr, nil)
	e := NewFromListener(server, nil, shutdownOwnerFunc(func(context.Context) error { cleanups.Add(1); return nil }))
	runDone := make(chan error, 1)
	go func() { runDone <- e.Run(context.Background()) }()
	deadline := time.Now().Add(2 * time.Second)
	for !e.Readiness().Ready && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !e.Readiness().Ready {
		t.Fatal("server did not become ready")
	}
	responseDone := make(chan error, 1)
	go func() {
		response, requestErr := http.Get("http://" + addr)
		if requestErr == nil {
			_, requestErr = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		responseDone <- requestErr
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not enter handler")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err = e.Shutdown(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want deadline exceeded", err)
	}
	if !errors.Is(err, ErrShutdownIncomplete) {
		t.Fatalf("Shutdown error = %v, want incomplete-shutdown marker", err)
	}
	if cleanups.Load() != 0 {
		t.Fatal("runtime cleanup ran while an admitted HTTP handler was active")
	}
	close(release)
	select {
	case err := <-responseDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active request did not finish")
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not join after active request drained")
	}
	if cleanups.Load() != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanups.Load())
	}
}

func TestReloadedTenantRequestDrainsBeforeEngineShutdownClosesPools(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	oldDB, oldMock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	newDB, newMock, err := sqlmock.New()
	if err != nil {
		_ = oldDB.Close()
		t.Fatal(err)
	}
	oldMock.ExpectPing()
	oldMock.ExpectPing()
	oldMock.ExpectClose()
	newMock.ExpectClose()
	old := &knet.LoadedSite{
		SiteID: "tenant-engine-test", Name: "engine.example", ConfigRevision: 3, DirectoryRevision: 7,
		DatabaseIdentity: "old-db", DB: oldDB,
		Config: knet.SiteRouterConfig{Hostname: "engine.example", Domains: []string{"engine.example"}},
	}
	router := knet.NewSiteRouter([]*knet.LoadedSite{old})
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	server := knet.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, handle, ok := router.AcquireByName(r.Context(), "engine.example")
		if !ok {
			http.Error(w, "tenant unavailable", http.StatusServiceUnavailable)
			return
		}
		defer handle.Release()
		close(entered)
		<-release
		_, _ = io.WriteString(w, "done")
	}), addr, nil)
	cleanupDone := make(chan error, 1)
	ready := make(chan struct{})
	e := NewFromListener(server, func() { close(ready) }, shutdownOwnerFunc(func(context.Context) error {
		cleanupDone <- router.RuntimeManager().Close(context.Background())
		return nil
	}))
	runDone := make(chan error, 1)
	go func() { runDone <- e.Run(context.Background()) }()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("engine did not become ready")
	}
	responseDone := make(chan error, 1)
	go func() {
		response, requestErr := http.Get("http://" + addr)
		if requestErr == nil {
			_, requestErr = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		responseDone <- requestErr
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not acquire tenant runtime")
	}
	if err := oldDB.Ping(); err != nil {
		t.Fatalf("pre-reload tenant DB ping: %v", err)
	}
	replacement := &knet.LoadedSite{
		SiteID: old.SiteID, Name: old.Name, ConfigRevision: old.ConfigRevision, DirectoryRevision: old.DirectoryRevision + 1,
		DatabaseIdentity: "new-db", DB: newDB, Config: old.Config,
	}
	retired, err := router.ReplaceSite(replacement)
	if err != nil || retired != old {
		t.Fatalf("replace tenant runtime=(%p, %v), want old runtime", retired, err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err = e.Shutdown(shutdownCtx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown with pinned tenant request error=%v, want deadline", err)
	}
	if err := oldDB.Ping(); err != nil {
		t.Fatalf("reload or timed shutdown closed old DB under active request: %v", err)
	}
	unblock()
	select {
	case err := <-responseDone:
		if err != nil {
			t.Fatalf("active HTTP request: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active HTTP request did not drain")
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("engine Run after request drain: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("engine did not join after request drain")
	}
	if err := <-cleanupDone; err != nil {
		t.Fatalf("tenant manager shutdown: %v", err)
	}
	if err := oldMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if err := newMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
