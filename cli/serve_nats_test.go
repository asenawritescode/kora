package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
)

func TestOutboxSideEffectSubjectExcludesRealtimeNamespace(t *testing.T) {
	got := outboxSideEffectSubject("kora")
	if got != "kora.kora.>" {
		t.Fatalf("outbox subject = %q", got)
	}
	if strings.HasPrefix("kora.realtime.site.changes", strings.TrimSuffix(got, ">")) {
		t.Fatalf("outbox subject %q also matches realtime messages", got)
	}
}

func TestAnalyticsSiteWALDirIsPartitionedAndSafe(t *testing.T) {
	first := analyticsSiteWALDir("data/analytics/wal", "tenant-one")
	second := analyticsSiteWALDir("data/analytics/wal", "tenant-two")
	if first == second || first != "data/analytics/wal/tenant-one" || second != "data/analytics/wal/tenant-two" {
		t.Fatalf("site WAL paths are not partitioned: first=%q second=%q", first, second)
	}
	if got := analyticsSiteWALDir("data/analytics/wal", "../escape/site"); strings.Contains(got, "/../") || strings.HasSuffix(got, "/site") {
		t.Fatalf("unsafe site name escaped WAL root: %q", got)
	}
}

func TestDecodeOutboxDeliveryRequiresCanonicalEnvelope(t *testing.T) {
	if _, err := decodeOutboxDelivery(contract.Delivery{Data: json.RawMessage(`{"type":"change","site":"demo"}`)}); err == nil {
		t.Fatal("raw realtime payload was accepted as an outbox event")
	}

	envelope := contract.EventEnvelope{
		ID: "event-1", Type: "kora.invoice.after_insert", Version: 1,
		Source: "kora.kernel", Site: "demo", AggregateType: "Invoice", AggregateID: "INV-1",
		OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{"data":{"status":"Draft"}}`),
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	event, err := decodeOutboxDelivery(contract.Delivery{Data: body})
	if err != nil {
		t.Fatal(err)
	}
	if event.Site != "demo" || event.Doctype != "Invoice" || event.DocName != "INV-1" {
		t.Fatalf("unexpected decoded event: %#v", event)
	}
}
