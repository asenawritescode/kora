//go:build integration

package engineadapter

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/site"
	_ "github.com/mattn/go-sqlite3"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

type persistentBillingFixture struct {
	mu      sync.Mutex
	ledger  map[string]BillingResult
	creates int
}

// sqlBillingOwnerCommandsFixture models an internal domain owner whose
// idempotency ledger shares the durable database with subscription state.
type sqlBillingOwnerCommandsFixture struct{ database *sql.DB }

func (p sqlBillingOwnerCommandsFixture) CreateObligation(ctx context.Context, _ policy.Execution, scope contract.ScopeContext, obligation BillingObligation) (BillingResult, error) {
	if p.database == nil || obligation.Source.Scope != scope.Scope || obligation.Source.ProviderID != scope.ProviderID {
		return BillingResult{}, errors.New("internal billing fixture scope is invalid")
	}
	prefix := sha256.Sum256([]byte(string(scope.Scope.TenantID) + "\x00" + obligation.IdempotencyKey))
	obligationID := "internal-" + hex.EncodeToString(prefix[:16])
	encoded, err := json.Marshal(BillingResult{Obligation: contract.BusinessRef{ProviderID: "kora.billing", ResourceType: "obligation", ID: obligationID, Scope: scope.Scope}, State: BillingStateAccepted})
	if err != nil {
		return BillingResult{}, err
	}
	if _, err := p.database.ExecContext(ctx, `INSERT OR IGNORE INTO fixture_billing_obligation (tenant_id, idempotency_key, result_json) VALUES (?, ?, ?)`, string(scope.Scope.TenantID), obligation.IdempotencyKey, string(encoded)); err != nil {
		return BillingResult{}, err
	}
	return p.ReconcileObligation(ctx, policy.Execution{}, scope, obligation.IdempotencyKey)
}

func (p sqlBillingOwnerCommandsFixture) ReconcileObligation(ctx context.Context, _ policy.Execution, scope contract.ScopeContext, key string) (BillingResult, error) {
	if p.database == nil || key == "" || scope.Scope.TenantID == "" {
		return BillingResult{}, errors.New("internal billing fixture reconciliation identity is invalid")
	}
	var encoded string
	err := p.database.QueryRowContext(ctx, `SELECT result_json FROM fixture_billing_obligation WHERE tenant_id=? AND idempotency_key=?`, string(scope.Scope.TenantID), key).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return BillingResult{State: BillingStateNotFound}, nil
	}
	if err != nil {
		return BillingResult{}, err
	}
	var result BillingResult
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		return BillingResult{}, err
	}
	return result, nil
}

func (p *persistentBillingFixture) CreateObligation(_ context.Context, _ policy.Execution, _ contract.ScopeContext, obligation BillingObligation) (BillingResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if result, ok := p.ledger[obligation.IdempotencyKey]; ok {
		return result, nil
	}
	p.creates++
	result := BillingResult{Obligation: contract.BusinessRef{ProviderID: "fixture.billing", ResourceType: "obligation", ID: fmt.Sprintf("obligation-%d", p.creates), Scope: obligation.Source.Scope}, State: BillingStateAccepted}
	p.ledger[obligation.IdempotencyKey] = result
	return result, nil
}

func (p *persistentBillingFixture) ReconcileObligation(_ context.Context, _ policy.Execution, _ contract.ScopeContext, key string) (BillingResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if result, ok := p.ledger[key]; ok {
		return result, nil
	}
	return BillingResult{State: BillingStateNotFound}, nil
}

