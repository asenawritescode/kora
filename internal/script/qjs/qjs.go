// Package qjs is the QuickJS-on-Wazero script runtime host (WASM-SANDBOX epic #843,
// phase 2). A QuickJS reactor (build/qjs_reactor.c, rebuilt into testdata/qjs.wasm)
// runs guest JavaScript under wazero. The guest reaches the Go host through one
// JSON call (kora_host.call / kora_host.take); this package never interprets the
// JSON. The kora.* mapping lives in package script (WASM-RUNNER, WASM-BRIDGE).
//
// Everything that touches QuickJS or wazero is behind the `qjsscript` build tag,
// so the default build and CI are unchanged and goja stays the only runtime until
// the switch (phase 5). Test with:
//
//	go test -tags qjsscript ./internal/script/qjs
package qjs
