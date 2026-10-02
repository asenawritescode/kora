//go:build integration

package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	koraai "github.com/asenawritescode/kora/api/ai"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/orm"
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
)

const kernelMeasuredMySQLDriverName = "kora-mysql-measured"

var kernelLoadMetrics = &kernelSQLMetrics{}

func init() {
	sql.Register(kernelMeasuredMySQLDriverName, &measuredSQLDriver{
		inner: &mysql.MySQLDriver{}, metrics: kernelLoadMetrics,
	})
}

type kernelSQLMetrics struct {
	statements atomic.Int64
	mu         sync.Mutex
	txTimes    []time.Duration
}

func (m *kernelSQLMetrics) reset() {
	m.statements.Store(0)
	m.mu.Lock()
	m.txTimes = nil
	m.mu.Unlock()
}

func (m *kernelSQLMetrics) recordStatement() { m.statements.Add(1) }

func (m *kernelSQLMetrics) recordTransaction(d time.Duration) {
	m.mu.Lock()
	m.txTimes = append(m.txTimes, d)
	m.mu.Unlock()
}

func (m *kernelSQLMetrics) snapshot() (int64, []time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statements.Load(), append([]time.Duration(nil), m.txTimes...)
}

// measuredSQLDriver is test-only. It counts executed SQL calls and transaction
// lifetimes without adding instrumentation work to the production DB path.
type measuredSQLDriver struct {
	inner   driver.Driver
	metrics *kernelSQLMetrics
}

func (d *measuredSQLDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &measuredSQLConn{Conn: conn, metrics: d.metrics}, nil
}

type measuredSQLConn struct {
	driver.Conn
	metrics *kernelSQLMetrics
}

func (c *measuredSQLConn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := c.Conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &measuredSQLStmt{Stmt: stmt, metrics: c.metrics}, nil
}

func (c *measuredSQLConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	var stmt driver.Stmt
	var err error
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		stmt, err = preparer.PrepareContext(ctx, query)
	} else {
		stmt, err = c.Conn.Prepare(query)
	}
	if err != nil {
		return nil, err
	}
	return &measuredSQLStmt{Stmt: stmt, metrics: c.metrics}, nil
}

func (c *measuredSQLConn) Begin() (driver.Tx, error) {
	started := time.Now()
	tx, err := c.Conn.Begin()
	if err != nil {
		return nil, err
	}
	return &measuredSQLTx{Tx: tx, metrics: c.metrics, started: started}, nil
}

func (c *measuredSQLConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if beginTx, ok := c.Conn.(driver.ConnBeginTx); ok {
		started := time.Now()
		tx, err := beginTx.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}
		return &measuredSQLTx{Tx: tx, metrics: c.metrics, started: started}, nil
	}
	if opts.Isolation != driver.IsolationLevel(0) || opts.ReadOnly {
		return nil, driver.ErrSkip
	}
	return c.Begin()
}

func (c *measuredSQLConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := execer.ExecContext(ctx, query, args)
	if !errors.Is(err, driver.ErrSkip) {
		c.metrics.recordStatement()
	}
	return result, err
}

func (c *measuredSQLConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if !errors.Is(err, driver.ErrSkip) {
		c.metrics.recordStatement()
	}
	return rows, err
}

type measuredSQLStmt struct {
	driver.Stmt
	metrics *kernelSQLMetrics
}

func (s *measuredSQLStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.metrics.recordStatement()
	return s.Stmt.Exec(args)
}

func (s *measuredSQLStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.metrics.recordStatement()
	return s.Stmt.Query(args)
}

func (s *measuredSQLStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if execer, ok := s.Stmt.(driver.StmtExecContext); ok {
		result, err := execer.ExecContext(ctx, args)
		if !errors.Is(err, driver.ErrSkip) {
			s.metrics.recordStatement()
		}
		return result, err
	}
	return nil, driver.ErrSkip
}