func TestLibSQLSubscriptionBillingProgressSurvivesRestartAndConcurrentRuns(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "subscriptions.db")
	open := func() (*sql.DB, error) {
		database, err := sql.Open("libsql", dsn)
		if err != nil {
			return nil, err
		}
		database.SetMaxOpenConns(1)
		return database, nil
	}
	dialect := db.Resolve("libsql")
	suffix := contract.NewID()[:12]
	tenant := contract.TenantID("tenant-" + contract.NewID())
	siteName := "billing-fixture-" + suffix
	registry := newBillingFixtureRegistry()
	database, err := open()
	if err != nil {
		t.Fatal("open fresh fixture libSQL:", err)
	}
	if err := site.BootstrapSystemTables(database, dialect); err != nil {
		t.Fatal("bootstrap fresh fixture:", err)
	}
	if err := schema.MigrateSiteFromRegistry(database, siteName, registry, dialect); err != nil {
		t.Fatal("migrate fresh programmatic subscription schemas:", err)
	}
	if _, err := database.Exec(`CREATE TABLE fixture_billing_obligation (tenant_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, result_json TEXT NOT NULL, PRIMARY KEY (tenant_id, idempotency_key))`); err != nil {
		t.Fatal("create internal billing owner fixture ledger:", err)
	}
	for _, table := range []string{"tabRecurring Invoice", "tabBilling Schedule"} {
		var found string
		err := database.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&found)
		if err != sql.ErrNoRows {
			t.Fatalf("fresh fixture should not contain %s table, found %q (err=%v)", table, found, err)
		}
	}
	due := time.Now().UTC().Format("2006-01-02")
	if _, err := database.Exec(`INSERT INTO "tabSubscription" (name,status,next_billing_date,start_date,billing_interval,interval_count,quantity,unit_price,currency,customer) VALUES (?,?,?,?,?,?,?,?,?,?)`, "sub-"+suffix, "Active", due, due, "Monthly", 1, 1, 12.5, "USD", "customer-1"); err != nil {
		t.Fatal("seed due subscription:", err)
	}
	internalBilling := InternalBillingAdapter{Commands: sqlBillingOwnerCommandsFixture{database: database}}
	pendingProvider := &fixedStateBillingProvider{state: BillingStatePending}
	if err := runBillingFixture(t, ctx, database, dialect, registry, tenant, siteName, pendingProvider); err == nil {
		t.Fatal("pending billing result was treated as successful")
	}
	var unchangedDate string
	if err := database.QueryRow(`SELECT next_billing_date FROM "tabSubscription" WHERE name=?`, "sub-"+suffix).Scan(&unchangedDate); err != nil {
		t.Fatal("read schedule after pending billing:", err)
	}
	if unchangedDate != due || pendingProvider.creates != 0 {
		t.Fatalf("pending billing advanced schedule to %s or created %d obligations", unchangedDate, pendingProvider.creates)
	}
	if _, err := database.Exec(`CREATE TRIGGER fail_subscription_schedule BEFORE UPDATE OF next_billing_date ON "tabSubscription" BEGIN SELECT RAISE(ABORT, 'simulated schedule write failure'); END`); err != nil {
		t.Fatal("install schedule failure trigger:", err)
	}
	if err := runBillingFixture(t, ctx, database, dialect, registry, tenant, siteName, internalBilling); err == nil {
		t.Fatal("schedule write failure was treated as successful")
	}
	if _, err := database.Exec(`DROP TRIGGER fail_subscription_schedule`); err != nil {
		t.Fatal("remove schedule failure trigger:", err)
	}
	var obligations int
	if err := database.QueryRow(`SELECT COUNT(*) FROM fixture_billing_obligation WHERE tenant_id=?`, string(tenant)).Scan(&obligations); err != nil || obligations != 1 {
		t.Fatalf("failed schedule commit left %d durable internal obligations, want one (err=%v)", obligations, err)
	}
	var unchangedAfterFailedWrite string
	if err := database.QueryRow(`SELECT next_billing_date FROM "tabSubscription" WHERE name=?`, "sub-"+suffix).Scan(&unchangedAfterFailedWrite); err != nil {
		t.Fatal("read schedule after failed write:", err)
	}
	if unchangedAfterFailedWrite != due {
		t.Fatalf("schedule advanced to %s despite injected write failure", unchangedAfterFailedWrite)
	}
	_ = database.Close()

	// Restart against the same durable subscription and internal-owner ledger.
	database, err = open()
	if err != nil {
		t.Fatal("reopen fixture libSQL after restart:", err)
	}
	defer database.Close()
	internalBilling = InternalBillingAdapter{Commands: sqlBillingOwnerCommandsFixture{database: database}}
	registry = newBillingFixtureRegistry()
	engine := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	if err := runBillingFixtureWithEngine(ctx, database, dialect, registry, tenant, siteName, internalBilling, engine); err != nil {
		t.Fatal("scheduler restart run:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM fixture_billing_obligation WHERE tenant_id=?`, string(tenant)).Scan(&obligations); err != nil || obligations != 1 {
		t.Fatalf("restart left %d durable internal obligations, want exactly one (err=%v)", obligations, err)
	}

	var nextDate string
	if err := database.QueryRow(`SELECT next_billing_date FROM "tabSubscription" WHERE name=?`, "sub-"+suffix).Scan(&nextDate); err != nil || nextDate <= due {
		t.Fatalf("restart did not advance accepted billing period: next=%q err=%v", nextDate, err)
	}
	secondSubscription := "sub-internal-concurrent-" + suffix
	if _, err := database.Exec(`INSERT INTO "tabSubscription" (name,status,next_billing_date,start_date,billing_interval,interval_count,quantity,unit_price,currency,customer) VALUES (?,?,?,?,?,?,?,?,?,?)`, secondSubscription, "Active", due, due, "Monthly", 1, 1, 12.5, "USD", "customer-1"); err != nil {
		t.Fatal("seed concurrently billed internal subscription:", err)
	}
	concurrentRuns := func(provider BillingProvider) {
		t.Helper()
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wait sync.WaitGroup
		for i := 0; i < 2; i++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				concurrentDB, openErr := open()
				if openErr != nil {
					errs <- openErr
					return
				}
				defer concurrentDB.Close()
				<-start
				errs <- runBillingFixture(t, ctx, concurrentDB, dialect, newBillingFixtureRegistry(), tenant, siteName, provider)
			}()
		}
		close(start)
		wait.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Errorf("concurrent scheduler invocation: %v", err)
			}
		}
	}
	concurrentRuns(InternalBillingAdapter{Commands: sqlBillingOwnerCommandsFixture{database: database}})
	if err := database.QueryRow(`SELECT COUNT(*) FROM fixture_billing_obligation WHERE tenant_id=?`, string(tenant)).Scan(&obligations); err != nil || obligations != 2 {
		t.Fatalf("concurrent internal scheduler runs left %d obligations, want exactly two total periods (err=%v)", obligations, err)
	}
	var internalNext string
	if err := database.QueryRow(`SELECT next_billing_date FROM "tabSubscription" WHERE name=?`, secondSubscription).Scan(&internalNext); err != nil || internalNext <= due {
		t.Fatalf("concurrent internal scheduler runs did not advance the schedule once: next=%q err=%v", internalNext, err)
	}

	thirdSubscription := "sub-external-concurrent-" + suffix
	if _, err := database.Exec(`INSERT INTO "tabSubscription" (name,status,next_billing_date,start_date,billing_interval,interval_count,quantity,unit_price,currency,customer) VALUES (?,?,?,?,?,?,?,?,?,?)`, thirdSubscription, "Active", due, due, "Monthly", 1, 1, 12.5, "USD", "customer-1"); err != nil {
		t.Fatal("seed concurrently billed external subscription:", err)
	}
	externalProvider := &persistentBillingFixture{ledger: map[string]BillingResult{}}
	concurrentRuns(externalProvider)
	externalProvider.mu.Lock()
	defer externalProvider.mu.Unlock()
	if externalProvider.creates != 1 {
		t.Fatalf("concurrent external scheduler runs produced %d effects, want exactly one", externalProvider.creates)
	}
}

