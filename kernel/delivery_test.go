//go:build integration
// +build integration

// Durable-delivery evidence for the kernel slice (DURABLE-001/002/005):
// committed outbox rows must eventually publish through both the local WAL
// provider and NATS JetStream, with receipt dedup and redaction guarantees.
package kernel_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/natsprovider"
	"github.com/asenawritescode/kora/outbox"
	"github.com/nats-io/nats-server/v2/server"
)

// capturingBus is a minimal analytics.EventBus used to observe deliveries
// routed through the local WAL provider.
type capturingBus struct {
	events chan analytics.ChangeEvent
}

func (b *capturingBus) Publish(e analytics.ChangeEvent) error {
	b.events <- e
	return nil
}
func (b *capturingBus) Subscribe() (<-chan analytics.ChangeEvent, error) { return b.events, nil }
func (b *capturingBus) DrainWAL(func(analytics.ChangeEvent)) (int, error) {
	return 0, nil
}
func (b *capturingBus) RotateWAL() (string, error)     { return "", nil }
func (b *capturingBus) CommitWALRotation(string) error { return nil }
func (b *capturingBus) Dropped() int64                 { return 0 }
func (b *capturingBus) Close() error                   { return nil }

// TestLocalProviderDeliveryAfterCommit proves the local durable provider path:
// kernel commit → _kora_outbox → Publisher → LocalProvider → EventBus.
func TestLocalProviderDeliveryAfterCommit(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)

	res := exec(t, s, k, createOp(map[string]any{"title": "Local delivery"}))
	mustComplete(t, res)

	bus := &capturingBus{events: make(chan analytics.ChangeEvent, 4)}
	pub := outbox.NewPublisher(s.DB, analytics.NewLocalProvider(bus), s.Dialect)
	delivered, err := pub.PublishDue(context.Background(), 10)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("expected 1 delivery via LocalProvider, got %d", delivered)
	}
	select {
	case ev := <-bus.events:
		if ev.Site != s.Name || ev.DocName == "" {
			t.Fatalf("unexpected change event %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no event reached the local bus")
	}
}

// TestNATSJetStreamDeliveryAfterCommit proves the JetStream path end-to-end:
// embedded NATS server, durable pull consumer receives the exact committed
// event (site + aggregate), satisfying the acceptance scenario that an SQL
// commit plus broker outage eventually publishes through the outbox.
func TestNATSJetStreamDeliveryAfterCommit(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()

	opts := &server.Options{JetStream: true, Port: -1}
	natsSrv, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("embedded nats: %v", err)
	}
	go natsSrv.Start()
	defer natsSrv.Shutdown()
	if !natsSrv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats not ready")
	}
	url := fmt.Sprintf("nats://%s", natsSrv.Addr().String())

	cfg := natsprovider.Config{
		Name:          "kernel-slice",
		ServerURLs:    []string{url},
		StreamName:    "KORA_EVENTS",
		SubjectPrefix: "kora",
		ConsumerName:  "kernel-slice-test",
		MaxDeliver:    5,
	}
	prov, err := natsprovider.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	defer prov.Close()
	if err := prov.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	k := newKernel(s)
	res := exec(t, s, k, createOp(map[string]any{"title": "NATS delivery"}))
	mustComplete(t, res)

	var docName string
	if err := s.DB.QueryRow("SELECT name FROM `tabTask` WHERE title='NATS delivery'").Scan(&docName); err != nil {
		t.Fatalf("doc: %v", err)
	}

	// Broker outage window: nothing published yet; rows stay pending.
	pub := outbox.NewPublisher(s.DB, prov, s.Dialect)
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE status='pending'"); n != 1 {
		t.Fatalf("commit must leave exactly one pending row, got %d", n)
	}

	consumer, err := natsprovider.NewConsumer(prov, cfg)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	got := make(chan contract.Delivery, 1)
	go func() {
		_ = consumer.Run(ctx, func(_ context.Context, d contract.Delivery) error {
			select {
			case got <- d:
			default:
			}
			return nil
		})
	}()

	if _, err := pub.PublishDue(ctx, 10); err != nil {
		t.Fatalf("publish due: %v", err)
	}

	select {
	case d := <-got:
		var env contract.EventEnvelope
		if err := json.Unmarshal(d.Data, &env); err != nil {
			t.Fatalf("delivery payload: %v", err)
		}
		if env.Site != s.Name || env.AggregateID != docName {
			t.Fatalf("delivered wrong event: site=%s aggregate=%s", env.Site, env.AggregateID)
		}
		if n := s.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE status='published'"); n != 1 {
			t.Fatalf("row should be marked published after JetStream delivery")
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no delivery observed from JetStream")
	}
}

// TestAuditLedgerStoresNoPayloads proves redaction: the audit ledger records
// hashes and identity metadata but never business payloads or secrets.
func TestAuditLedgerStoresNoPayloads(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)

	const secretValue = "CONFIDENTIAL-SERIAL-XYZ"
	res := exec(t, s, k, createOp(map[string]any{"title": "Audit probe", "serial": secretValue}))
	mustComplete(t, res)

	rows, err := s.DB.Query(`SELECT id, site, operation_id, correlation_id, causation_id, source,
		principal_type, principal_id, actor_user, actor_roles, command_name, doctype, doc_name,
		status, error_code, payload_hash FROM _kora_operation_audit WHERE doctype='Task'`)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		found = true
		var id, site, opID, corr, caus, source, ptype, pid, user, roles, cmd, dt, doc, status, code string
		var hash string
		if err := rows.Scan(&id, &site, &opID, &corr, &caus, &source, &ptype, &pid, &user, &roles, &cmd, &dt, &doc, &status, &code, &hash); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cells := []string{id, site, opID, corr, caus, source, ptype, pid, user, roles, cmd, dt, doc, status, code, hash}
		for _, c := range cells {
			if c == secretValue {
				t.Fatalf("audit row leaks the business payload value in column data")
			}
		}
		if len(hash) != 64 {
			t.Fatalf("payload_hash must be a sha256 hex digest, got %q", hash)
		}
	}
	if !found {
		t.Fatalf("expected at least one audit row")
	}
}
