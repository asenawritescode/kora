// Package qjs is the phase-1 spike of the QuickJS-on-Wazero script runtime
// (WASM-SANDBOX epic #843). It proves the two things goja cannot give us: a real
// per-runtime heap cap (QuickJS JS_SetMemoryLimit) and a hard host timeout
// (wazero closing a runaway guest on context cancellation).
//
// Everything that actually touches QuickJS/wazero is behind the `qjsscript`
// build tag, so the default build and CI are unchanged and goja stays the only
// runtime until the switch (phase 5). Build and run the spike with:
//
//	go test -tags qjsscript ./internal/script/qjs
//
// The host bridge (kora.*, __kora_event__, #839 libraries) is phase 2; this
// package carries only the spike until then.
package qjs
