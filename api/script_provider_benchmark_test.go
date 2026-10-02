//go:build integration

package api

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/script"
)

// BenchmarkScriptProviderCRUD measures a complete three-command script
// provider cycle against the integration MySQL fixture. Use -benchtime=10x or
// more for comparisons and keep the DSN/schema/pool settings constant.
func BenchmarkScriptProviderCRUD(b *testing.B) {
	handler, database, registry, site := newResourceIntegrationHandler(b, kernelMeasuredMySQLDriverName)
	for _, ddl := range db.ExtensibilityTablesMySQL()[:2] {
		if _, err := database.Exec(ddl); err != nil {
			b.Fatalf("create script tables: %v", err)
		}
	}
	tx := *handler.TxManager
	tx.CurrentUser = "benchmark@example.test"
	tx.CurrentUserRole = doctype.AdminRole
	tx.CurrentUserRoles = []string{doctype.AdminRole}
	tx.ScriptRunner = &nestedScriptMutationRunner{}
	tx.ScriptStore = &script.Store{DB: database, Dialect: db.Resolve("mysql")}
	provider := NewScriptProvider(&tx, registry, site, nil, nil)

	kernelLoadMetrics.reset()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		title := fmt.Sprintf("benchmark-%d", index)
		created, err := provider.CreateDoc("TestDoc", map[string]any{"title": title}, "benchmark-owner", "benchmark@example.test")
		if err != nil {
			b.Fatalf("create: %v", err)
		}
		name := created["name"].(string)
		if err := provider.SaveDoc("TestDoc", map[string]any{"name": name, "title": title + "-updated"}, "benchmark@example.test"); err != nil {
			b.Fatalf("update: %v", err)
		}
		if err := provider.DeleteDoc("TestDoc", name); err != nil {
			b.Fatalf("delete: %v", err)
		}
	}
	b.ReportMetric(float64(3*b.N)/b.Elapsed().Seconds(), "mutations/s")
	statementCount, transactionTimes := kernelLoadMetrics.snapshot()
	b.ReportMetric(float64(statementCount)/float64(3*b.N), "sql/mutation")
	b.ReportMetric(meanDuration(transactionTimes).Seconds()*1e9, "tx-ns/op")
}

// TestScriptProviderCRUDLoad measures the script bridge's canonical create,
// update, and delete commands under concurrent load. Each worker owns its
// provider/TxManager instance; only the registry and DB pool are shared.
func TestScriptProviderCRUDLoad(t *testing.T) {
	const workers, recordsPerWorker = 8, 12
	handler, database, registry, site := newResourceIntegrationHandler(t, kernelMeasuredMySQLDriverName)
	for _, ddl := range db.ExtensibilityTablesMySQL()[:2] {
		if _, err := database.Exec(ddl); err != nil {
			t.Fatalf("create script tables: %v", err)
		}
	}
	store := &script.Store{DB: database, Dialect: db.Resolve("mysql")}
	runner := script.NewEmbeddedRunner(script.EmbeddedConfig{PoolSize: workers, Timeout: 5 * time.Second})
	t.Cleanup(func() { _ = runner.Close() })
	if err := store.Insert(script.ScriptRecord{
		Name: "load-validation-hook", Site: site, ScriptType: script.TypeDocEvent,
		DocType: "TestDoc", Event: script.EventValidate, IsActive: true,
		Script: "var doc = __kora_event__.doc; var total = 0; for (var i = 0; i < 200; i++) total += i; if (!doc.title || total !== 19900) throw 'validation failed';",
	}); err != nil {
		t.Fatalf("insert load validation hook: %v", err)
	}
	providers := make([]script.KoraProvider, workers)
	for worker := range workers {
		tx := *handler.TxManager
		tx.CurrentUser = fmt.Sprintf("script-load-%02d@example.test", worker)
		tx.CurrentUserRole = doctype.AdminRole
		tx.CurrentUserRoles = []string{doctype.AdminRole}
		tx.ScriptRunner = runner
		tx.ScriptStore = store
		providers[worker] = NewScriptProvider(&tx, registry, site, nil, nil)
	}
	kernelLoadMetrics.reset()
	before := database.Stats()
	latencies := make(chan time.Duration, workers*recordsPerWorker*3)
	errors := make(chan error, workers*recordsPerWorker)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-start
			provider := providers[workerID]
			for recordIndex := 0; recordIndex < recordsPerWorker; recordIndex++ {
				label := fmt.Sprintf("script-load-%02d-%02d", workerID, recordIndex)
				started := time.Now()
				created, err := provider.CreateDoc("TestDoc", map[string]any{"title": label}, "script-load-owner", fmt.Sprintf("script-load-%02d@example.test", workerID))
				latencies <- time.Since(started)
				if err != nil {
					errors <- fmt.Errorf("create %s: %w", label, err)
					continue
				}
				name, ok := created["name"].(string)
				if !ok || name == "" {
					errors <- fmt.Errorf("create %s returned no record name", label)
					continue
				}
				started = time.Now()
				err = provider.SaveDoc("TestDoc", map[string]any{"name": name, "title": label + "-updated"}, fmt.Sprintf("script-load-%02d@example.test", workerID))
				latencies <- time.Since(started)
				if err != nil {
					errors <- fmt.Errorf("update %s: %w", name, err)
					continue
				}
				started = time.Now()
				err = provider.DeleteDoc("TestDoc", name)
				latencies <- time.Since(started)
				if err != nil {
					errors <- fmt.Errorf("delete %s: %w", name, err)
				}
			}
		}(worker)
	}
	started := time.Now()
	close(start)
	wg.Wait()
	close(latencies)
	close(errors)
	var failures []error
	for err := range errors {
		failures = append(failures, err)
	}
	if len(failures) > 0 {
		t.Fatalf("script-provider load had %d failures; first: %v", len(failures), failures[0])
	}
	values := make([]time.Duration, 0, workers*recordsPerWorker*3)
	for latency := range latencies {
		values = append(values, latency)
	}
	if len(values) != workers*recordsPerWorker*3 {
		t.Fatalf("recorded %d mutations, want %d", len(values), workers*recordsPerWorker*3)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	after := database.Stats()
	seconds := time.Since(started).Seconds()
	statementCount, transactionTimes := kernelLoadMetrics.snapshot()
	sort.Slice(transactionTimes, func(i, j int) bool { return transactionTimes[i] < transactionTimes[j] })
	if statementCount == 0 || len(transactionTimes) != len(values) {
		t.Fatalf("SQL instrumentation recorded statements=%d transactions=%d for %d mutations", statementCount, len(transactionTimes), len(values))
	}
	t.Logf("script-provider load (active Goja validation hook): mutations=%d concurrency=%d errors=%d throughput=%.1f mutations/s p50=%s p95=%s p99=%s sql_statements=%d sql_per_mutation=%.1f tx_p50=%s tx_p95=%s tx_p99=%s pool_wait_count=%d pool_wait=%s open_conns=%d",
		len(values), workers, len(failures), float64(len(values))/seconds,
		percentile(values, .50), percentile(values, .95), percentile(values, .99),
		statementCount, float64(statementCount)/float64(len(values)),
		percentile(transactionTimes, .50), percentile(transactionTimes, .95), percentile(transactionTimes, .99),
		after.WaitCount-before.WaitCount, after.WaitDuration-before.WaitDuration, after.OpenConnections)
}