func (s *measuredSQLStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if queryer, ok := s.Stmt.(driver.StmtQueryContext); ok {
		rows, err := queryer.QueryContext(ctx, args)
		if !errors.Is(err, driver.ErrSkip) {
			s.metrics.recordStatement()
		}
		return rows, err
	}
	return nil, driver.ErrSkip
}

type measuredSQLTx struct {
	driver.Tx
	metrics *kernelSQLMetrics
	started time.Time
	once    sync.Once
}

func (tx *measuredSQLTx) Commit() error {
	err := tx.Tx.Commit()
	tx.once.Do(func() { tx.metrics.recordTransaction(time.Since(tx.started)) })
	return err
}

func (tx *measuredSQLTx) Rollback() error {
	err := tx.Tx.Rollback()
	tx.once.Do(func() { tx.metrics.recordTransaction(time.Since(tx.started)) })
	return err
}

// BenchmarkKernelRecordCRUD measures the MVP REST contract over the canonical
// kernel path, including create/update/delete and their transactional effects.
func BenchmarkKernelRecordCRUD(b *testing.B) {
	handler, database, registry, site := newResourceIntegrationHandler(b, kernelMeasuredMySQLDriverName)
	kernelLoadMetrics.reset()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		title := fmt.Sprintf("benchmark-%d", index)
		created := serveResourceMutationBody(b, handler, database, registry, site, httpMethodPost,
			"/api/resource/TestDoc", fmt.Sprintf(`{"title":%q,"serial":%q}`, title, title), "", []string{doctype.AdminRole})
		if created.Code != http.StatusCreated {
			b.Fatalf("create returned HTTP %d: %s", created.Code, created.Body.String())
		}
		doc := requireRESTDocument(b, created.Body.Bytes(), "TestDoc")
		name, ok := doc["name"].(string)
		if !ok || name == "" {
			b.Fatal("create response is missing the document name")
		}
		updated := serveResourceMutationBody(b, handler, database, registry, site, httpMethodPut,
			"/api/resource/TestDoc/"+name, fmt.Sprintf(`{"title":%q}`, title+"-updated"), "", []string{doctype.AdminRole})
		if updated.Code != http.StatusOK {
			b.Fatalf("update returned HTTP %d: %s", updated.Code, updated.Body.String())
		}
		deleted := serveResourceMutationBody(b, handler, database, registry, site, httpMethodDelete,
			"/api/resource/TestDoc/"+name, "", "", []string{doctype.AdminRole})
		if deleted.Code != http.StatusOK {
			b.Fatalf("delete returned HTTP %d: %s", deleted.Code, deleted.Body.String())
		}
	}
	b.ReportMetric(float64(3*b.N)/b.Elapsed().Seconds(), "mutations/s")
	statementCount, transactionTimes := kernelLoadMetrics.snapshot()
	b.ReportMetric(float64(statementCount)/float64(3*b.N), "sql/mutation")
	b.ReportMetric(meanDuration(transactionTimes).Seconds()*1e9, "tx-ns/op")
}

// BenchmarkPOSMutationBundle measures an atomic sale plus linked payment create.
func BenchmarkPOSMutationBundle(b *testing.B) {
	handler, database, registry, site := newResourceIntegrationHandler(b, kernelMeasuredMySQLDriverName)
	kernelLoadMetrics.reset()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		payload := posMutationBundle(index)
		ctx := kernelAdapterContext(b, database, registry, site)
		if _, _, cerr := handler.runKernelMutationBundle(ctx, payload, fmt.Sprintf("pos-bench-%d", index)); cerr != nil {
			b.Fatalf("POS bundle failed: %s: %s", cerr.Type, cerr.Message)
		}
	}
	b.ReportMetric(float64(2*b.N)/b.Elapsed().Seconds(), "mutations/s")
	statementCount, transactionTimes := kernelLoadMetrics.snapshot()
	b.ReportMetric(float64(statementCount)/float64(b.N), "sql/bundle")
	b.ReportMetric(meanDuration(transactionTimes).Seconds()*1e9, "tx-ns/bundle")
}

