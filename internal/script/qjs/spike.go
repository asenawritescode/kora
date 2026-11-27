//go:build qjsscript

package qjs

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// spikeModule is the pinned QuickJS reactor, built from quickjs-ng v0.18.0 by
// build/build.sh (see build/README.md). It exports init/eval/buffer; it is NOT
// the production artifact, only the phase-1 proof of the memory cap and timeout.
//
//go:embed testdata/qjs-spike.wasm
var spikeModule []byte

// pageSize is wazero's WebAssembly linear-memory page size (64 KiB).
const pageSize = 64 * 1024

// evalOutcome is how one evaluation ended.
type evalOutcome int

const (
	evalOK        evalOutcome = iota // ran to completion, no exception
	evalException                    // QuickJS threw (e.g. the heap cap tripped)
	evalStopped                      // the host stopped a runaway guest (timeout)
)

func (o evalOutcome) String() string {
	switch o {
	case evalOK:
		return "ok"
	case evalException:
		return "js-exception"
	case evalStopped:
		return "host-stopped"
	default:
		return "unknown"
	}
}

// spikeLimits bound one evaluation.
type spikeLimits struct {
	heapBytes uint32        // QuickJS JS_SetMemoryLimit (0 = unset)
	stackSize uint32        // QuickJS JS_SetMaxStackSize (0 = default)
	maxPages  uint32        // wazero linear-memory cap, in 64 KiB pages
	timeout   time.Duration // host deadline for the whole evaluation
}

// evalSpike evaluates one JS program in a fresh QuickJS instance under the given
// limits and reports how it ended. A fresh runtime per call keeps the spike
// honest about isolation; pooling is phase 4.
func evalSpike(ctx context.Context, js string, lim spikeLimits) (evalOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, lim.timeout)
	defer cancel()

	cfg := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true). // the host can stop a runaway guest
		WithMemoryLimitPages(lim.maxPages)
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	defer rt.Close(context.Background())

	wasi_snapshot_preview1.MustInstantiate(ctx, rt)
	mod, err := rt.InstantiateWithConfig(ctx, spikeModule, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return evalStopped, fmt.Errorf("instantiate: %w", err)
	}
	if init0 := mod.ExportedFunction("_initialize"); init0 != nil {
		if _, err := init0.Call(ctx); err != nil {
			return evalStopped, fmt.Errorf("_initialize: %w", err)
		}
	}

	res, err := mod.ExportedFunction("init").Call(ctx, uint64(lim.heapBytes), uint64(lim.stackSize))
	if err != nil {
		return evalStopped, fmt.Errorf("init: %w", err)
	}
	if int32(res[0]) != 0 {
		return evalStopped, fmt.Errorf("init returned %d", int32(res[0]))
	}

	bufres, err := mod.ExportedFunction("buffer").Call(ctx)
	if err != nil {
		return evalStopped, fmt.Errorf("buffer: %w", err)
	}
	if !mod.Memory().Write(uint32(bufres[0]), []byte(js)) {
		return evalStopped, errors.New("source does not fit the input buffer")
	}

	evres, err := mod.ExportedFunction("eval").Call(ctx, uint64(len(js)))
	if err != nil {
		// A cancelled context closes the module and surfaces here: the runaway
		// guest was stopped by the host rather than allowed to run forever.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return evalStopped, nil
		}
		return evalStopped, fmt.Errorf("eval: %w", err)
	}
	if int32(evres[0]) == 1 {
		return evalException, nil
	}
	return evalOK, nil
}
