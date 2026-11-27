//go:build qjsscript

package qjs

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// module is the pinned QuickJS reactor (quickjs-ng v0.18.0 plus build/qjs_reactor.c),
// rebuilt from source as described in build/README.md.
//
//go:embed testdata/qjs.wasm
var module []byte

// ErrSourceTooLarge is returned when a source (plus its NUL terminator) does not
// fit the guest input buffer.
var ErrSourceTooLarge = errors.New("qjs: source does not fit the input buffer")

// maxInt32 bounds the length a host call may return to the guest (i32 ABI).
const maxInt32 = math.MaxInt32

// Limits bound every instance of an engine. Zero means "unset" for each field.
type Limits struct {
	HeapBytes  uint32 // QuickJS JS_SetMemoryLimit
	StackBytes uint32 // QuickJS JS_SetMaxStackSize
	MaxPages   uint32 // wazero linear-memory cap, in 64 KiB pages
}

// Handler answers one guest host call. The request and response are JSON text;
// this package never interprets either.
type Handler func(ctx context.Context, request []byte) []byte

// Engine owns one wazero runtime and one compiled QuickJS module. Instances
// created from it share the compiled code but nothing else.
type Engine struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	limits   Limits
}

// NewEngine compiles the QuickJS reactor once under the given limits.
func NewEngine(ctx context.Context, limits Limits) (*Engine, error) {
	rt, err := newRuntime(ctx, limits)
	if err != nil {
		return nil, err
	}
	compiled, err := rt.CompileModule(ctx, module)
	if err != nil {
		// Best effort: the compile error is the one to report.
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("compile: %w", err)
	}
	return &Engine{rt: rt, compiled: compiled, limits: limits}, nil
}

// newRuntime builds the wazero runtime with WASI and the kora_host imports.
func newRuntime(ctx context.Context, limits Limits) (wazero.Runtime, error) {
	cfg := wazero.NewRuntimeConfig().WithCloseOnContextDone(true)
	if limits.MaxPages > 0 {
		cfg = cfg.WithMemoryLimitPages(limits.MaxPages)
	}
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("wasi: %w", err)
	}
	_, err := rt.NewHostModuleBuilder("kora_host").
		NewFunctionBuilder().WithFunc(hostCall).Export("call").
		NewFunctionBuilder().WithFunc(hostTake).Export("take").
		Instantiate(ctx)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("host module: %w", err)
	}
	return rt, nil
}

// Close releases the runtime and every instance it created.
func (e *Engine) Close(ctx context.Context) error {
	return e.rt.Close(ctx)
}

// Instance is one isolated QuickJS runtime. Eval calls on one instance are
// serialised; different instances evaluate independently.
type Instance struct {
	mu        sync.Mutex
	mod       api.Module
	h         Handler
	buf       uint32
	bufCap    uint32
	eval      api.Function
	outputPtr api.Function
	outputLen api.Function
}

// NewInstance creates a fresh QuickJS runtime with the engine's limits. It has no
// handler yet: a host call made before SetHandler throws.
func (e *Engine) NewInstance(ctx context.Context) (*Instance, error) {
	cfg := wazero.NewModuleConfig().WithName("").WithSysWalltime().WithSysNanotime()
	mod, err := e.rt.InstantiateModule(ctx, e.compiled, cfg)
	if err != nil {
		return nil, fmt.Errorf("instantiate: %w", err)
	}
	inst, err := initInstance(ctx, mod, e.limits)
	if err != nil {
		_ = mod.Close(ctx) // best effort: the init error is the one to report
		return nil, err
	}
	return inst, nil
}

// initInstance resolves the reactor exports and runs QuickJS init under the limits.
func initInstance(ctx context.Context, mod api.Module, lim Limits) (*Instance, error) {
	// A reactor may export _initialize (the WASI reactor constructor); call it when present.
	if initFn := mod.ExportedFunction("_initialize"); initFn != nil {
		if _, err := initFn.Call(ctx); err != nil {
			return nil, fmt.Errorf("_initialize: %w", err)
		}
	}
	inst := &Instance{mod: mod}
	for name, dst := range map[string]*api.Function{"eval": &inst.eval, "output": &inst.outputPtr, "output_len": &inst.outputLen} {
		fn, err := exported(mod, name)
		if err != nil {
			return nil, err
		}
		*dst = fn
	}
	if err := runInit(ctx, mod, lim); err != nil {
		return nil, err
	}
	if err := inst.resolveBuffer(ctx, mod); err != nil {
		return nil, err
	}
	return inst, nil
}

// runInit calls the guest init(heap, stack); non-zero means QuickJS could not start.
func runInit(ctx context.Context, mod api.Module, lim Limits) error {
	fn, err := exported(mod, "init")
	if err != nil {
		return err
	}
	res, err := fn.Call(ctx, uint64(lim.HeapBytes), uint64(lim.StackBytes))
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}
	if rc := int32(res[0]); rc != 0 {
		return fmt.Errorf("init returned %d", rc)
	}
	return nil
}

// resolveBuffer caches the guest input buffer address and capacity.
func (i *Instance) resolveBuffer(ctx context.Context, mod api.Module) error {
	bufFn, err := exported(mod, "buffer")
	if err != nil {
		return err
	}
	capFn, err := exported(mod, "buffer_cap")
	if err != nil {
		return err
	}
	bufRes, err := bufFn.Call(ctx)
	if err != nil {
		return fmt.Errorf("buffer: %w", err)
	}
	capRes, err := capFn.Call(ctx)
	if err != nil {
		return fmt.Errorf("buffer_cap: %w", err)
	}
	i.buf = uint32(bufRes[0])
	i.bufCap = uint32(capRes[0])
	return nil
}

