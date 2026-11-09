package dbaccess

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
)

var budgets = struct {
	sync.Mutex
	byServer map[string]*budgetEntry
}{byServer: make(map[string]*budgetEntry)}

type budgetEntry struct {
	budget *Budget
	pools  int
}

// Open creates a database/sql pool whose physical driver connections share a
// bounded process budget with every other pool targeting the same server.
func Open(driverName, dsn string) (*sql.DB, error) {
	key := ServerKey(driverName, dsn)
	budget, releaseBudget, err := acquireBudget(key)
	if err != nil {
		return nil, err
	}
	probe, err := sql.Open(driverName, dsn)
	if err != nil {
		releaseBudget()
		return nil, err
	}
	driverImpl := probe.Driver()
	_ = probe.Close()
	var base driver.Connector
	if contextual, ok := driverImpl.(driver.DriverContext); ok {
		base, err = contextual.OpenConnector(dsn)
	} else {
		base = legacyConnector{driver: driverImpl, dsn: dsn}
	}
	if err != nil {
		releaseBudget()
		return nil, fmt.Errorf("create database connector: %w", err)
	}
	database := sql.OpenDB(&budgetConnector{base: base, budget: budget, releaseBudget: releaseBudget})
	// database/sql's default is unlimited. Callers may tighten this for a
	// tenant or administrative pool, but never inherit an unbounded pool.
	database.SetMaxOpenConns(min(budget.capacity, 16))
	database.SetMaxIdleConns(1)
	return database, nil
}

// ServerKey groups pools by physical endpoint. Operators sharing one backend
// through DNS aliases or proxies can set KORA_DB_BUDGET_KEY identically on all
// participating Engine processes to force a common allocation identity.
func ServerKey(driverName, dsn string) string {
	if override := strings.TrimSpace(os.Getenv("KORA_DB_BUDGET_KEY")); override != "" {
		return strings.ToLower(driverName) + ":" + override
	}
	return strings.ToLower(driverName) + ":" + endpoint(driverName, dsn)
}

func endpoint(driverName, dsn string) string {
	switch strings.ToLower(driverName) {
	case "mysql":
		if cfg, err := mysql.ParseDSN(dsn); err == nil {
			return cfg.Net + ":" + cfg.Addr
		}
	case "postgres":
		if parsed, err := url.Parse(dsn); err == nil && parsed.Host != "" {
			return strings.ToLower(parsed.Host)
		}
		var host, port string
		for _, item := range strings.Fields(dsn) {
			key, value, ok := strings.Cut(item, "=")
			if !ok {
				continue
			}
			value = strings.Trim(value, "'\"")
			switch key {
			case "host":
				host = value
			case "port":
				port = value
			}
		}
		if host != "" {
			if port == "" {
				port = "5432"
			}
			return net.JoinHostPort(strings.ToLower(host), port)
		}
	case "libsql":
		if parsed, err := url.Parse(dsn); err == nil && parsed.Host != "" {
			return strings.ToLower(parsed.Scheme + "://" + parsed.Host)
		}
		path := strings.SplitN(strings.TrimPrefix(dsn, "file:"), "?", 2)[0]
		if absolute, err := filepath.Abs(path); err == nil {
			return absolute
		}
	}
	// Avoid placing opaque DSNs (which can contain credentials) in keys/logs.
	return "unparsed"
}

func budgetFor(key string) (*Budget, error) {
	budgets.Lock()
	defer budgets.Unlock()
	entry := budgets.byServer[key]
	if entry == nil {
		var err error
		entry, err = newBudgetEntryLocked(key)
		if err != nil {
			return nil, err
		}
	}
	return entry.budget, nil
}

func acquireBudget(key string) (*Budget, func(), error) {
	budgets.Lock()
	entry := budgets.byServer[key]
	if entry == nil {
		var err error
		entry, err = newBudgetEntryLocked(key)
		if err != nil {
			budgets.Unlock()
			return nil, nil, err
		}
	}
	entry.pools++
	budget := entry.budget
	budgets.Unlock()
	var once sync.Once
	return budget, func() {
		once.Do(func() { releaseBudgetPool(key, entry) })
	}, nil
}