// TestKernelRecordCRUDLoad records request latency percentiles and database
// pool waits under concurrent independent MVP CRUD traffic. It intentionally
// reports performance instead of enforcing machine-specific timing limits.
func TestKernelRecordCRUDLoad(t *testing.T) {
	workers := perfLoadSetting(t, "KORA_PERF_WORKERS", 8, 1, 256)
	recordsPerWorker := perfLoadSetting(t, "KORA_PERF_RECORDS_PER_WORKER", 12, 1, 1000)
	if workers*recordsPerWorker > 10000 {
		t.Fatalf("load shape too large: workers*records_per_worker=%d exceeds 10000", workers*recordsPerWorker)
	}
	measureSQL := os.Getenv("KORA_PERF_DISABLE_SQL_INSTRUMENTATION") != "1"
	var handler *Handler
	var database *sql.DB
	var registry *doctype.Registry
	var site string
	if measureSQL {
		handler, database, registry, site = newResourceIntegrationHandler(t, kernelMeasuredMySQLDriverName)
	} else {
		handler, database, registry, site = newResourceIntegrationHandler(t)
	}
	if maxOpen := perfLoadSetting(t, "KORA_PERF_MAX_OPEN_CONNS", 0, 0, 512); maxOpen > 0 {
		database.SetMaxOpenConns(maxOpen)
	}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
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
			for recordIndex := 0; recordIndex < recordsPerWorker; recordIndex++ {
				serial := fmt.Sprintf("load-%02d-%02d", workerID, recordIndex)
				title := "load-test-" + serial
				created := timedResourceMutation(latencies, func() *httptest.ResponseRecorder {
					return serveResourceMutationBody(t, handler, database, registry, site, httpMethodPost,
						"/api/resource/TestDoc", fmt.Sprintf(`{"title":%q,"serial":%q}`, title, serial), "", []string{doctype.AdminRole})
				})
				if created.Code != http.StatusCreated {
					errors <- fmt.Errorf("create %s: HTTP %d: %s", serial, created.Code, created.Body.String())
					continue
				}
				doc, err := decodeLoadDocument(created.Body.Bytes())
				if err != nil {
					errors <- fmt.Errorf("decode %s: %w", serial, err)
					continue
				}
				name, ok := doc["name"].(string)
				if !ok || name == "" {
					errors <- fmt.Errorf("create %s returned no record name", serial)
					continue
				}
				updated := timedResourceMutation(latencies, func() *httptest.ResponseRecorder {
					return serveResourceMutationBody(t, handler, database, registry, site, httpMethodPut,
						"/api/resource/TestDoc/"+name, fmt.Sprintf(`{"title":%q}`, title+"-updated"), "", []string{doctype.AdminRole})
				})
				if updated.Code != http.StatusOK {
					errors <- fmt.Errorf("update %s: HTTP %d: %s", name, updated.Code, updated.Body.String())
					continue
				}
				deleted := timedResourceMutation(latencies, func() *httptest.ResponseRecorder {
					return serveResourceMutationBody(t, handler, database, registry, site, httpMethodDelete,
						"/api/resource/TestDoc/"+name, "", "", []string{doctype.AdminRole})
				})
				if deleted.Code != http.StatusOK {
					errors <- fmt.Errorf("delete %s: HTTP %d: %s", name, deleted.Code, deleted.Body.String())
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
		t.Fatalf("CRUD load had %d failures; first: %v", len(failures), failures[0])
	}
	values := make([]time.Duration, 0, workers*recordsPerWorker*3)
	for latency := range latencies {
		values = append(values, latency)
	}
	if len(values) != workers*recordsPerWorker*3 {
		t.Fatalf("recorded %d operations, want %d", len(values), workers*recordsPerWorker*3)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	after := database.Stats()
	seconds := time.Since(started).Seconds()
	statementCount, transactionTimes := kernelLoadMetrics.snapshot()
	sort.Slice(transactionTimes, func(i, j int) bool { return transactionTimes[i] < transactionTimes[j] })
	if measureSQL && (statementCount == 0 || len(transactionTimes) != len(values)) {
		t.Fatalf("SQL instrumentation recorded statements=%d transactions=%d for %d successful mutations", statementCount, len(transactionTimes), len(values))
	}
	var createdNames, distinctNames int
	if err := database.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT doc_name)
		FROM _kora_operation_audit
		WHERE site = ? AND command_name = 'record.create' AND status = 'completed'`, site).
		Scan(&createdNames, &distinctNames); err != nil {
		t.Fatalf("count generated names from create audit: %v", err)
	}
	wantCreates := workers * recordsPerWorker
	if createdNames != wantCreates || distinctNames != wantCreates {
		t.Fatalf("concurrent generated names: create audits=%d distinct names=%d, want %d unique creates", createdNames, distinctNames, wantCreates)
	}
	sqlPerMutation := float64(0)
	if len(values) > 0 {
		sqlPerMutation = float64(statementCount) / float64(len(values))
	}
	if measureSQL {
		t.Logf("CRUD load: operations=%d concurrency=%d errors=%d throughput=%.1f mutations/s p50=%s p95=%s p99=%s sql_statements=%d sql_per_mutation=%.1f tx_count=%d tx_p50=%s tx_p95=%s tx_p99=%s pool_wait_count=%d pool_wait=%s open_conns=%d max_open_conns=%d",
			len(values), workers, len(failures), float64(len(values))/seconds,
			percentile(values, .50), percentile(values, .95), percentile(values, .99),
			statementCount, sqlPerMutation, len(transactionTimes),
			percentile(transactionTimes, .50), percentile(transactionTimes, .95), percentile(transactionTimes, .99),
			after.WaitCount-before.WaitCount, after.WaitDuration-before.WaitDuration, after.OpenConnections, after.MaxOpenConnections)
	} else {
		t.Logf("CRUD load (driver instrumentation disabled): operations=%d concurrency=%d errors=%d throughput=%.1f mutations/s p50=%s p95=%s p99=%s pool_wait_count=%d pool_wait=%s open_conns=%d max_open_conns=%d",
			len(values), workers, len(failures), float64(len(values))/seconds,
			percentile(values, .50), percentile(values, .95), percentile(values, .99),
			after.WaitCount-before.WaitCount, after.WaitDuration-before.WaitDuration, after.OpenConnections, after.MaxOpenConnections)
	}
}

func perfLoadSetting(t *testing.T, name string, fallback, minimum, maximum int) int {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum || parsed > maximum {
		t.Fatalf("%s must be an integer from %d through %d, got %q", name, minimum, maximum, value)
	}
	return parsed
}

// TestPOSMutationBundleLoad measures the atomic checkout write path under the
// same bounded concurrent load as CRUD. It reports latency and DB resource
// data for review; machine-specific thresholds do not belong in this test.
func TestPOSMutationBundleLoad(t *testing.T) {
	const workers, bundlesPerWorker = 8, 12
	handler, database, registry, site := newResourceIntegrationHandler(t, kernelMeasuredMySQLDriverName)
	kernelLoadMetrics.reset()
	before := database.Stats()
	latencies := make(chan time.Duration, workers*bundlesPerWorker)
	errors := make(chan error, workers*bundlesPerWorker)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-start
			for bundleIndex := 0; bundleIndex < bundlesPerWorker; bundleIndex++ {
				index := workerID*bundlesPerWorker + bundleIndex
				ctx := kernelAdapterContext(t, database, registry, site)
				started := time.Now()
				result, replayed, cerr := handler.runKernelMutationBundle(ctx, posMutationBundle(index), fmt.Sprintf("pos-load-%02d-%02d", workerID, bundleIndex))
				latencies <- time.Since(started)
				if cerr != nil {
					errors <- fmt.Errorf("bundle %d: %s: %s", index, cerr.Type, cerr.Message)
					continue
				}
				if replayed || result == nil || result.Name == "" || len(result.Related) != 1 {
					errors <- fmt.Errorf("bundle %d returned invalid result (replayed=%t): %+v", index, replayed, result)
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
		t.Fatalf("POS bundle load had %d failures; first: %v", len(failures), failures[0])
	}
	values := make([]time.Duration, 0, workers*bundlesPerWorker)
	for latency := range latencies {
		values = append(values, latency)
	}
	if len(values) != workers*bundlesPerWorker {
		t.Fatalf("recorded %d bundles, want %d", len(values), workers*bundlesPerWorker)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	after := database.Stats()
	seconds := time.Since(started).Seconds()
	statementCount, transactionTimes := kernelLoadMetrics.snapshot()
	sort.Slice(transactionTimes, func(i, j int) bool { return transactionTimes[i] < transactionTimes[j] })
	if statementCount == 0 || len(transactionTimes) != len(values) {
		t.Fatalf("SQL instrumentation recorded statements=%d transactions=%d for %d bundles", statementCount, len(transactionTimes), len(values))
	}
	t.Logf("POS bundle load: bundles=%d concurrency=%d errors=%d throughput=%.1f bundles/s p50=%s p95=%s p99=%s sql_statements=%d sql_per_bundle=%.1f tx_p50=%s tx_p95=%s tx_p99=%s pool_wait_count=%d pool_wait=%s open_conns=%d",
		len(values), workers, len(failures), float64(len(values))/seconds,
		percentile(values, .50), percentile(values, .95), percentile(values, .99),
		statementCount, float64(statementCount)/float64(len(values)),
		percentile(transactionTimes, .50), percentile(transactionTimes, .95), percentile(transactionTimes, .99),
		after.WaitCount-before.WaitCount, after.WaitDuration-before.WaitDuration, after.OpenConnections)
}

// TestMPesaCallbackLoad measures the kernel-backed provider callback update
// path under concurrent delivery. Each request has an independent operation
// ID so the sample measures throughput rather than duplicate-key contention.
func TestMPesaCallbackLoad(t *testing.T) {
	const workers, callbacksPerWorker = 8, 12
	handler, database, registry, site := newResourceIntegrationHandler(t, kernelMeasuredMySQLDriverName)
	registerPOSIntegrationTypes(t, database, site, registry)
	operationTable := "`" + registry.Get("External Operation").RawTableName() + "`"
	paymentTable := "`" + registry.Get("Payment").RawTableName() + "`"
	for index := 0; index < workers*callbacksPerWorker; index++ {
		suffix := fmt.Sprintf("mpesa-load-%03d", index)
		if _, err := database.Exec("INSERT INTO "+operationTable+" (name, operation_type, purpose, source_doctype, provider, status, provider_request_id) VALUES (?, ?, ?, ?, ?, ?, ?)",
			"OP-"+suffix, "Payment", "POS payment", "Sale", "M-Pesa", "Pending", "checkout-"+suffix); err != nil {
			t.Fatalf("seed operation %s: %v", suffix, err)
		}
		if _, err := database.Exec("INSERT INTO "+paymentTable+" (name, reference, amount, method, external_operation, status) VALUES (?, ?, ?, ?, ?, ?)", "PAY-"+suffix, "REF-"+suffix, 500, "mpesa", "OP-"+suffix, "Pending"); err != nil {
			t.Fatalf("seed payment %s: %v", suffix, err)
		}
	}
	kernelLoadMetrics.reset()
	before := database.Stats()
	latencies := make(chan time.Duration, workers*callbacksPerWorker)
	errors := make(chan error, workers*callbacksPerWorker)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-start
			for callbackIndex := 0; callbackIndex < callbacksPerWorker; callbackIndex++ {
				index := workerID*callbacksPerWorker + callbackIndex
				suffix := fmt.Sprintf("mpesa-load-%03d", index)
				body := fmt.Sprintf(`{"Body":{"stkCallback":{"CheckoutRequestID":"checkout-%s","ResultCode":0,"ResultDesc":"Success","CallbackMetadata":{"Item":[{"Name":"MpesaReceiptNumber","Value":"RCP-%s"}]}}}}`, suffix, suffix)
				started := time.Now()
				response := serveMPesaCallbackDialect(t, handler, database, registry, site, "mysql", body)
				latencies <- time.Since(started)
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"payment_status":"Succeeded"`) {
					errors <- fmt.Errorf("callback %s returned HTTP %d: %s", suffix, response.Code, response.Body.String())
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
		t.Fatalf("M-Pesa callback load had %d failures; first: %v", len(failures), failures[0])
	}
	values := make([]time.Duration, 0, workers*callbacksPerWorker)
	for latency := range latencies {
		values = append(values, latency)
	}
	if len(values) != workers*callbacksPerWorker {
		t.Fatalf("recorded %d callbacks, want %d", len(values), workers*callbacksPerWorker)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	after := database.Stats()
	seconds := time.Since(started).Seconds()
	statementCount, transactionTimes := kernelLoadMetrics.snapshot()
	sort.Slice(transactionTimes, func(i, j int) bool { return transactionTimes[i] < transactionTimes[j] })
	if statementCount == 0 || len(transactionTimes) != len(values) {
		t.Fatalf("SQL instrumentation recorded statements=%d transactions=%d for %d callbacks", statementCount, len(transactionTimes), len(values))
	}
	t.Logf("M-Pesa callback load: callbacks=%d concurrency=%d errors=%d throughput=%.1f callbacks/s p50=%s p95=%s p99=%s sql_statements=%d sql_per_callback=%.1f tx_p50=%s tx_p95=%s tx_p99=%s pool_wait_count=%d pool_wait=%s open_conns=%d",
		len(values), workers, len(failures), float64(len(values))/seconds,
		percentile(values, .50), percentile(values, .95), percentile(values, .99),
		statementCount, float64(statementCount)/float64(len(values)),
		percentile(transactionTimes, .50), percentile(transactionTimes, .95), percentile(transactionTimes, .99),
		after.WaitCount-before.WaitCount, after.WaitDuration-before.WaitDuration, after.OpenConnections)
}

// TestAIMutationLoad measures the confirmed AI tool-to-kernel mutation adapter
// without involving an LLM/provider call. The sample isolates the write path
// from variable model latency while retaining tool mapping and kernel costs.
func TestAIMutationLoad(t *testing.T) {
	const workers, mutationsPerWorker = 8, 12
	handler, database, registry, site := newResourceIntegrationHandler(t, kernelMeasuredMySQLDriverName)
	transactions := make([]*orm.TxManager, workers)
	for worker := 0; worker < workers; worker++ {
		tx := *handler.TxManager
		tx.Context = context.Background()
		tx.CurrentUser = fmt.Sprintf("ai-load-%02d@example.test", worker)
		tx.CurrentUserRole = doctype.AdminRole
		tx.CurrentUserRoles = []string{doctype.AdminRole}
		transactions[worker] = &tx
	}
	kernelLoadMetrics.reset()
	before := database.Stats()
	latencies := make(chan time.Duration, workers*mutationsPerWorker)
	errors := make(chan error, workers*mutationsPerWorker)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-start
			tx := transactions[workerID]
			user := fmt.Sprintf("ai-load-%02d@example.test", workerID)
			for mutationIndex := 0; mutationIndex < mutationsPerWorker; mutationIndex++ {
				key := fmt.Sprintf("ai-load-%02d-%02d", workerID, mutationIndex)
				args := map[string]any{"title": key}
				started := time.Now()
				result := koraai.ExecuteConfirmedToolWithIdempotencyKey(tx, registry, "TestDoc_create", args, user, site, key)
				latencies <- time.Since(started)
				if !strings.HasPrefix(result, `Created TestDoc "`) {
					errors <- fmt.Errorf("AI create %s returned %q", key, result)
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
		t.Fatalf("AI mutation load had %d failures; first: %v", len(failures), failures[0])
	}
	values := make([]time.Duration, 0, workers*mutationsPerWorker)
	for latency := range latencies {
		values = append(values, latency)
	}
	if len(values) != workers*mutationsPerWorker {
		t.Fatalf("recorded %d AI mutations, want %d", len(values), workers*mutationsPerWorker)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	after := database.Stats()
	seconds := time.Since(started).Seconds()
	statementCount, transactionTimes := kernelLoadMetrics.snapshot()
	sort.Slice(transactionTimes, func(i, j int) bool { return transactionTimes[i] < transactionTimes[j] })
	if statementCount == 0 || len(transactionTimes) != len(values) {
		t.Fatalf("SQL instrumentation recorded statements=%d transactions=%d for %d AI mutations", statementCount, len(transactionTimes), len(values))
	}
	t.Logf("AI mutation load (no LLM call): mutations=%d concurrency=%d errors=%d throughput=%.1f mutations/s p50=%s p95=%s p99=%s sql_statements=%d sql_per_mutation=%.1f tx_p50=%s tx_p95=%s tx_p99=%s pool_wait_count=%d pool_wait=%s open_conns=%d",
		len(values), workers, len(failures), float64(len(values))/seconds,
		percentile(values, .50), percentile(values, .95), percentile(values, .99),
		statementCount, float64(statementCount)/float64(len(values)),
		percentile(transactionTimes, .50), percentile(transactionTimes, .95), percentile(transactionTimes, .99),
		after.WaitCount-before.WaitCount, after.WaitDuration-before.WaitDuration, after.OpenConnections)
}

// TestExternalOperationInitiationLoad measures the local operation/payment/
// event bundle used to start an external payment attempt. The fixture has no
// provider script, so no network request or transaction spans provider I/O.
func TestExternalOperationInitiationLoad(t *testing.T) {
	const workers, operationsPerWorker = 8, 12
	handler, database, registry, site := newResourceIntegrationHandler(t, kernelMeasuredMySQLDriverName)
	registerPOSIntegrationTypes(t, database, site, registry)
	registry.Views.Register(&doctype.View{
		Name: "Payment initiation load", Route: "/payment-load", Type: "register", SourceDocType: "Sale",
		Components: []doctype.ViewComponent{{ID: "payment", Actions: []doctype.ViewAction{{
			ID: "initiate", Trigger: "on_click", Type: "initiate_external_operation",
			Config: map[string]any{"operation_type": "Payment", "purpose": "POS sale payment", "source_doctype": "Sale", "provider": "M-Pesa"},
		}}}},
	})
	kernelLoadMetrics.reset()
	before := database.Stats()
	latencies := make(chan time.Duration, workers*operationsPerWorker)
	errors := make(chan error, workers*operationsPerWorker)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-start
			for operationIndex := 0; operationIndex < operationsPerWorker; operationIndex++ {
				clientReference := fmt.Sprintf("operation-load-%02d-%02d", workerID, operationIndex)
				request := map[string]any{"client_reference": clientReference, "total": 250.0, "payment_method": "mpesa", "customer_phone": "+254700000001"}
				started := time.Now()
				response := serveManifestAction(t, handler, site, database, registry, "initiate", request)
				latencies <- time.Since(started)
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"Pending"`) {
					errors <- fmt.Errorf("external initiation %s returned HTTP %d: %s", clientReference, response.Code, response.Body.String())
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
		t.Fatalf("external-operation load had %d failures; first: %v", len(failures), failures[0])
	}
	values := make([]time.Duration, 0, workers*operationsPerWorker)
	for latency := range latencies {
		values = append(values, latency)
	}
	if len(values) != workers*operationsPerWorker {
		t.Fatalf("recorded %d operation initiations, want %d", len(values), workers*operationsPerWorker)
	}
	var persisted int
	if err := database.QueryRow("SELECT COUNT(*) FROM `tabExternal Operation`").Scan(&persisted); err != nil {
		t.Fatalf("count initiated external operations: %v", err)
	}
	if persisted != len(values) {
		t.Fatalf("persisted external operations=%d, want %d", persisted, len(values))
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	after := database.Stats()
	seconds := time.Since(started).Seconds()
	statementCount, transactionTimes := kernelLoadMetrics.snapshot()
	sort.Slice(transactionTimes, func(i, j int) bool { return transactionTimes[i] < transactionTimes[j] })
	if statementCount == 0 || len(transactionTimes) < len(values) {
		t.Fatalf("SQL instrumentation recorded statements=%d transactions=%d for %d initiations", statementCount, len(transactionTimes), len(values))
	}
	t.Logf("external-operation initiation load: operations=%d concurrency=%d errors=%d throughput=%.1f operations/s p50=%s p95=%s p99=%s sql_statements=%d sql_per_operation=%.1f transactions=%d tx_per_operation=%.1f tx_p50=%s tx_p95=%s tx_p99=%s pool_wait_count=%d pool_wait=%s open_conns=%d",
		len(values), workers, len(failures), float64(len(values))/seconds,
		percentile(values, .50), percentile(values, .95), percentile(values, .99),
		statementCount, float64(statementCount)/float64(len(values)), len(transactionTimes), float64(len(transactionTimes))/float64(len(values)),
		percentile(transactionTimes, .50), percentile(transactionTimes, .95), percentile(transactionTimes, .99),
		after.WaitCount-before.WaitCount, after.WaitDuration-before.WaitDuration, after.OpenConnections)
}

func posMutationBundle(index int) kernel.RecordMutationBundlePayload {
	saleData, _ := json.Marshal(map[string]any{"title": fmt.Sprintf("POS sale %d", index), "total_amount": 500})
	paymentData, _ := json.Marshal(map[string]any{"sales_invoice": "$records.sale.name", "amount": 500, "method": "Cash"})
	return kernel.RecordMutationBundlePayload{Doctype: "Sales Invoice", Records: []kernel.RecordMutationBundleItem{
		{Key: "sale", Doctype: "Sales Invoice", Data: saleData},
		{Key: "payment", Doctype: "Payment", Data: paymentData},
	}}
}

func kernelAdapterContext(tb testing.TB, database *sql.DB, registry *doctype.Registry, site string, databaseType ...string) *gin.Context {
	tb.Helper()
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/kernel/record.mutate_bundle", nil)
	ctx.Set("site_name", site)
	ctx.Set("site_db", database)
	ctx.Set("site_registry", registry)
	dbType := "mysql"
	if len(databaseType) > 0 && databaseType[0] != "" {
		dbType = databaseType[0]
	}
	ctx.Set("site_db_type", dbType)
	ctx.Set("user", "pos-benchmark@example.test")
	ctx.Set("user_role", doctype.AdminRole)
	ctx.Set("user_roles", []string{doctype.AdminRole})
	return ctx
}

func timedResourceMutation(latencies chan<- time.Duration, mutate func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	started := time.Now()
	response := mutate()
	latencies <- time.Since(started)
	return response
}

func decodeLoadDocument(body []byte) (map[string]any, error) {
	var response struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	if response.Data == nil {
		return nil, fmt.Errorf("response has no document")
	}
	return response.Data, nil
}

func percentile(values []time.Duration, fraction float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	index := int(float64(len(values)-1) * fraction)
	return values[index]
}

func meanDuration(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	var total time.Duration
	for _, value := range values {
		total += value
	}
	return total / time.Duration(len(values))
}
