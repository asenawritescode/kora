// Package engine owns the running Engine process boundary.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	knet "github.com/asenawritescode/kora/net"
)

var ErrServerRequired = errors.New("engine HTTP server is required")
var ErrAlreadyRun = errors.New("engine Run may only be called once")
var ErrAlreadyShutdown = errors.New("engine cannot run after shutdown has started")
var ErrShutdownIncomplete = errors.New("engine shutdown incomplete; dependent resources retained")

// Engine owns the listener and the assembled runtime's shutdown boundary.
// Runtime construction remains explicit in the composition root.
type Engine struct {
	server      *knet.Server
	onReady     func()
	onStopped   func(error)
	resources   ShutdownOwner
	shutdownMu  sync.Mutex
	stopNotify  sync.Once
	cleanupDone bool
	state       atomic.Uint32
	runStarted  atomic.Bool
}

// NewFromListener wraps an already constructed HTTP server. Production process
// startup should use New so Engine owns configuration assembly and rollback.
func NewFromListener(server *knet.Server, onReady func(), resources ShutdownOwner) *Engine {
	return &Engine{server: server, onReady: onReady, resources: resources}
}

// Run serves until listener failure or termination signal, always joining the
// signal watcher and completing runtime shutdown before returning.
func (e *Engine) Run(ctx context.Context) error {
	if e == nil || e.server == nil {
		return ErrServerRequired
	}
	if !e.runStarted.CompareAndSwap(false, true) {
		return ErrAlreadyRun
	}
	if !e.state.CompareAndSwap(uint32(stateNotStarted), uint32(stateStarting)) {
		return ErrAlreadyShutdown
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if starter, ok := e.resources.(interface{ StartRegisteredWorkers() error }); ok {
		if err := starter.StartRegisteredWorkers(); err != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			shutdownErr := e.Shutdown(shutdownCtx)
			cancel()
			result := errors.Join(err, shutdownErr)
			e.notifyStopped(result)
			return result
		}
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	stop := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case sig := <-sigCh:
			slog.Info("received signal, shutting down gracefully", "signal", sig)
			e.shutdownGracefully()
		case <-ctx.Done():
			e.shutdownGracefully()
		case <-stop:
		}
	}()
	err := e.server.ListenAndServeReady(func() {
		if !e.state.CompareAndSwap(uint32(stateStarting), uint32(stateReady)) {
			return
		}
		if e.onReady != nil {
			e.onReady()
		}
	})
	close(stop)
	signal.Stop(sigCh)
	<-watcherDone
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	shutdownErr := e.Shutdown(shutdownCtx)
	cancel()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	result := errors.Join(err, shutdownErr)
	e.notifyStopped(result)
	return result
}

// Shutdown runs the composed resource cleanup exactly once and joins callers.
func (e *Engine) Shutdown(ctx context.Context) (result error) {
	if e == nil {
		return nil
	}
	defer func() {
		if !e.runStarted.Load() {
			e.notifyStopped(result)
		}
	}()
	e.shutdownMu.Lock()
	defer e.shutdownMu.Unlock()
	e.beginShutdown()
	if ctx == nil {
		ctx = context.Background()
	}
	if e.server != nil {
		if err := e.server.Shutdown(ctx); err != nil {
			e.markShutdownIncomplete()
			return errors.Join(ErrShutdownIncomplete, fmt.Errorf("drain HTTP server: %w", err))
		}
	}
	if e.cleanupDone {
		e.state.Store(uint32(stateStopped))
		return nil
	}
	if e.resources != nil {
		if err := e.resources.Shutdown(ctx); err != nil {
			e.state.Store(uint32(stateShutdownIncomplete))
			return err
		}
	}
	e.cleanupDone = true
	e.state.Store(uint32(stateStopped))
	return nil
}

func (e *Engine) notifyStopped(err error) {
	if e != nil && e.onStopped != nil {
		e.stopNotify.Do(func() { e.onStopped(err) })
	}
}

func (e *Engine) beginShutdown() {
	for {
		current := engineState(e.state.Load())
		if current == stateShuttingDown || current == stateShutdownIncomplete || current == stateStopped {
			return
		}
		if e.state.CompareAndSwap(uint32(current), uint32(stateShuttingDown)) {
			return
		}
	}
}

func (e *Engine) markShutdownIncomplete() {
	for {
		current := engineState(e.state.Load())
		if current == stateStopped || current == stateShutdownIncomplete {
			return
		}
		if e.state.CompareAndSwap(uint32(current), uint32(stateShutdownIncomplete)) {
			return
		}
	}
}

func (e *Engine) shutdownGracefully() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.Shutdown(ctx); err != nil {
		slog.Error("engine shutdown error", "error", err)
	}
}

func (e *Engine) Readiness() Readiness {
	if e == nil {
		return Readiness{Reasons: []string{"not_started"}}
	}
	state := engineState(e.state.Load())
	if state == stateReady {
		return Readiness{Ready: true}
	}
	return Readiness{Reasons: []string{state.reason()}}
}

type engineState uint32

const (
	stateNotStarted engineState = iota
	stateStarting
	stateReady
	stateShuttingDown
	stateShutdownIncomplete
	stateStopped
)

func (s engineState) reason() string {
	switch s {
	case stateStarting:
		return "starting"
	case stateShuttingDown:
		return "shutting_down"
	case stateShutdownIncomplete:
		return "shutdown_incomplete"
	case stateStopped:
		return "stopped"
	default:
		return "not_started"
	}
}

// Readiness reports listener state with bounded, stable reason codes. It
// never includes raw startup or shutdown errors.
type Readiness struct {
	Ready   bool
	Reasons []string
}