func newBudgetEntryLocked(key string) (*budgetEntry, error) {
	maxEndpoints, err := envInt("KORA_DB_MAX_BUDGET_ENDPOINTS", 8)
	if err != nil || maxEndpoints <= 0 {
		return nil, fmt.Errorf("KORA_DB_MAX_BUDGET_ENDPOINTS must be a positive integer")
	}
	if len(budgets.byServer) >= maxEndpoints {
		return nil, fmt.Errorf("database endpoint budget registry is full")
	}
	cfg, err := ConfigFromEnv()
	if err != nil {
		return nil, err
	}
	budget, err := New(cfg)
	if err != nil {
		return nil, err
	}
	entry := &budgetEntry{budget: budget}
	budgets.byServer[key] = entry
	return entry, nil
}

func releaseBudgetPool(key string, entry *budgetEntry) {
	budgets.Lock()
	defer budgets.Unlock()
	if budgets.byServer[key] != entry {
		return
	}
	if entry.pools > 0 {
		entry.pools--
	}
	if entry.pools == 0 {
		delete(budgets.byServer, key)
	}
}

// AdmissionFor returns the unit-of-work limiter shared by pools that target
// the same endpoint identity.
func AdmissionFor(driverName, dsn string) (*Admission, error) {
	budget, err := budgetFor(ServerKey(driverName, dsn))
	if err != nil {
		return nil, err
	}
	return budget.WorkAdmission(), nil
}

// Snapshots returns a point-in-time view of each process-local endpoint
// budget. The set is bounded by the configured database endpoints, not tenant
// request cardinality.
func Snapshots() map[string]Snapshot {
	budgets.Lock()
	defer budgets.Unlock()
	result := make(map[string]Snapshot, len(budgets.byServer))
	for key, entry := range budgets.byServer {
		result[key] = entry.budget.Snapshot()
	}
	return result
}

type legacyConnector struct {
	driver driver.Driver
	dsn    string
}

func (c legacyConnector) Connect(context.Context) (driver.Conn, error) { return c.driver.Open(c.dsn) }
func (c legacyConnector) Driver() driver.Driver                        { return c.driver }

type budgetConnector struct {
	base          driver.Connector
	budget        *Budget
	releaseBudget func()
	mu            sync.Mutex
	connections   int
	closing       bool
	finalizing    bool
}

func (c *budgetConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return nil, errors.New("database connector is closed")
	}
	c.connections++
	c.mu.Unlock()
	connectionClosed := func() { c.connectionClosed() }

	release, err := c.budget.Acquire(ctx)
	if err != nil {
		connectionClosed()
		return nil, err
	}
	connection, err := c.base.Connect(ctx)
	if err != nil {
		release()
		connectionClosed()
		return nil, err
	}
	return &budgetConn{Conn: connection, release: release, budget: c.budget, onClose: connectionClosed}, nil
}

func (c *budgetConnector) Driver() driver.Driver { return c.base.Driver() }

// Close is called by database/sql when the owning *sql.DB closes. The endpoint
// registry reference remains held until all in-flight connects and physical
// driver connections are also closed, preventing a replacement pool from
// splitting a live server's shared connection budget.
func (c *budgetConnector) Close() error {
	c.mu.Lock()
	c.closing = true
	if c.connections > 0 || c.finalizing {
		c.mu.Unlock()
		return nil
	}
	c.finalizing = true
	c.mu.Unlock()
	return c.finishClose()
}

func (c *budgetConnector) connectionClosed() {
	c.mu.Lock()
	if c.connections > 0 {
		c.connections--
	}
	if !c.closing || c.connections != 0 || c.finalizing {
		c.mu.Unlock()
		return
	}
	c.finalizing = true
	c.mu.Unlock()
	if err := c.finishClose(); err != nil {
		// database/sql may have returned from DB.Close before the caller released
		// its last sql.Conn, so there is no later close call to return this error.
		slog.Error("close retired database connector", "error", err)
	}
}

func (c *budgetConnector) finishClose() error {
	var err error
	if closer, ok := c.base.(interface{ Close() error }); ok {
		err = closer.Close()
	}
	if c.releaseBudget != nil {
		c.releaseBudget()
	}
	return err
}

