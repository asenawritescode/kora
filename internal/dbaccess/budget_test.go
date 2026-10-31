package dbaccess

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestBudgetBoundsConcurrentConnectionsAndWaiters(t *testing.T) {
	budget, err := New(Config{MaxConnections: 1, MaxWaiters: 1, Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	releaseFirst, err := budget.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()

	acquired := make(chan func(), 1)
	failed := make(chan error, 1)
	go func() {
		release, err := budget.Acquire(context.Background())
		if err != nil {
			failed <- err
			return
		}
		acquired <- release
	}()
	deadline := time.Now().Add(time.Second)
	for budget.Snapshot().Waiters != 1 {
		if time.Now().After(deadline) {
			t.Fatal("second caller did not enter bounded wait queue")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := budget.Acquire(context.Background()); !errors.Is(err, ErrExhausted) {
		t.Fatalf("third caller error = %v, want ErrExhausted", err)
	}

	releaseFirst()
	select {
	case release := <-acquired:
		release()
	case err := <-failed:
		t.Fatalf("queued caller failed after capacity was released: %v", err)
	case <-time.After(time.Second):
		t.Fatal("queued caller did not acquire released capacity")
	}
	if got := budget.Snapshot(); got.InUse != 0 || got.Waiters != 0 || got.Rejected != 1 || got.WaitCount != 1 || got.WaitDuration <= 0 {
		t.Fatalf("unexpected final budget snapshot: %#v", got)
	}
}

func TestBudgetWaitHasDeadlineAndHonorsCancellation(t *testing.T) {
	budget, err := New(Config{MaxConnections: 1, MaxWaiters: 1, Wait: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	release, err := budget.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := budget.Acquire(context.Background()); !errors.Is(err, ErrExhausted) {
		t.Fatalf("saturated acquire error = %v, want ErrExhausted", err)
	}
	if elapsed := time.Since(started); elapsed < 15*time.Millisecond || elapsed > time.Second {
		t.Fatalf("bounded wait took %s", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		cancel()
	}()
	if _, err := budget.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire error = %v, want context.Canceled", err)
	}
	wg.Wait()
	release()
	if got := budget.Snapshot(); got.Waiters != 0 || got.WaitCount != 2 || got.Rejected != 2 {
		t.Fatalf("waiters were not retired after timeout/cancellation: %#v", got)
	}
}

func TestBudgetConnectorTracksOperationDurationAndLockContention(t *testing.T) {
	budget, err := New(Config{MaxConnections: 1, MaxWaiters: 0, Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	connection := &budgetConn{Conn: fakeConn{execErr: errors.New("database is locked")}, budget: budget, release: func() {}}
	if _, err := connection.ExecContext(context.Background(), "UPDATE records SET value = ?", nil); err == nil {
		t.Fatal("locked fake query unexpectedly succeeded")
	}
	connection.Conn = fakeConn{execErr: errors.New("constraint violation")}
	if _, err := connection.ExecContext(context.Background(), "UPDATE records SET value = ?", nil); err == nil {
		t.Fatal("fake query unexpectedly succeeded")
	}
	snapshot := budget.Snapshot()
	if snapshot.OperationCount != 2 || snapshot.LockContention != 1 || snapshot.OperationDuration < 0 {
		t.Fatalf("operation telemetry = %#v", snapshot)
	}
}

type sqlStateTestError string

func (e sqlStateTestError) Error() string    { return string(e) }
func (e sqlStateTestError) SQLState() string { return string(e) }

func TestLockContentionClassifierCoversSupportedDriverSignals(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "mysql lock timeout", err: &mysql.MySQLError{Number: 1205}, want: true},
		{name: "mysql deadlock", err: &mysql.MySQLError{Number: 1213}, want: true},
		{name: "postgres lock not available", err: sqlStateTestError("55P03"), want: true},
		{name: "postgres deadlock", err: sqlStateTestError("40P01"), want: true},
		{name: "libsql busy", err: errors.New("SQLITE_BUSY: database is locked"), want: true},
		{name: "unrelated error", err: errors.New("constraint violation"), want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isLockContention(test.err); got != test.want {
				t.Fatalf("isLockContention(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}

type cancelOnSecondErrContext struct {
	calls atomic.Int32
	done  chan struct{}
}

func newCancelOnSecondErrContext() *cancelOnSecondErrContext {
	return &cancelOnSecondErrContext{done: make(chan struct{})}
}

func (c *cancelOnSecondErrContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelOnSecondErrContext) Done() <-chan struct{}       { return c.done }
func (c *cancelOnSecondErrContext) Value(any) any               { return nil }
func (c *cancelOnSecondErrContext) Err() error {
	if c.calls.Add(1) == 2 {
		close(c.done)
		return context.Canceled
	}
	return nil
}

func TestBudgetReleasesPermitWhenContextCancelsDuringAcquire(t *testing.T) {
	budget, err := New(Config{MaxConnections: 1, MaxWaiters: 1, Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := budget.Acquire(newCancelOnSecondErrContext()); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire error = %v, want cancellation", err)
	}
	if got := budget.Snapshot(); got.InUse != 0 || got.Rejected != 1 {
		t.Fatalf("canceled acquisition leaked a connection permit: %#v", got)
	}
}

func TestConfigFromEnvDividesPhysicalServerBudgetAcrossReplicas(t *testing.T) {
	t.Setenv("KORA_DB_CONNECTION_BUDGET", "64")
	t.Setenv("KORA_DB_SERVER_MAX_CONNECTIONS", "240")
	t.Setenv("KORA_DB_SERVER_RESERVED_CONNECTIONS", "40")
	t.Setenv("KORA_DB_ENGINE_REPLICAS", "4")
	t.Setenv("KORA_DB_SERVER_MAX_HEAVY_JOBS", "4")
	t.Setenv("KORA_DB_CONNECTION_WAITERS", "10")
	t.Setenv("KORA_DB_CONNECTION_WAIT_MS", "750")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConnections != 50 || cfg.MaxWaiters != 10 || cfg.Wait != 750*time.Millisecond || cfg.MaxHeavyJobs != 1 {
		t.Fatalf("configured allocation = %#v", cfg)
	}
}

func TestConfigFromEnvRejectsHeavyAllocationWithoutInteractiveCapacity(t *testing.T) {
	t.Setenv("KORA_DB_CONNECTION_BUDGET", "8")
	t.Setenv("KORA_DB_SERVER_MAX_CONNECTIONS", "0")
	t.Setenv("KORA_DB_ENGINE_REPLICAS", "2")
	t.Setenv("KORA_DB_ACTIVE_WORK_BUDGET", "2")
	t.Setenv("KORA_DB_SERVER_MAX_HEAVY_JOBS", "4")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("heavy job allocation that consumes all active capacity was accepted")
	}
}

func TestBudgetConnectorReleasesPermitWhenConnectionCloses(t *testing.T) {
	budget, err := New(Config{MaxConnections: 1, MaxWaiters: 0, Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	connector := budgetConnector{base: fakeConnector{}, budget: budget}
	conn, err := connector.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := budget.Snapshot().InUse; got != 1 {
		t.Fatalf("in-use after connect = %d, want 1", got)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if got := budget.Snapshot().InUse; got != 0 {
		t.Fatalf("in-use after close = %d, want 0", got)
	}
}

func TestServerKeyExcludesCredentials(t *testing.T) {
	t.Setenv("KORA_DB_BUDGET_KEY", "")
	key := ServerKey("mysql", "secret:password@tcp(db.example:3306)/tenant")
	if key != "mysql:tcp:db.example:3306" {
		t.Fatalf("server key = %q", key)
	}
	if strings.Contains(key, "secret") || strings.Contains(key, "password") || strings.Contains(key, "tenant") {
		t.Fatalf("server key includes sensitive or tenant-specific data: %q", key)
	}
}

func TestServerKeySharesPhysicalEndpointAcrossTenantDatabases(t *testing.T) {
	t.Setenv("KORA_DB_BUDGET_KEY", "")
	cases := []struct {
		driver string
		first  string
		second string
		secret []string
	}{
		{
			driver: "mysql",
			first:  "alice:first-secret@tcp(db.example:3306)/tenant_a?parseTime=true",
			second: "bob:second-secret@tcp(db.example:3306)/tenant_b?charset=utf8mb4",
			secret: []string{"alice", "bob", "first-secret", "second-secret", "tenant_a", "tenant_b"},
		},
		{
			driver: "postgres",
			first:  "postgres://alice:first-secret@db.example:5432/tenant_a?sslmode=disable",
			second: "postgres://bob:second-secret@db.example:5432/tenant_b?sslmode=require",
			secret: []string{"alice", "bob", "first-secret", "second-secret", "tenant_a", "tenant_b"},
		},
		{
			driver: "libsql",
			first:  "http://db.example:8080/tenant_a?authToken=first-secret",
			second: "http://db.example:8080/tenant_b?authToken=second-secret",
			secret: []string{"first-secret", "second-secret", "tenant_a", "tenant_b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.driver, func(t *testing.T) {
			first := ServerKey(tc.driver, tc.first)
			second := ServerKey(tc.driver, tc.second)
			if first != second {
				t.Fatalf("server keys differ for tenant pools on one endpoint: %q != %q", first, second)
			}
			for _, sensitive := range tc.secret {
				if strings.Contains(first, sensitive) {
					t.Fatalf("server key %q contains tenant credential/database value %q", first, sensitive)
				}
			}
		})
	}
}

type fakeConnector struct{}

func (fakeConnector) Connect(context.Context) (driver.Conn, error) { return fakeConn{}, nil }
func (fakeConnector) Driver() driver.Driver                        { return &fakeDriver{} }

type fakeDriver struct{ live atomic.Int32 }

func (d *fakeDriver) Open(string) (driver.Conn, error) {
	d.live.Add(1)
	return fakeConn{driver: d}, nil
}
func (d *fakeDriver) OpenConnector(string) (driver.Connector, error) {
	return countedFakeConnector{driver: d}, nil
}

type countedFakeConnector struct{ driver *fakeDriver }

func (c countedFakeConnector) Connect(context.Context) (driver.Conn, error) {
	c.driver.live.Add(1)
	return fakeConn{driver: c.driver}, nil
}
func (c countedFakeConnector) Driver() driver.Driver { return c.driver }

type fakeConn struct {
	driver  *fakeDriver
	execErr error
}

func (fakeConn) Prepare(string) (driver.Stmt, error) { return nil, io.EOF }
func (c fakeConn) Close() error {
	if c.driver != nil {
		c.driver.live.Add(-1)
	}
	return nil
}
func (fakeConn) Begin() (driver.Tx, error)  { return nil, io.EOF }
func (fakeConn) Ping(context.Context) error { return nil }
func (c fakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return nil, c.execErr
}

func TestOpenSharesConnectionBudgetAcrossSQLPools(t *testing.T) {
	const driverName = "dbaccess-budget-test"
	driverImpl := budgetTestDriver
	t.Setenv("KORA_DB_BUDGET_KEY", "dbaccess-budget-test-server")
	t.Setenv("KORA_DB_CONNECTION_BUDGET", "1")
	t.Setenv("KORA_DB_SERVER_MAX_CONNECTIONS", "0")
	t.Setenv("KORA_DB_SERVER_RESERVED_CONNECTIONS", "0")
	t.Setenv("KORA_DB_ENGINE_REPLICAS", "1")
	t.Setenv("KORA_DB_CONNECTION_WAITERS", "1")
	t.Setenv("KORA_DB_CONNECTION_WAIT_MS", "1000")

	first, err := Open(driverName, "first")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(driverName, "second")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	first.SetMaxOpenConns(1)
	second.SetMaxOpenConns(1)

	firstConn, err := first.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := second.Conn(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second pool saturated error = %v, want context deadline", err)
	}
	if got := driverImpl.live.Load(); got != 1 {
		t.Fatalf("simultaneous physical connections = %d, want shared cap 1", got)
	}
	if err := firstConn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	secondConn, err := second.Conn(context.Background())
	if err != nil {
		t.Fatalf("connection was not admitted after release: %v", err)
	}
	if err := secondConn.Close(); err != nil {
		t.Fatal(err)
	}
	if got := driverImpl.live.Load(); got != 1 {
		t.Fatalf("live connections after handoff = %d, want 1", got)
	}
}

func TestOpenReleasesEndpointRegistryAfterLastPoolCloses(t *testing.T) {
	const driverName = "dbaccess-budget-test"
	t.Setenv("KORA_DB_BUDGET_KEY", "pool-lifecycle:"+t.Name())
	t.Setenv("KORA_DB_MAX_BUDGET_ENDPOINTS", "100")
	t.Setenv("KORA_DB_CONNECTION_BUDGET", "2")
	t.Setenv("KORA_DB_SERVER_MAX_CONNECTIONS", "0")
	t.Setenv("KORA_DB_SERVER_RESERVED_CONNECTIONS", "0")
	t.Setenv("KORA_DB_ENGINE_REPLICAS", "1")
	t.Setenv("KORA_DB_CONNECTION_WAITERS", "2")
	t.Setenv("KORA_DB_CONNECTION_WAIT_MS", "100")
	key := ServerKey(driverName, "first")

	first, err := Open(driverName, "first")
	if err != nil {
		t.Fatal("open first pool:", err)
	}
	second, err := Open(driverName, "second")
	if err != nil {
		_ = first.Close()
		t.Fatal("open second pool:", err)
	}
	budgets.Lock()
	entry := budgets.byServer[key]
	poolRefs := 0
	if entry != nil {
		poolRefs = entry.pools
	}
	budgets.Unlock()
	if poolRefs != 2 {
		t.Fatalf("endpoint pool refs = %d, want 2", poolRefs)
	}
	sharedBudget := entry.budget
	if err := first.Close(); err != nil {
		t.Fatal("close first pool:", err)
	}
	if budget, err := budgetFor(key); err != nil || budget != sharedBudget {
		t.Fatalf("shared budget disappeared while second pool remained: budget=%v error=%v", budget, err)
	}
	if err := second.Close(); err != nil {
		t.Fatal("close second pool:", err)
	}
	budgets.Lock()
	entry = budgets.byServer[key]
	budgets.Unlock()
	if entry != nil {
		t.Fatal("endpoint budget remained registered after its last SQL pool closed")
	}
}

func TestEndpointBudgetRemainsSharedWhileClosedPoolHasLiveConnection(t *testing.T) {
	const driverName = "dbaccess-budget-test"
	t.Setenv("KORA_DB_BUDGET_KEY", "pool-live-connection:"+t.Name())
	t.Setenv("KORA_DB_MAX_BUDGET_ENDPOINTS", "100")
	t.Setenv("KORA_DB_CONNECTION_BUDGET", "1")
	t.Setenv("KORA_DB_SERVER_MAX_CONNECTIONS", "0")
	t.Setenv("KORA_DB_SERVER_RESERVED_CONNECTIONS", "0")
	t.Setenv("KORA_DB_ENGINE_REPLICAS", "1")
	t.Setenv("KORA_DB_CONNECTION_WAITERS", "0")
	t.Setenv("KORA_DB_CONNECTION_WAIT_MS", "100")

	first, err := Open(driverName, "first")
	if err != nil {
		t.Fatal("open first pool:", err)
	}
	firstConn, err := first.Conn(context.Background())
	if err != nil {
		_ = first.Close()
		t.Fatal("acquire first connection:", err)
	}
	key := ServerKey(driverName, "first")
	if err := first.Close(); err != nil {
		_ = firstConn.Close()
		t.Fatal("close first pool with a live connection:", err)
	}

	second, err := Open(driverName, "second")
	if err != nil {
		_ = firstConn.Close()
		t.Fatal("open second pool:", err)
	}
	defer second.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := second.Conn(ctx); !errors.Is(err, ErrExhausted) && !errors.Is(err, context.DeadlineExceeded) {
		_ = firstConn.Close()
		t.Fatalf("second pool bypassed live connection budget: %v", err)
	}
	budgets.Lock()
	entry := budgets.byServer[key]
	budgets.Unlock()
	if entry == nil || entry.budget.Snapshot().InUse != 1 {
		_ = firstConn.Close()
		t.Fatalf("shared endpoint budget while old connection remained = %#v, want one active permit", entry)
	}
	if err := firstConn.Close(); err != nil {
		t.Fatal("close old connection:", err)
	}
	secondConn, err := second.Conn(context.Background())
	if err != nil {
		t.Fatalf("replacement pool was not admitted after old connection closed: %v", err)
	}
	if err := secondConn.Close(); err != nil {
		t.Fatal("close replacement connection:", err)
	}
}

func TestAdmissionForSharesActiveWorkBudgetAcrossSQLPools(t *testing.T) {
	t.Setenv("KORA_DB_BUDGET_KEY", "shared-active-work-test")
	t.Setenv("KORA_DB_CONNECTION_BUDGET", "2")
	t.Setenv("KORA_DB_SERVER_MAX_CONNECTIONS", "0")
	t.Setenv("KORA_DB_SERVER_RESERVED_CONNECTIONS", "0")
	t.Setenv("KORA_DB_ENGINE_REPLICAS", "1")
	t.Setenv("KORA_DB_ACTIVE_WORK_BUDGET", "1")
	t.Setenv("KORA_DB_ACTIVE_WORK_WAITERS", "0")
	t.Setenv("KORA_DB_ACTIVE_WORK_WAIT_MS", "100")

	first, err := AdmissionFor("mysql", "first-database-dsn")
	if err != nil {
		t.Fatal(err)
	}
	second, err := AdmissionFor("mysql", "second-database-dsn")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("pools sharing a physical server received different active-work admission")
	}
	release, err := first.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := second.Acquire(context.Background()); !errors.Is(err, ErrExhausted) {
		t.Fatalf("second pool admission error = %v, want shared capacity rejection", err)
	}
}

type budgetDriverArg struct{ value int64 }

type optionalDriver struct {
	execCalls  atomic.Int32
	queryCalls atomic.Int32
	argChecks  atomic.Int32
}

func (d *optionalDriver) Open(string) (driver.Conn, error) { return &optionalConn{driver: d}, nil }
func (d *optionalDriver) OpenConnector(string) (driver.Connector, error) {
	return optionalConnector{driver: d}, nil
}

type optionalConnector struct{ driver *optionalDriver }

func (c optionalConnector) Connect(context.Context) (driver.Conn, error) {
	return &optionalConn{driver: c.driver}, nil
}
func (c optionalConnector) Driver() driver.Driver { return c.driver }

type optionalConn struct{ driver *optionalDriver }

func (c *optionalConn) Prepare(string) (driver.Stmt, error) { return nil, io.EOF }
func (c *optionalConn) Close() error                        { return nil }
func (c *optionalConn) Begin() (driver.Tx, error)           { return nil, io.EOF }
func (c *optionalConn) Exec(_ string, args []driver.Value) (driver.Result, error) {
	if len(args) != 1 || args[0] != int64(9) {
		return nil, errors.New("wrapped driver did not receive normalized argument")
	}
	c.driver.execCalls.Add(1)
	return driver.RowsAffected(1), nil
}
func (c *optionalConn) Query(string, []driver.Value) (driver.Rows, error) {
	c.driver.queryCalls.Add(1)
	return &optionalRows{}, nil
}
func (c *optionalConn) CheckNamedValue(value *driver.NamedValue) error {
	custom, ok := value.Value.(budgetDriverArg)
	if !ok {
		return driver.ErrSkip
	}
	c.driver.argChecks.Add(1)
	value.Value = custom.value
	return nil
}
func (c *optionalConn) IsValid() bool { return true }

type optionalRows struct{ read bool }

func (*optionalRows) Columns() []string { return []string{"value"} }
func (r *optionalRows) Close() error    { return nil }
func (r *optionalRows) Next(values []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	values[0] = "through-wrapper"
	return nil
}

func TestBudgetConnectionPreservesUnderlyingDriverOptionalInterfaces(t *testing.T) {
	driverImpl := &optionalDriver{}
	budget, err := New(Config{MaxConnections: 1, MaxWaiters: 1, Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	database := sql.OpenDB(&budgetConnector{base: optionalConnector{driver: driverImpl}, budget: budget})
	defer database.Close()

	result, err := database.ExecContext(context.Background(), "custom exec", budgetDriverArg{value: 9})
	if err != nil {
		t.Fatal("ExecContext through budget wrapper:", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		t.Fatalf("ExecContext result = %d, %v", count, err)
	}
	var got string
	if err := database.QueryRowContext(context.Background(), "custom query").Scan(&got); err != nil {
		t.Fatal("QueryContext through budget wrapper:", err)
	}
	if got != "through-wrapper" {
		t.Fatalf("QueryContext value = %q", got)
	}
	if driverImpl.execCalls.Load() != 1 || driverImpl.queryCalls.Load() != 1 || driverImpl.argChecks.Load() != 1 {
		t.Fatalf("wrapped driver optional calls: exec=%d query=%d argument_checks=%d", driverImpl.execCalls.Load(), driverImpl.queryCalls.Load(), driverImpl.argChecks.Load())
	}
}

var (
	registerBudgetTestDriver sync.Once
	budgetTestDriver         = &fakeDriver{}
)

func init() {
	registerBudgetTestDriver.Do(func() { sql.Register("dbaccess-budget-test", budgetTestDriver) })
}

func TestOpenBuildsConnectorsForSupportedDialectsWithoutConnecting(t *testing.T) {
	t.Setenv("KORA_DB_BUDGET_KEY", "dbaccess-supported-dialects")
	t.Setenv("KORA_DB_CONNECTION_BUDGET", "16")
	t.Setenv("KORA_DB_SERVER_MAX_CONNECTIONS", "0")
	t.Setenv("KORA_DB_SERVER_RESERVED_CONNECTIONS", "0")
	t.Setenv("KORA_DB_ENGINE_REPLICAS", "1")
	for _, tc := range []struct{ driver, dsn string }{
		{"mysql", "user:password@tcp(127.0.0.1:3306)/tenant"},
		{"postgres", "postgres://user:password@127.0.0.1:5432/tenant?sslmode=disable"},
		{"libsql", "file:/tmp/kora-dbaccess-test.db?mode=ro"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			database, err := Open(tc.driver, tc.dsn)
			if err != nil {
				t.Fatalf("construct %s connector: %v", tc.driver, err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
