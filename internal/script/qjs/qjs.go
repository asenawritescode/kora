// Package qjs is the QuickJS-on-Wazero script runtime host (WASM-SANDBOX epic #843,
// phase 2). A QuickJS reactor (build/qjs_reactor.c, rebuilt into testdata/qjs.wasm)
// runs guest JavaScript under wazero. The guest reaches the Go host through one
// JSON call (kora_host.call / kora_host.take); this package never interprets the
// JSON. The kora.* mapping lives in package script (WASM-RUNNER, WASM-BRIDGE).
//
// This is the production script runtime: the default build of package script runs
// guest JavaScript on this host. The goja fallback (-tags gojascript) is removed
// one release after the switch. Test with:
//
//	go test ./internal/script/qjs
package qjs