type budgetConn struct {
	driver.Conn
	release func()
	budget  *Budget
	onClose func()
	close   sync.Once
}

func (c *budgetConn) Close() error {
	var err error
	c.close.Do(func() {
		err = c.Conn.Close()
		c.release()
		if c.onClose != nil {
			c.onClose()
		}
	})
	return err
}

func (c *budgetConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if conn, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return conn.PrepareContext(ctx, query)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.Conn.Prepare(query)
}

func (c *budgetConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if conn, ok := c.Conn.(driver.ConnBeginTx); ok {
		return conn.BeginTx(ctx, opts)
	}
	if opts.Isolation != driver.IsolationLevel(0) || opts.ReadOnly {
		return nil, fmt.Errorf("driver does not support requested transaction options")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.Conn.Begin() //nolint:staticcheck // transparent wrapper: must mirror the legacy optional driver interfaces
}

func (c *budgetConn) Ping(ctx context.Context) error {
	if pinger, ok := c.Conn.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

// Preserve legacy optional execution interfaces as well as their context-aware
// equivalents below. Omitting them makes database/sql fall back to prepared
// statements for drivers that expose only the legacy interfaces.
func (c *budgetConn) Exec(query string, args []driver.Value) (driver.Result, error) {
	if execer, ok := c.Conn.(driver.Execer); ok { //nolint:staticcheck // transparent wrapper: must mirror the legacy optional driver interfaces
		return execer.Exec(query, args)
	}
	return nil, driver.ErrSkip
}

func (c *budgetConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	if queryer, ok := c.Conn.(driver.Queryer); ok { //nolint:staticcheck // transparent wrapper: must mirror the legacy optional driver interfaces
		return queryer.Query(query, args)
	}
	return nil, driver.ErrSkip
}

func (c *budgetConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.Conn.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *budgetConn) IsValid() bool {
	if validator, ok := c.Conn.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

func (c *budgetConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

func (c *budgetConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if execer, ok := c.Conn.(driver.ExecerContext); ok {
		started := time.Now()
		result, err := execer.ExecContext(ctx, query, args)
		c.budget.recordOperation(started, err)
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if execer, ok := c.Conn.(driver.Execer); ok { //nolint:staticcheck // transparent wrapper: must mirror the legacy optional driver interfaces
		values, err := legacyValues(args)
		if err != nil {
			return nil, err
		}
		started := time.Now()
		result, err := execer.Exec(query, values)
		c.budget.recordOperation(started, err)
		return result, err
	}
	return nil, driver.ErrSkip
}

func (c *budgetConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if queryer, ok := c.Conn.(driver.QueryerContext); ok {
		started := time.Now()
		rows, err := queryer.QueryContext(ctx, query, args)
		c.budget.recordOperation(started, err)
		return rows, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if queryer, ok := c.Conn.(driver.Queryer); ok { //nolint:staticcheck // transparent wrapper: must mirror the legacy optional driver interfaces
		values, err := legacyValues(args)
		if err != nil {
			return nil, err
		}
		started := time.Now()
		rows, err := queryer.Query(query, values)
		c.budget.recordOperation(started, err)
		return rows, err
	}
	return nil, driver.ErrSkip
}

func isLockContention(err error) bool {
	if err == nil {
		return false
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		switch mysqlErr.Number {
		case 1205, 1213, 3572:
			return true
		}
	}
	var stateErr interface{ SQLState() string }
	if errors.As(err, &stateErr) {
		switch stateErr.SQLState() {
		case "40P01", "55P03":
			return true
		}
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "database is busy") || strings.Contains(message, "sqlite_busy") || strings.Contains(message, "sqlite_locked")
}

func legacyValues(args []driver.NamedValue) ([]driver.Value, error) {
	values := make([]driver.Value, len(args))
	for index, arg := range args {
		if arg.Name != "" {
			return nil, driver.ErrSkip
		}
		// arg.Value is already a driver.Value. A NULL parameter is a nil value and is valid; the
		// previous type assertion rejected it as "not normalized".
		values[index] = arg.Value
	}
	return values, nil
}
