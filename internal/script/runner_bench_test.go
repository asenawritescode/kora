package script

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// benchRunner returns a runner with the default pool, closed at benchmark end.
func benchRunner(b *testing.B) *EmbeddedRunner {
	b.Helper()
	r := NewEmbeddedRunner(DefaultEmbeddedConfig())
	b.Cleanup(func() { _ = r.Close() })
	return r
}

// BenchmarkExecute measures one Execute per iteration on the active runner (goja by
// default, QuickJS with -tags qjsscript). Each sub-benchmark covers one cost:
// the bare round trip, host reads, library loading and a large document.
func BenchmarkExecute(b *testing.B) {
	bigDoc := make(map[string]any, 50)
	for i := range 50 {
		bigDoc[fmt.Sprintf("field_%02d", i)] = fmt.Sprintf("value %d", i)
	}
	libraries := []Library{
		{Name: "lib_a", Source: `var fmtA = function(x) { return "A" + x; };`},
		{Name: "lib_b", Source: `var fmtB = function(x) { return "B" + x; };`},
		{Name: "lib_c", Source: `var fmtC = function(x) { return "C" + x; };`},
	}
	cases := []struct {
		name string
		req  ExecuteRequest
	}{
		{name: "trivial", req: ExecuteRequest{Script: `return {result: 1};`}},
		{name: "kora_reads", req: ExecuteRequest{
			Script:   strings.Repeat("kora.getDoc('Customer', 'C-1');\n", 10) + "return {result: 1};",
			Provider: &fakeKoraProvider{doc: map[string]any{"name": "C-1"}},
		}},
		{name: "libraries", req: ExecuteRequest{
			Script:    `return {result: fmtA(1) + fmtB(2) + fmtC(3)};`,
			Libraries: libraries,
		}},
		{name: "event_doc", req: ExecuteRequest{
			Script:   `return {doc: event.doc};`,
			Document: bigDoc,
		}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			r := benchRunner(b)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := r.Execute(context.Background(), tc.req); err != nil {
					b.Fatalf("Execute: %v", err)
				}
			}
		})
	}
}
