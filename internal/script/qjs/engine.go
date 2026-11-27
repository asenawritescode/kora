package qjs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
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

// BuildID identifies the reactor build. Bytecode from Compile is only valid for the
// build that produced it, so callers key any cache of bytecode by this value.
func BuildID() [sha256.Size]byte {
	return buildID
}

var buildID = sha256.Sum256(module)

// ErrSourceTooLarge is returned when a source does not fit the guest's i32 length ABI.
var ErrSourceTooLarge = errors.New("qjs: source exceeds the i32 length limit")

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

// compileCache keeps the compiled QuickJS module in memory across every runtime in the
// process, so only the first engine pays for compilation. It is in-memory only.
var compileCache = wazero.NewCompilationCache()

// newRuntime builds the wazero runtime with WASI and the kora_host imports.
func newRuntime(ctx context.Context, limits Limits) (wazero.Runtime, error) {
	cfg := wazero.NewRuntimeConfig().WithCloseOnContextDone(true).WithCompilationCache(compileCache)
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

// wasmPageBytes is the wasm linear-memory page size.
const wasmPageBytes = 64 << 10

// snapshotGlobals are the mutable wasm globals a snapshot must carry. The reactor keeps
// its shadow stack pointer in a global, outside linear memory.
var snapshotGlobals = []string{"__stack_pointer"}

// Snapshot is the state of one initialised instance: its whole linear memory and the
// mutable globals in snapshotGlobals. It is never mutated after Snapshot returns, so
// any number of instances can be restored from it.
type Snapshot struct {
	memory  []byte
	globals map[string]uint64
}

// Close releases the runtime and every instance it created.
func (e *Engine) Close(ctx context.Context) error {
	return e.rt.Close(ctx)
}

// Instance is one isolated QuickJS runtime. Eval calls on one instance are
// serialised; different instances evaluate independently.
type Instance struct {
	mu           sync.Mutex
	mod          api.Module
	h            Handler
	eval         api.Function
	compile      api.Function
	evalBytecode api.Function
	reserve      api.Function
	outputPtr    api.Function
	outputLen    api.Function
}

// NewInstance creates a fresh QuickJS runtime with the engine's limits. It has no
// handler yet: a host call made before SetHandler throws.
func (e *Engine) NewInstance(ctx context.Context) (*Instance, error) {
	mod, err := e.instantiate(ctx)
	if err != nil {
		return nil, err
	}
	inst, err := initInstance(ctx, mod, e.limits)
	if err != nil {
		_ = mod.Close(ctx) // best effort: the init error is the one to report
		return nil, err
	}
	return inst, nil
}

// instantiate creates a wasm module instance from the compiled reactor, without running it.
func (e *Engine) instantiate(ctx context.Context) (api.Module, error) {
	cfg := wazero.NewModuleConfig().WithName("").WithSysWalltime().WithSysNanotime()
	mod, err := e.rt.InstantiateModule(ctx, e.compiled, cfg)
	if err != nil {
		return nil, fmt.Errorf("instantiate: %w", err)
	}
	return mod, nil
}

// initInstance resolves the reactor exports and runs QuickJS init under the limits.
func initInstance(ctx context.Context, mod api.Module, lim Limits) (*Instance, error) {
	// A reactor may export _initialize (the WASI reactor constructor); call it when present.
	if initFn := mod.ExportedFunction("_initialize"); initFn != nil {
		if _, err := initFn.Call(ctx); err != nil {
			return nil, fmt.Errorf("_initialize: %w", err)
		}
	}
	inst, err := bindInstance(mod)
	if err != nil {
		return nil, err
	}
	if err := runInit(ctx, mod, lim); err != nil {
		return nil, err
	}
	return inst, nil
}

// bindInstance resolves the reactor exports an Instance calls.
func bindInstance(mod api.Module) (*Instance, error) {
	inst := &Instance{mod: mod}
	exports := map[string]*api.Function{
		"eval":           &inst.eval,
		"compile":        &inst.compile,
		"eval_bytecode":  &inst.evalBytecode,
		"buffer_reserve": &inst.reserve,
		"output":         &inst.outputPtr,
		"output_len":     &inst.outputLen,
	}
	for name, dst := range exports {
		fn, err := exported(mod, name)
		if err != nil {
			return nil, err
		}
		*dst = fn
	}
	return inst, nil
}

// Snapshot captures the instance's linear memory and snapshotGlobals. Take it from an
// instance that has finished its setup and is idle; Restore resumes from it without
// running init again.
func (i *Instance) Snapshot() (*Snapshot, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	mem := i.mod.Memory()
	view, ok := mem.Read(0, mem.Size())
	if !ok {
		return nil, errors.New("qjs: read guest memory for snapshot")
	}
	snap := &Snapshot{memory: bytes.Clone(view), globals: make(map[string]uint64, len(snapshotGlobals))}
	for _, name := range snapshotGlobals {
		g := i.mod.ExportedGlobal(name)
		if g == nil {
			return nil, fmt.Errorf("qjs: guest global %q is not exported", name)
		}
		snap.globals[name] = g.Get()
	}
	return snap, nil
}

// Restore creates an instance from a snapshot instead of running init: the snapshot
// already holds the initialised runtime. The instance gets its own Math.random seed,
// because the snapshot copies the generator state.
func (e *Engine) Restore(ctx context.Context, snap *Snapshot) (*Instance, error) {
	mod, err := e.instantiate(ctx)
	if err != nil {
		return nil, err
	}
	inst, err := restoreInstance(ctx, mod, snap)
	if err != nil {
		_ = mod.Close(ctx) // best effort: the restore error is the one to report
		return nil, err
	}
	return inst, nil
}

// restoreInstance writes the snapshot into a fresh module instance and reseeds it.
func restoreInstance(ctx context.Context, mod api.Module, snap *Snapshot) (*Instance, error) {
	mem := mod.Memory()
	size := uint32(len(snap.memory))
	if mem.Size() > size {
		return nil, errors.New("qjs: snapshot is smaller than the module memory")
	}
	if grow := (size - mem.Size()) / wasmPageBytes; grow > 0 {
		if _, ok := mem.Grow(grow); !ok {
			return nil, errors.New("qjs: grow guest memory for snapshot")
		}
	}
	if !mem.Write(0, snap.memory) {
		return nil, errors.New("qjs: write snapshot to guest memory")
	}
	for name, v := range snap.globals {
		g, ok := mod.ExportedGlobal(name).(api.MutableGlobal)
		if !ok {
			return nil, fmt.Errorf("qjs: guest global %q is not mutable", name)
		}
		g.Set(v)
	}
	inst, err := bindInstance(mod)
	if err != nil {
		return nil, err
	}
	if err := reseedRandom(ctx, mod); err != nil {
		return nil, err
	}
	return inst, nil
}

// reseedRandom gives the restored runtime a fresh Math.random sequence.
func reseedRandom(ctx context.Context, mod api.Module) error {
	var seed [8]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return fmt.Errorf("qjs: random seed: %w", err)
	}
	fn, err := exported(mod, "reseed")
	if err != nil {
		return err
	}
	_, err = fn.Call(ctx,
		uint64(binary.LittleEndian.Uint32(seed[:4])),
		uint64(binary.LittleEndian.Uint32(seed[4:])))
	if err != nil {
		return fmt.Errorf("reseed: %w", err)
	}
	return nil
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
	return i.call(ctx, i.eval, src, uint64(mode))
}

