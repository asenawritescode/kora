//go:build integration

package engineadapter

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/go-sql-driver/mysql"
)

func TestMySQLSubscriptionBillingProgressSurvivesRestartAndConcurrentRuns(t *testing.T) {
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL root connection:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	databaseName := fmt.Sprintf("kora_billing_fixture_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + databaseName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create disposable billing database:", err)
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS `" + databaseName + "`"); _ = root.Close() })
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable MySQL DSN:", err)
	}
	parsed.DBName = databaseName
	dsn = parsed.FormatDSN()
	open := func() (*sql.DB, error) {
		database, err := sql.Open("mysql", dsn)
		if err == nil {
			database.SetMaxOpenConns(4)
		}
		return database, err
	}
	database, err := open()
	if err != nil {
		t.Fatal("open fresh fixture database:", err)
	}
	dialect := db.Resolve("mysql")
	if err := site.BootstrapSystemTables(database, dialect); err != nil {
		t.Fatal("bootstrap fresh billing fixture:", err)
	}
	suffix := contract.NewID()[:12]
	tenant := contract.TenantID("tenant-" + contract.NewID())
	siteName := "billing-fixture-" + suffix
	registry := newBillingFixtureRegistry()
	if err := schema.MigrateSiteFromRegistry(database, siteName, registry, dialect); err != nil {
		t.Fatal("migrate fresh programmatic subscription schemas:", err)
	}
	for _, table := range []string{"tabRecurring Invoice", "tabBilling Schedule"} {
		var count int
		if err := database.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?`, table).Scan(&count); err != nil {
			t.Fatalf("inspect fresh fixture for %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("fresh fixture contains removed local invoice table %s", table)
		}
	}
	due := time.Now().UTC().Format("2006-01-02")
	subscriptionName := "sub-" + suffix
	quote := dialect.QuoteIdent
	insert := fmt.Sprintf("INSERT INTO %s (name,status,next_billing_date,start_date,billing_interval,interval_count,quantity,unit_price,currency,customer) VALUES (?,?,?,?,?,?,?,?,?,?)", quote("tabSubscription"))
	if _, err := database.Exec(insert, subscriptionName, "Active", due, due, "Monthly", 1, 1, 12.5, "USD", "customer-1"); err != nil {
		t.Fatal("seed due subscription:", err)
	}
	provider := &persistentBillingFixture{ledger: map[string]BillingResult{}}
	if err := runBillingFixture(t, context.Background(), database, dialect, registry, tenant, siteName, provider); err != nil {
		t.Fatal("first scheduler run:", err)
	}
	if provider.creates != 1 {
		t.Fatalf("first scheduler run created %d obligations, want one", provider.creates)
	}
	var nextDate string
	if err := database.QueryRow("SELECT next_billing_date FROM "+quote("tabSubscription")+" WHERE name=?", subscriptionName).Scan(&nextDate); err != nil {
		t.Fatal("read advanced billing date:", err)
	}
	if nextDate <= due {
		t.Fatalf("schedule remained at %s after accepted obligation for %s", nextDate, due)
	}
	if _, err := database.Exec("SELECT 1 FROM " + quote("tabRecurring Invoice")); err == nil {
		t.Fatal("fixture unexpectedly contains the removed local invoice table")
	}
	_ = database.Close()

	// A new DB handle and handler instance model Engine/scheduler restart.
	database, err = open()
	if err != nil {
		t.Fatal("reopen fixture after restart:", err)
	}
	defer database.Close()
	if err := runBillingFixture(t, context.Background(), database, dialect, newBillingFixtureRegistry(), tenant, siteName, provider); err != nil {
		t.Fatal("scheduler restart run:", err)
	}
	if provider.creates != 1 {
		t.Fatalf("restart created a duplicate obligation; count=%d", provider.creates)
	}
	update := "UPDATE " + quote("tabSubscription") + " SET next_billing_date=? WHERE name=?"
	// The obligation key is deterministic per (tenant, subscription, billing
	// date), so re-billing the same date is a no-op by design. Move the schedule
	// back one month to make a second, distinct period due for the racing runs.
	secondPeriod := time.Now().UTC().AddDate(0, -1, 0).Format("2006-01-02")
	if _, err := database.Exec(update, secondPeriod, subscriptionName); err != nil {
		t.Fatal("make a second period due for concurrent runs:", err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			concurrentDB, err := open()
			if err != nil {
				errs <- err
				return
			}
			defer concurrentDB.Close()
			<-start
			errs <- runBillingFixture(t, context.Background(), concurrentDB, dialect, newBillingFixtureRegistry(), tenant, siteName, provider)
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
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.creates != 2 {
		t.Fatalf("concurrent run produced %d total obligations, want one for each of two periods", provider.creates)
	}
}
