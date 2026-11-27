# Building the QuickJS spike wasm (#843 phase 1)

`testdata/qjs-spike.wasm` is a pinned, reproducible build of a minimal QuickJS
WASI reactor. It is the phase-1 proof artifact (memory cap + timeout), not the
production runtime — the kora.* host bridge is phase 2.

## Inputs (pinned)
- **quickjs-ng v0.18.0** — the maintained QuickJS fork (core sources only:
  `quickjs.c dtoa.c libregexp.c libunicode.c`; no `quickjs-libc.c`, so no POSIX
  `environ`/`signal` gaps under WASI).
- **wasi-sdk 34.0** (clang 23.1.0, target `wasm32-wasip1`).
- `spike_reactor.c` (in this directory) — a ~40-line reactor exporting
  `init(heap, stack)`, `eval(len)`, `buffer()`.

## Reproduce
```sh
# 1. QuickJS source
git clone --depth 1 --branch v0.18.0 https://github.com/quickjs-ng/quickjs.git qjs
cp spike_reactor.c qjs/

# 2. WASI SDK 34 (self-contained, no root)
#    https://github.com/WebAssembly/wasi-sdk/releases/tag/wasi-sdk-34
WASI=/path/to/wasi-sdk

# 3. Build the reactor
cd qjs
"$WASI/bin/clang" --target=wasm32-wasip1 \
  --sysroot="$WASI/share/wasi-sysroot" \
  -mexec-model=reactor -O2 -DQUICKJS_NG_BUILD \
  -Wl,-z,stack-size=4194304 \
  -o qjs-spike.wasm \
  spike_reactor.c quickjs.c dtoa.c libregexp.c libunicode.c -lm
```

Copy the result to `../testdata/qjs-spike.wasm`.

## Follow-up (not phase 1)
Wire wasi-sdk into CI so the artifact is rebuilt rather than vendored, and run
`go test -tags qjsscript ./internal/script/qjs` there. Tracked under #843.