func runBillingFixture(t *testing.T, ctx context.Context, database *sql.DB, dialect db.Dialect, registry *doctype.Registry, tenant contract.TenantID, siteName string, provider BillingProvider) error {
	t.Helper()
	return runBillingFixtureWithEngine(ctx, database, dialect, registry, tenant, siteName, provider, kernel.New(dialect, outbox.NewSQLWriter(dialect)))
}

func runBillingFixtureWithEngine(ctx context.Context, database *sql.DB, dialect db.Dialect, registry *doctype.Registry, tenant contract.TenantID, siteName string, provider BillingProvider, commandKernel *kernel.Kernel) error {
	generator := &recurringInvoiceGenerator{database: database, registry: registry, dialect: dialect, kernel: commandKernel, tenantID: string(tenant), siteName: siteName, billing: provider}
	return generator.GenerateDueInvoices(ctx, nil)
}

func newBillingFixtureRegistry() *doctype.Registry {
	registry := doctype.NewRegistry()
	subscription := &doctype.DocType{Name: "Subscription", Fields: []doctype.Field{
		{Fieldname: "status", Fieldtype: "Data"}, {Fieldname: "next_billing_date", Fieldtype: "Date"}, {Fieldname: "start_date", Fieldtype: "Date"},
		{Fieldname: "billing_interval", Fieldtype: "Data"}, {Fieldname: "interval_count", Fieldtype: "Int"}, {Fieldname: "end_date", Fieldtype: "Date"},
		{Fieldname: "quantity", Fieldtype: "Float"}, {Fieldname: "unit_price", Fieldtype: "Currency"}, {Fieldname: "currency", Fieldtype: "Data"},
		{Fieldname: "customer", Fieldtype: "Data"}, {Fieldname: "plan", Fieldtype: "Data"},
	}}
	plan := &doctype.DocType{Name: "Plan", Fields: []doctype.Field{{Fieldname: "tax_rate", Fieldtype: "Float"}}}
	line := &doctype.DocType{Name: "Subscription Line", Fields: []doctype.Field{{Fieldname: "subscription", Fieldtype: "Data"}, {Fieldname: "status", Fieldtype: "Data"}, {Fieldname: "quantity", Fieldtype: "Float"}, {Fieldname: "unit_price", Fieldtype: "Currency"}}}
	registry.LoadFull([]*doctype.DocType{subscription, plan, line}, []*doctype.Role{{Name: "Billing Worker"}}, []*doctype.Permission{
		{Doctype: "Subscription", Role: "Billing Worker", Read: true, Write: true},
		{Doctype: "Plan", Role: "Billing Worker", Read: true},
		{Doctype: "Subscription Line", Role: "Billing Worker", Read: true},
	})
	return registry
}