// exported looks up a required reactor export.
func exported(mod api.Module, name string) (api.Function, error) {
	fn := mod.ExportedFunction(name)
	if fn == nil {
		return nil, fmt.Errorf("guest export %q missing", name)
	}
	return fn, nil
}

// Close stops and releases the instance.
func (i *Instance) Close(ctx context.Context) error {
	return i.mod.Close(ctx)
}

// SetHandler sets the handler that answers the guest's host calls from the next Eval on.
func (i *Instance) SetHandler(h Handler) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.h = h
}

// noHandlerAnswer makes the guest throw: a host call arrived before SetHandler.
const noHandlerAnswer = `{"throw":"kora host calls are not available"}`

// noHandler answers a host call made before SetHandler.
func noHandler(context.Context, []byte) []byte {
	return []byte(noHandlerAnswer)
}

// Mode is a bit set matching the reactor's eval flags.
type Mode uint32

const (
	// ModeCompileOnly parses the source without running it (a syntax check).
	ModeCompileOnly Mode = 1
	// ModeJSONResult returns JSON.stringify of the completion value, or "" when it has none.
	ModeJSONResult Mode = 2
)

// Result is the outcome of one evaluation that reached QuickJS.
type Result struct {
	Thrown bool   // the script threw; Output is the exception's string form
	Output string // the completion value (see Mode), or the exception text
}

// Eval runs src in this instance. A returned error means the evaluation did not
// reach QuickJS (wrong source size, a closed instance, a cancelled context); a
// script exception is a Result with Thrown set. Callers check ctx.Err() to tell a
// timeout from another failure.
func (i *Instance) Eval(ctx context.Context, src string, mode Mode) (Result, error) {
	if uint64(len(src))+1 > uint64(i.bufCap) {
		return Result{}, ErrSourceTooLarge
	}
	i.mu.Lock()
	defer i.mu.Unlock()

	// QuickJS reads its input up to the length given, but the NUL keeps stale
	// bytes from an earlier, longer eval out of the parser.
	payload := make([]byte, len(src)+1)
	copy(payload, src)
	if !i.mod.Memory().Write(i.buf, payload) {
		return Result{}, errors.New("qjs: write source to guest memory")
	}

	h := i.h
	if h == nil {
		h = noHandler
	}
	evalCtx := context.WithValue(ctx, stateKey{}, &callState{h: h})
	res, err := i.eval.Call(evalCtx, uint64(len(src)), uint64(mode))
	if err != nil {
		return Result{}, fmt.Errorf("eval: %w", err)
	}
	switch rc := int32(res[0]); rc {
	case 0, 1:
		out, err := i.readOutput(ctx)
		return Result{Thrown: rc == 1, Output: out}, err
	case -2:
		return Result{}, errors.New("qjs: instance not initialised")
	default:
		return Result{}, fmt.Errorf("qjs: eval returned %d", rc)
	}
}

// readOutput copies the guest output buffer. The copy is needed: the guest reuses
// its buffer on the next call.
func (i *Instance) readOutput(ctx context.Context) (string, error) {
	ptrRes, err := i.outputPtr.Call(ctx)
	if err != nil {
		return "", fmt.Errorf("output: %w", err)
	}
	lenRes, err := i.outputLen.Call(ctx)
	if err != nil {
		return "", fmt.Errorf("output_len: %w", err)
	}
	n := uint32(lenRes[0])
	if n == 0 {
		return "", nil
	}
	b, ok := i.mod.Memory().Read(uint32(ptrRes[0]), n)
	if !ok {
		return "", errors.New("qjs: read output from guest memory")
	}
	return string(b), nil
}

// callState is the per-Eval host-call state. It travels on the context so the
// shared kora_host functions reach the right handler without globals.
type callState struct {
	h       Handler
	pending []byte
}

// stateKey is the private context key for callState.
type stateKey struct{}

// stateFrom returns the callState of the running Eval, or nil outside one.
func stateFrom(ctx context.Context) *callState {
	st, _ := ctx.Value(stateKey{}).(*callState)
	return st
}

// hostCall is the guest's kora_host.call(ptr, len) -> respLen. It returns -1 when
// the request cannot be read; the guest then throws.
func hostCall(ctx context.Context, m api.Module, ptr, n uint32) int32 {
	st := stateFrom(ctx)
	req, ok := m.Memory().Read(ptr, n)
	if st == nil || !ok {
		return -1
	}
	st.pending = st.h(ctx, bytes.Clone(req))
	if len(st.pending) > maxInt32 {
		st.pending = nil
		return -1
	}
	return int32(len(st.pending))
}

// hostTake is the guest's kora_host.take(dst): it copies the pending response into
// the buffer the guest allocated for it (exactly len(pending) bytes).
func hostTake(ctx context.Context, m api.Module, dst uint32) {
	st := stateFrom(ctx)
	if st == nil {
		return
	}
	// The guest allocated len(pending) bytes at dst for this response, so the
	// write is in bounds. The import is void in the guest ABI, so it cannot report
	// a failure; the guest would see an empty string.
	_ = m.Memory().Write(dst, st.pending)
}