// Compile parses src as a global script without running it, and returns its bytecode.
// A syntax error is a Result with Thrown set and no bytecode. The bytecode runs on any
// instance of this build through EvalBytecode.
func (i *Instance) Compile(ctx context.Context, src string) ([]byte, Result, error) {
	res, err := i.call(ctx, i.compile, src)
	if err != nil || res.Thrown {
		return nil, res, err
	}
	return []byte(res.Output), res, nil
}

// EvalBytecode runs bytecode that Compile produced on this build, with the same mode
// and result semantics as Eval. The bytecode is trusted: it comes only from Compile.
func (i *Instance) EvalBytecode(ctx context.Context, code []byte, mode Mode) (Result, error) {
	return i.call(ctx, i.evalBytecode, string(code), uint64(mode))
}

// call writes src into the guest input buffer and calls one reactor entry point on it
// with the given flags. The entry points share the answer protocol of eval.
func (i *Instance) call(ctx context.Context, fn api.Function, src string, flags ...uint64) (Result, error) {
	if uint64(len(src)) > maxInt32-1 {
		return Result{}, ErrSourceTooLarge
	}
	i.mu.Lock()
	defer i.mu.Unlock()

	buf, err := i.reserveBuffer(ctx, uint32(len(src)))
	if err != nil {
		return Result{}, err
	}
	// QuickJS reads its input up to the length given, but the NUL keeps stale
	// bytes from an earlier, longer eval out of the parser.
	mem := i.mod.Memory()
	if !mem.WriteString(buf, src) || !mem.WriteByte(buf+uint32(len(src)), 0) {
		return Result{}, errors.New("qjs: write source to guest memory")
	}

	h := i.h
	if h == nil {
		h = noHandler
	}
	evalCtx := context.WithValue(ctx, stateKey{}, &callState{h: h})
	args := append([]uint64{uint64(len(src))}, flags...)
	res, err := fn.Call(evalCtx, args...)
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

// reserveBuffer has the guest hold an input buffer of at least n+1 bytes and returns
// its address. The guest grows the buffer only when a source is larger than any before it.
func (i *Instance) reserveBuffer(ctx context.Context, n uint32) (uint32, error) {
	res, err := i.reserve.Call(ctx, uint64(n))
	if err != nil {
		return 0, fmt.Errorf("buffer_reserve: %w", err)
	}
	if ptr := uint32(res[0]); ptr != 0 {
		return ptr, nil
	}
	return 0, errors.New("qjs: guest could not allocate the input buffer")
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
