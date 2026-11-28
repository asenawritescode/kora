# Building the QuickJS reactor (`testdata/qjs.wasm`)

`testdata/qjs.wasm` is a pinned, reproducible build of `qjs_reactor.c`, a QuickJS
WASI reactor. It is vendored and CI checks that it is the build of the pinned sources below.
It is the production script runtime (the default build of `internal/script`), so a rebuild is a release change.

## Inputs (pinned)
- **quickjs-ng v0.18.0**: core sources only (`quickjs.c dtoa.c libregexp.c libunicode.c`;
  no `quickjs-libc.c`, so no POSIX `environ`/`signal` gaps under WASI).
- **wasi-sdk 34.0** (clang, target `wasm32-wasip1`).
- `qjs_reactor.c` (in this directory).

## Exports (guest)
- `init(heap, stack) -> i32`: start a runtime and context; 0 ok, -1 failed.
- `buffer_reserve(n) -> ptr`: a heap input buffer of at least `n+1` bytes (the host NUL-terminates
  the source after writing it). It grows only when `n` exceeds the current size; NULL on allocation failure.
- `eval(len, flags) -> i32`: flags 1 = compile only, 2 = JSON result.
  0 ok, 1 JS exception, -2 not initialised.
- `output() -> ptr`, `output_len() -> i32`: the result or exception text of the last eval.
- `compile(len) -> i32`: compiles the source in the input buffer without running it; 0 ok with output = the
  bytecode (binary), 1 syntax error with output = its message. Bytecode runs through `eval_bytecode` on any
  instance of the same runtime, the compiling one included (wazero v1.12.0; v1.8.2 trapped on large bytecode, #879).
- `eval_bytecode(len, flags) -> i32`: runs bytecode from the input buffer with the same flags and answers as `eval`.
- `reseed(lo, hi)`: sets the context's `Math.random` state. Called on every instance restored from a
  snapshot (the snapshot copies the generator state, so each instance reseeds from crypto/rand).
- Exported mutable global `__stack_pointer` (`-Wl,--export=__stack_pointer`): saved and restored with the
  snapshot together with linear memory. It is the only mutable wasm global (`wasm-objdump -x` lists it).

## Patched sources
`quickjs-reseed.patch` adds `JS_KoraReseedRandom` to `quickjs.c` (the `JSContext` is opaque to the reactor).
`build.sh` applies it with `git apply` after cloning, so the pinned tag plus this patch is the build input.

## Imports (host, module `kora_host`)
- `call(ptr, len) -> respLen`: one synchronous JSON request; `-1` means failed.
- `take(dst)`: copies the pending response into `dst` (`respLen` bytes).

## Reproduce
```sh
internal/script/qjs/build/build.sh <workdir> internal/script/qjs/testdata/qjs.wasm
```

`build.sh` downloads wasi-sdk 34.0 (sha256 pinned, skipped when `WASI_SDK` is set),
clones quickjs-ng v0.18.0 (commit d7215b0ad3fc2c562e8317fd9bca058fd4863804), applies
`quickjs-reseed.patch`, copies `qjs_reactor.c` in and runs the clang command above.
CI (`WASM build` job in `.github/workflows/ci.yml`) rebuilds the file and fails if its
sha256 differs from `testdata/qjs.wasm`.

The wasm shadow stack is 1 MiB. The runtime caps the JS stack at 768 KiB
(`StackBytes` in `internal/script/runtime_qjs.go`), so QuickJS throws a catchable
RangeError before the wasm stack overflows.

