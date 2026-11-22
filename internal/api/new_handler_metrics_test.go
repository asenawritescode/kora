package api

import (
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

// A handler built without the router can still answer metrics reads: its metrics API is never the zero value,
// which would dereference a nil definitions loader when Ask Zana calls a metrics tool.
func TestNewHandlerHasAMetricsAPI(t *testing.T) {
	h := NewHandler(doctype.NewRegistry(), nil)
	if h.metrics.defs == nil {
		t.Fatal("NewHandler left the metrics API without a definitions loader")
	}
}
