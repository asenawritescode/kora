package auth

import (
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/email"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// dbTimeout is the maximum time a database query can take before being cancelled.
// Prevents indefinite hangs when the database server is slow or overloaded.
const dbTimeout = 5 * time.Second
const dbCleanupTimeout = 10 * time.Second // cleanup queries get longer timeout
const magicLinkLifetime = 15 * time.Minute
const magicLinkCleanupGrace = 24 * time.Hour
const authRateLimitWindow = 10 * time.Minute
const authRateLimitMax = 8

var magicLinkRateLimit = struct {
	sync.Mutex
	requests map[string][]time.Time
}{requests: make(map[string][]time.Time)}

// User represents an authenticated user.
type User struct {
	Name     string   `json:"name"`
	Email    string   `json:"email"`
	FullName string   `json:"full_name"`
	Roles    []string `json:"roles"`
	Enabled  bool     `json:"enabled"`
}

// SessionLifetime is the duration for which sessions are valid. Set before creating SessionManagers.
var SessionLifetime = 24 * time.Hour

// sessionCacheTTL is how long session lookups are cached before re-validating.
const sessionCacheTTL = 30 * time.Second
const sessionCacheCleanupInterval = 5 * time.Minute
const sessionCacheSweepInterval = 30 * time.Second
const DefaultSessionCacheMaxBytes uint64 = 8 << 20
const defaultSessionCacheMaxEntries = 8192

type sessionCacheEntry struct {
	user      *User
	site      string // site the session belongs to (prevents cross-site cache hits)
	cachedAt  time.Time
	createdAt time.Time
	expiresAt time.Time // session expiry from DB
	sizeBytes uint64
	lru       *list.Element
}

type SessionCacheSnapshot struct {
	Entries    int
	Bytes      uint64
	MaxEntries int
	MaxBytes   uint64
}

// SessionManager manages user sessions with an in-memory TTL cache.
type SessionManager struct {
	DB               *sql.DB
	dialect          kdb.Dialect
	cacheMu          sync.RWMutex
	cache            map[string]*sessionCacheEntry
	cacheLRU         list.List
	cacheBytes       uint64
	cacheMaxBytes    uint64
	cacheMaxEntries  int
	cacheBudget      *SessionCacheBudget
	cleanupAdmission interface {
		Acquire(context.Context) (func(), error)
	}
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	startMu   sync.Mutex
	started   bool
	closed    bool
	doneOnce  sync.Once
	closeOnce sync.Once
}

// NewSessionManager constructs an independently owned session manager. Callers
// that retain it must call Close when its database/runtime is retired. Routed
// Engine requests should use the manager pinned in request context instead.
func NewSessionManager(db *sql.DB) *SessionManager {
	return newSessionManager(db)
}

// NewOwnedSessionManager constructs a tenant-owned manager. Its sweeper is
// stopped and joined by Close when the runtime is retired.
func NewOwnedSessionManager(db *sql.DB) *SessionManager { return newSessionManager(db) }

// NewOwnedSessionManagerPaused creates a tenant-owned manager without starting
// its sweeper. Engine.Run or a live tenant activation calls StartCleanup.
func NewOwnedSessionManagerPaused(db *sql.DB) *SessionManager { return newPausedSessionManager(db) }

func sessionManagerForRequest(c *gin.Context, db *sql.DB) *SessionManager {
	if c != nil {
		if value, exists := c.Get("site_session_manager"); exists {
			if manager, ok := value.(*SessionManager); ok && manager != nil {
				return manager
			}
		}
		if value, exists := c.Get("request_session_manager"); exists {
			if manager, ok := value.(*SessionManager); ok && manager != nil {
				return manager
			}
		}
		manager := newRequestSessionManager(db)
		if dbType := strings.TrimSpace(c.GetString("site_db_type")); dbType != "" {
			manager.SetDialect(kdb.Resolve(dbType))
		}
		c.Set("request_session_manager", manager)
		return manager
	}
	return newRequestSessionManager(db)
}

func newSessionManager(db *sql.DB) *SessionManager {
	ctx, cancel := context.WithCancel(context.Background())
	sm := newSessionManagerWithCacheLimits(db, DefaultSessionCacheMaxBytes, defaultSessionCacheMaxEntries)
	sm.ctx, sm.cancel, sm.done = ctx, cancel, make(chan struct{})
	if db == nil {
		sm.doneOnce.Do(func() { close(sm.done) })
	} else {
		sm.StartCleanup()
	}
	return sm
}

func newPausedSessionManager(db *sql.DB) *SessionManager {
	sm := newSessionManagerWithCacheLimits(db, DefaultSessionCacheMaxBytes, defaultSessionCacheMaxEntries)
	if db == nil {
		sm.done = make(chan struct{})
		sm.doneOnce.Do(func() { close(sm.done) })
		return sm
	}
	sm.ctx, sm.cancel = context.WithCancel(context.Background())
	sm.done = make(chan struct{})
	return sm
}

func newSessionManagerWithCacheLimits(db *sql.DB, maxBytes uint64, maxEntries int) *SessionManager {
	if maxBytes == 0 {
		maxBytes = DefaultSessionCacheMaxBytes
	}
	if maxEntries <= 0 {
		maxEntries = defaultSessionCacheMaxEntries
	}
	return &SessionManager{DB: db, cache: make(map[string]*sessionCacheEntry), cacheMaxBytes: maxBytes, cacheMaxEntries: maxEntries}
}

// SetDialect binds the SQL dialect of DB. Call it before the manager serves
// requests; without it queries keep MySQL-style '?' placeholders.
func (sm *SessionManager) SetDialect(dialect kdb.Dialect) *SessionManager {
	if sm != nil {
		sm.dialect = dialect
	}
	return sm
}

func (sm *SessionManager) sql(query string) string {
	if sm == nil || sm.dialect == nil {
		return query
	}
	return kdb.Rebind(sm.dialect, query)
}

// userColumn quotes _kora_session.user, a reserved word in PostgreSQL.
func (sm *SessionManager) userColumn() string {
	if sm == nil || sm.dialect == nil {
		return "user"
	}
	return sm.dialect.QuoteIdent("user")
}

func (sm *SessionManager) CacheSnapshot() SessionCacheSnapshot {
	if sm == nil {
		return SessionCacheSnapshot{}
	}
	sm.cacheMu.RLock()
	defer sm.cacheMu.RUnlock()
	return SessionCacheSnapshot{Entries: len(sm.cache), Bytes: sm.cacheBytes, MaxEntries: sm.cacheMaxEntries, MaxBytes: sm.cacheMaxBytes}
}

// SetCacheBudget attaches a process-shared retained-byte budget. It clears
// entries populated before attachment so accounting begins from a known state.
func (sm *SessionManager) SetCacheBudget(budget *SessionCacheBudget) {
	if sm == nil {
		return
	}
	sm.cacheMu.Lock()
	defer sm.cacheMu.Unlock()
	if sm.cacheBudget == budget {
		return
	}
	if sm.cacheBudget != nil {
		sm.cacheBudget.release(sm.cacheBytes)
	}
	clear(sm.cache)
	sm.cacheLRU.Init()
	sm.cacheBytes = 0
	sm.cacheBudget = budget
}

// SetCleanupAdmission attaches the Engine endpoint work budget to the periodic
// database cleanup pass. Compatibility callers without an Engine owner retain
// the historical unshared behavior.
func (sm *SessionManager) SetCleanupAdmission(admission interface {
	Acquire(context.Context) (func(), error)
}) {
	if sm == nil {
		return
	}
	sm.startMu.Lock()
	sm.cleanupAdmission = admission
	sm.startMu.Unlock()
}

// StartCleanup starts the owned session-cache sweeper exactly once.
func (sm *SessionManager) StartCleanup() {
	if sm == nil {
		return
	}
	sm.startMu.Lock()
	defer sm.startMu.Unlock()
	if sm.closed || sm.started || sm.DB == nil {
		return
	}
	sm.started = true
	go sm.sweepCacheLoop()
}

// newRequestSessionManager has no background sweeper: the Gin request owns its
// short-lived cache and drops it with the request. Long-lived callers use
// NewSessionManager and explicitly join its sweeper with Close.
func newRequestSessionManager(db *sql.DB) *SessionManager {
	done := make(chan struct{})
	close(done)
	manager := newSessionManagerWithCacheLimits(db, DefaultSessionCacheMaxBytes, defaultSessionCacheMaxEntries)
	manager.done = done
	manager.doneOnce.Do(func() {})
	return manager
}

// Close stops and joins the session cache cleanup worker.
func (sm *SessionManager) Close() error {
	if sm == nil {
		return nil
	}
	sm.closeOnce.Do(func() {
		sm.startMu.Lock()
		sm.closed = true
		started := sm.started
		cancel, done := sm.cancel, sm.done
		if !started && done != nil {
			sm.doneOnce.Do(func() { close(done) })
		}
		sm.startMu.Unlock()
		if cancel != nil {
			cancel()
		}
		if done != nil {
			<-done
		}
		sm.cacheMu.Lock()
		if sm.cacheBudget != nil {
			sm.cacheBudget.release(sm.cacheBytes)
		}
		clear(sm.cache)
		sm.cacheLRU.Init()
		sm.cacheBytes = 0
		sm.cacheMu.Unlock()
	})
	return nil
}

// CreateSession creates a new session for a user and returns the session ID.
func (sm *SessionManager) CreateSession(site string, user *User) (string, error) {
	if sm.DB == nil {
		return "", fmt.Errorf("no database connection available: %w", ErrNoDBConnection)
	}
	sid := generateSessionID()
	expiresAt := time.Now().Add(SessionLifetime)

	// Marshal user data as JSON in Go (dialect-neutral, avoids MySQL-only JSON_OBJECT).
	userData := gin.H{
		"name":      user.Name,
		"email":     user.Email,
		"full_name": user.FullName,
		"roles":     user.Roles,
	}
	userJSON, err := json.Marshal(userData)
	if err != nil {
		return "", fmt.Errorf("marshaling user data: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	_, err = sm.DB.ExecContext(ctx,
		sm.sql(`INSERT INTO _kora_session (sid, site, `+sm.userColumn()+`, data, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`),
		sid, site, user.Name, string(userJSON), expiresAt, time.Now(),
	)
	if err != nil {
		return "", fmt.Errorf("creating session: %w", err)
	}

	return sid, nil
}

// GetSession validates a session ID for a given site and returns the associated user
// plus the session creation timestamp.
// Uses an in-memory TTL cache to avoid hitting the database on every request.
func (sm *SessionManager) GetSession(site, sid string) (*User, time.Time, error) {
	if sm.DB == nil {
		return nil, time.Time{}, fmt.Errorf("no database connection available: %w", ErrNoDBConnection)
	}
	// Check cache first.
	sm.cacheMu.Lock()
	entry, ok := sm.cache[sid]
	if ok && entry.lru != nil {
		sm.cacheLRU.MoveToFront(entry.lru)
	}
	sm.cacheMu.Unlock()

	if ok && time.Now().Before(entry.cachedAt.Add(sessionCacheTTL)) && entry.site == site {
		if time.Now().After(entry.expiresAt) {
			sm.DeleteSession(sid)
			return nil, time.Time{}, fmt.Errorf("session expired: %w", ErrSessionExpired)
		}
		return cloneSessionUser(entry.user), entry.createdAt, nil
	}

	// Cache miss or expired — query database.
	var userJSON string
	var expiresStr string // scanned as string for SQLite compatibility (TEXT column)
	var createdStr string

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	err := sm.DB.QueryRowContext(ctx,
		sm.sql("SELECT data, expires_at, created_at FROM _kora_session WHERE site = ? AND sid = ?"),
		site, sid,
	).Scan(&userJSON, &expiresStr, &createdStr)

	if err == sql.ErrNoRows {
		// Remove from cache if present.
		sm.cacheMu.Lock()
		sm.deleteCacheEntryLocked(sid)
		sm.cacheMu.Unlock()
		return nil, time.Time{}, fmt.Errorf("session not found")
	}
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("querying session: %w", err)
	}

	expiresAt, err := parseTime(expiresStr)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("parsing session expiry: %w", err)
	}
	createdAt, err := parseTime(createdStr)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("parsing session creation time: %w", err)
	}

	if time.Now().After(expiresAt) {
		sm.DeleteSession(sid)
		return nil, time.Time{}, fmt.Errorf("session expired: %w", ErrSessionExpired)
	}

	// Parse JSON. For simplicity in Phase 1, parse manually.
	user := &User{}
	if err := scanUserJSON(userJSON, user); err != nil {
		return nil, time.Time{}, fmt.Errorf("parsing session data: %w", err)
	}

	// Populate the byte-bounded cache. Large entries still authenticate this
	// request, but are not retained in process memory.
	sm.cacheSession(sid, site, user, createdAt, expiresAt, time.Now())

	return user, createdAt, nil
}

func estimateSessionCacheEntryBytes(sid, site string, user *User) uint64 {
	// Account for map/list links, entry fields, string headers, and role slice
	// storage with fixed conservative overhead plus retained string bytes.
	size := uint64(256 + len(sid) + len(site))
	if user == nil {
		return size
	}
	size += uint64(len(user.Name) + len(user.Email) + len(user.FullName))
	for _, role := range user.Roles {
		size += uint64(16 + len(role))
	}
	return size
}

func (sm *SessionManager) cacheSession(sid, site string, user *User, createdAt, expiresAt, cachedAt time.Time) {
	retainedUser := cloneSessionUser(user)
	entry := &sessionCacheEntry{user: retainedUser, site: site, cachedAt: cachedAt, createdAt: createdAt, expiresAt: expiresAt}
	entry.sizeBytes = estimateSessionCacheEntryBytes(sid, site, retainedUser)
	sm.cacheMu.Lock()
	defer sm.cacheMu.Unlock()
	sm.deleteCacheEntryLocked(sid)
	if entry.sizeBytes > sm.cacheMaxBytes {
		return
	}
	for sm.cacheBytes+entry.sizeBytes > sm.cacheMaxBytes || len(sm.cache) >= sm.cacheMaxEntries {
		oldest := sm.cacheLRU.Back()
		if oldest == nil {
			return
		}
		sm.deleteCacheEntryLocked(oldest.Value.(string))
	}
	if sm.cacheBudget != nil && !sm.cacheBudget.acquire(entry.sizeBytes) {
		return
	}
	entry.lru = sm.cacheLRU.PushFront(sid)
	sm.cache[sid] = entry
	sm.cacheBytes += entry.sizeBytes
}

func cloneSessionUser(user *User) *User {
	if user == nil {
		return nil
	}
	copy := *user
	copy.Roles = append([]string(nil), user.Roles...)
	return &copy
}

func (sm *SessionManager) deleteCacheEntryLocked(sid string) {
	entry := sm.cache[sid]
	if entry == nil {
		return
	}
	if entry.lru != nil {
		sm.cacheLRU.Remove(entry.lru)
	}
	if entry.sizeBytes >= sm.cacheBytes {
		sm.cacheBytes = 0
	} else {
		sm.cacheBytes -= entry.sizeBytes
	}
	if sm.cacheBudget != nil {
		sm.cacheBudget.release(entry.sizeBytes)
	}
	delete(sm.cache, sid)
}

func scanUserJSON(jsonStr string, user *User) error {
	// Simple JSON parsing — extract name, email, full_name, roles.
	extract := func(key string) string {
		start := 0
		for {
			idx := indexAfter(jsonStr, `"`+key+`"`, start)
			if idx < 0 {
				return ""
			}
			// Skip whitespace and colon.
			rest := jsonStr[idx:]
			colonIdx := 0
			for colonIdx < len(rest) && (rest[colonIdx] == ' ' || rest[colonIdx] == ':') {
				colonIdx++
			}
			if colonIdx >= len(rest) {
				return ""
			}
			rest = rest[colonIdx:]
			if len(rest) > 0 && rest[0] == '[' {
				// Array value.
				endIdx := 1
				for endIdx < len(rest) && rest[endIdx] != ']' {
					endIdx++
				}
				return rest[:endIdx+1]
			}
			if len(rest) > 0 && rest[0] == '"' {
				// String value.
				endIdx := 1
				for endIdx < len(rest) && rest[endIdx] != '"' {
					endIdx++
				}
				return rest[1:endIdx]
			}
			start = idx + len(key) + 2
		}
	}

	user.Name = strings.Clone(extract("name"))
	user.Email = strings.Clone(extract("email"))
	user.FullName = strings.Clone(extract("full_name"))
	rolesStr := extract("roles")
	if rolesStr != "" {
		// Parse array: ["Role1", "Role2"]
		rolesStr = trim(rolesStr, "[]")
		if rolesStr != "" {
			parts := splitQuoted(rolesStr)
			for _, p := range parts {
				user.Roles = append(user.Roles, strings.Clone(p))
			}
		}
	}

	return nil
}

func indexAfter(s, substr string, start int) int {
	for i := start; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i + len(substr)
		}
	}
	return -1
}

func trim(s, cutset string) string {
	for len(s) > 0 && contains(cutset, s[0]) {
		s = s[1:]
	}
	for len(s) > 0 && contains(cutset, s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return s
}

func contains(set string, c byte) bool {
	for i := 0; i < len(set); i++ {
		if set[i] == c {
			return true
		}
	}
	return false
}

func splitQuoted(s string) []string {
	var result []string
	var current string
	inQuote := false
	for _, c := range s {
		if c == '"' {
			inQuote = !inQuote
			if !inQuote && current != "" {
				result = append(result, current)
				current = ""
			}
		} else if inQuote {
			current += string(c)
		}
	}
	return result
}

// DeleteSession removes a session.
func (sm *SessionManager) DeleteSession(sid string) {
	if sm.DB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	_, err := sm.DB.ExecContext(ctx, sm.sql("DELETE FROM _kora_session WHERE sid = ?"), sid)
	if err != nil {
		slog.Warn("failed to delete session", "sid", sid, "error", err)
	}
	// Invalidate cache entry.
	sm.cacheMu.Lock()
	sm.deleteCacheEntryLocked(sid)
	sm.cacheMu.Unlock()
}

// InvalidateSession removes a session from the cache without deleting it from the database.
// Use this when user state changes (role change, password change, disable) to force re-validation.
func (sm *SessionManager) InvalidateSession(sid string) {
	sm.cacheMu.Lock()
	sm.deleteCacheEntryLocked(sid)
	sm.cacheMu.Unlock()
}

// sweepCacheLoop periodically removes expired cache entries and cleans up expired DB sessions.
func (sm *SessionManager) sweepCacheLoop() {
	cacheTicker := time.NewTicker(sessionCacheSweepInterval)
	defer cacheTicker.Stop()
	databaseTicker := time.NewTicker(sessionCacheCleanupInterval)
	defer databaseTicker.Stop()
	defer sm.doneOnce.Do(func() { close(sm.done) })
	for {
		select {
		case <-sm.ctx.Done():
			return
		case <-cacheTicker.C:
			sm.sweepExpiredCache(time.Now())
			continue
		case <-databaseTicker.C:
		}
		sm.cleanupDatabaseRows()
		cleanupAuthRateLimits()
	}
}

func (sm *SessionManager) cleanupDatabaseRows() {
	if sm == nil || sm.DB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(sm.ctx, dbCleanupTimeout)
	defer cancel()
	sm.startMu.Lock()
	admission := sm.cleanupAdmission
	sm.startMu.Unlock()
	if admission != nil {
		release, err := admission.Acquire(ctx)
		if err != nil {
			if sm.ctx.Err() == nil {
				slog.Warn("session database cleanup deferred by work admission", "error", err)
			}
			return
		}
		defer release()
	}
	sm.cleanupExpiredContext(ctx)
	sm.cleanupMagicLinksContext(ctx)
}

func (sm *SessionManager) sweepExpiredCache(now time.Time) {
	sm.cacheMu.Lock()
	defer sm.cacheMu.Unlock()
	for sid, entry := range sm.cache {
		if now.After(entry.expiresAt) || now.After(entry.cachedAt.Add(sessionCacheTTL*2)) {
			sm.deleteCacheEntryLocked(sid)
		}
	}
}

func (sm *SessionManager) cleanupExpired() {
	if sm.DB == nil {
		return // console-only mode — no site database
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbCleanupTimeout)
	defer cancel()
	sm.cleanupExpiredContext(ctx)
}

func (sm *SessionManager) cleanupExpiredContext(ctx context.Context) {
	_, err := sm.DB.ExecContext(ctx, sm.sql("DELETE FROM _kora_session WHERE expires_at < ?"), time.Now())
	if err != nil {
		slog.Warn("failed to cleanup expired sessions", "error", err)
	}
}

func (sm *SessionManager) cleanupMagicLinks() {
	if sm.DB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbCleanupTimeout)
	defer cancel()
	sm.cleanupMagicLinksContext(ctx)
}

func (sm *SessionManager) cleanupMagicLinksContext(ctx context.Context) {
	cutoff := time.Now().Add(-magicLinkCleanupGrace)
	_, err := sm.DB.ExecContext(ctx,
		sm.sql(`DELETE FROM _kora_magic_link
		 WHERE expires_at < ? OR revoked_at < ? OR (used_at IS NOT NULL AND used_at < ?)`),
		time.Now(), cutoff, cutoff,
	)
	if err != nil {
		slog.Warn("failed to cleanup magic links", "error", err)
	}
}

func (sm *SessionManager) FindUserByEmail(site, emailAddr string) (*User, error) {
	return sm.FindUserByEmailContext(context.Background(), site, emailAddr)
}

// FindUserByCloudSubject resolves the immutable Cloud subject to the
// passwordless tenant principal created during provisioning.
func (sm *SessionManager) FindUserByCloudSubject(site, subject string) (*User, error) {
	if sm == nil || sm.DB == nil {
		return nil, ErrNoDBConnection
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	var u User
	var roles string
	err := sm.DB.QueryRowContext(ctx, sm.sql(`SELECT name,email,full_name,enabled,COALESCE(roles,'') FROM _kora_user WHERE site=? AND name=?`), site, subject).Scan(&u.Name, &u.Email, &u.FullName, &u.Enabled, &roles)
	if err != nil {
		return nil, err
	}
	if !u.Enabled {
		return nil, ErrDisabledAccount
	}
	if roles != "" {
		u.Roles = splitRolesStr(roles)
	}
	return &u, nil
}

// FindUserByEmailContext looks up an enabled user while honoring the owning
// request or worker cancellation. The compatibility method above remains for
// older callers that do not carry a context.
func (sm *SessionManager) FindUserByEmailContext(parent context.Context, site, emailAddr string) (*User, error) {
	if parent == nil {
		return nil, fmt.Errorf("user lookup requires a context")
	}
	if sm.DB == nil {
		return nil, fmt.Errorf("no database connection available: %w", ErrNoDBConnection)
	}
	var name, storedEmail, fullName, rolesStr string
	var enabled bool

	ctx, cancel := context.WithTimeout(parent, dbTimeout)
	defer cancel()
	err := sm.DB.QueryRowContext(ctx,
		sm.sql("SELECT name, email, full_name, enabled, COALESCE(roles, '') FROM _kora_user WHERE site = ? AND email = ?"),
		site, emailAddr,
	).Scan(&name, &storedEmail, &fullName, &enabled, &rolesStr)
	if err == sql.ErrNoRows {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("querying user: %w", err)
	}
	if !enabled {
		slog.Warn("magic-link request for disabled account", "email", emailAddr)
		return nil, ErrDisabledAccount
	}

	var roles []string
	if rolesStr != "" {
		roles = splitRolesStr(rolesStr)
	}

	return &User{
		Name:     name,
		Email:    storedEmail,
		FullName: fullName,
		Roles:    roles,
		Enabled:  enabled,
	}, nil
}

func (sm *SessionManager) IsEmailVerified(site, emailAddr string) (bool, error) {
	if sm.DB == nil {
		return false, fmt.Errorf("no database connection available: %w", ErrNoDBConnection)
	}

	var verified sql.NullTime
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	err := sm.DB.QueryRowContext(ctx,
		sm.sql("SELECT email_verified_at FROM _kora_user WHERE site = ? AND email = ?"),
		site, emailAddr,
	).Scan(&verified)
	if err == sql.ErrNoRows {
		return false, ErrInvalidCredentials
	}
	if err != nil {
		return false, fmt.Errorf("querying user verification state: %w", err)
	}
	return verified.Valid, nil
}

func (sm *SessionManager) MarkEmailVerified(site, emailAddr string) error {
	if sm.DB == nil {
		return fmt.Errorf("no database connection available: %w", ErrNoDBConnection)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	_, err := sm.DB.ExecContext(ctx,
		sm.sql(`UPDATE _kora_user
		 SET email_verified_at = COALESCE(email_verified_at, ?), modified = CURRENT_TIMESTAMP
		 WHERE site = ? AND email = ?`),
		time.Now(), site, emailAddr,
	)
	if err != nil {
		return fmt.Errorf("marking email verified: %w", err)
	}
	return nil
}

type MagicLinkRecord struct {
	ID        string     `json:"id"`
	Email     string     `json:"email"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

func (sm *SessionManager) ListMagicLinks(site, emailAddr string) ([]MagicLinkRecord, error) {
	if sm.DB == nil {
		return nil, fmt.Errorf("no database connection available: %w", ErrNoDBConnection)
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	rows, err := sm.DB.QueryContext(ctx,
		sm.sql(`SELECT id, email, created_at, expires_at, used_at, revoked_at
		 FROM _kora_magic_link
		 WHERE site = ? AND email = ? AND (revoked_at IS NULL OR revoked_at >= ?) AND expires_at >= ?
		 ORDER BY created_at DESC`),
		site, emailAddr, time.Now().Add(-magicLinkCleanupGrace), time.Now().Add(-magicLinkCleanupGrace),
	)
	if err != nil {
		return nil, fmt.Errorf("querying magic links: %w", err)
	}
	defer rows.Close()

	var links []MagicLinkRecord
	for rows.Next() {
		var rec MagicLinkRecord
		var usedAt, revokedAt sql.NullTime
		if err := rows.Scan(&rec.ID, &rec.Email, &rec.CreatedAt, &rec.ExpiresAt, &usedAt, &revokedAt); err != nil {
			return nil, fmt.Errorf("scanning magic link: %w", err)
		}
		if usedAt.Valid {
			v := usedAt.Time
			rec.UsedAt = &v
		}
		if revokedAt.Valid {
			v := revokedAt.Time
			rec.RevokedAt = &v
		}
		links = append(links, rec)
	}
	return links, nil
}

func (sm *SessionManager) RevokeMagicLink(site, emailAddr, id string) error {
	if sm.DB == nil {
		return fmt.Errorf("no database connection available: %w", ErrNoDBConnection)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	_, err := sm.DB.ExecContext(ctx,
		sm.sql(`UPDATE _kora_magic_link
		 SET revoked_at = COALESCE(revoked_at, ?)
		 WHERE site = ? AND email = ? AND id = ?`),
		time.Now(), site, emailAddr, id,
	)
	if err != nil {
		return fmt.Errorf("revoking magic link: %w", err)
	}
	return nil
}

func (sm *SessionManager) RevokeAllMagicLinks(site, emailAddr string) error {
	if sm.DB == nil {
		return fmt.Errorf("no database connection available: %w", ErrNoDBConnection)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	_, err := sm.DB.ExecContext(ctx,
		sm.sql(`UPDATE _kora_magic_link
		 SET revoked_at = COALESCE(revoked_at, ?)
		 WHERE site = ? AND email = ? AND revoked_at IS NULL AND used_at IS NULL`),
		time.Now(), site, emailAddr,
	)
	if err != nil {
		return fmt.Errorf("revoking magic links: %w", err)
	}
	return nil
}

// consumeMagicLink atomically consumes a one-time email link and creates its
// session. The raw token is never persisted; only its SHA-256 digest is stored.
func (sm *SessionManager) consumeMagicLink(parent context.Context, site, token string) (*User, string, time.Time, error) {
	if sm == nil || sm.DB == nil {
		return nil, "", time.Time{}, ErrNoDBConnection
	}
	if parent == nil {
		return nil, "", time.Time{}, errors.New("magic-link verification requires a context")
	}
	if len(token) != 64 {
		return nil, "", time.Time{}, ErrInvalidCredentials
	}
	if _, err := hex.DecodeString(token); err != nil {
		return nil, "", time.Time{}, ErrInvalidCredentials
	}
	ctx, cancel := context.WithTimeout(parent, dbTimeout)
	defer cancel()
	tx, err := sm.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("beginning magic-link verification: %w", err)
	}
	defer tx.Rollback()
	var id, emailAddr string
	var expiresAt time.Time
	var usedAt, revokedAt sql.NullTime
	err = tx.QueryRowContext(ctx,
		sm.sql(`SELECT id, email, expires_at, used_at, revoked_at FROM _kora_magic_link WHERE site = ? AND token_hash = ?`),
		site, hashMagicLinkToken(token),
	).Scan(&id, &emailAddr, &expiresAt, &usedAt, &revokedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", time.Time{}, ErrInvalidCredentials
		}
		return nil, "", time.Time{}, fmt.Errorf("looking up magic link: %w", err)
	}
	if revokedAt.Valid {
		return nil, "", time.Time{}, ErrMagicLinkRevoked
	}
	if usedAt.Valid {
		return nil, "", time.Time{}, ErrMagicLinkUsed
	}
	now := time.Now()
	if !expiresAt.After(now) {
		return nil, "", time.Time{}, ErrMagicLinkExpired
	}
	var user User
	var roles string
	if err := tx.QueryRowContext(ctx,
		sm.sql(`SELECT name, email, full_name, enabled, COALESCE(roles, '') FROM _kora_user WHERE site = ? AND email = ?`),
		site, emailAddr,
	).Scan(&user.Name, &user.Email, &user.FullName, &user.Enabled, &roles); err != nil || !user.Enabled {
		return nil, "", time.Time{}, ErrInvalidCredentials
	}
	user.Roles = splitRolesStr(roles)
	result, err := tx.ExecContext(ctx,
		sm.sql(`UPDATE _kora_magic_link SET used_at = ? WHERE id = ? AND site = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?`),
		now, id, site, now,
	)
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("consuming magic link: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return nil, "", time.Time{}, ErrMagicLinkUsed
	}
	if _, err := tx.ExecContext(ctx,
		sm.sql(`UPDATE _kora_user SET email_verified_at = COALESCE(email_verified_at, ?), modified = CURRENT_TIMESTAMP WHERE site = ? AND email = ?`),
		now, site, emailAddr,
	); err != nil {
		return nil, "", time.Time{}, fmt.Errorf("verifying sign-in email: %w", err)
	}
	sid := generateSessionID()
	userData, err := json.Marshal(gin.H{"name": user.Name, "email": user.Email, "full_name": user.FullName, "roles": user.Roles})
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("encoding session user: %w", err)
	}
	sessionExpires := now.Add(SessionLifetime)
	if _, err := tx.ExecContext(ctx,
		sm.sql(`INSERT INTO _kora_session (sid, site, `+sm.userColumn()+`, data, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`),
		sid, site, user.Name, string(userData), sessionExpires, now,
	); err != nil {
		return nil, "", time.Time{}, fmt.Errorf("creating magic-link session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, "", time.Time{}, fmt.Errorf("committing magic-link session: %w", err)
	}
	return &user, sid, sessionExpires, nil
}

func splitRolesStr(s string) []string {
	// Try newline first, then comma.
	if containsStr(s, "\n") {
		result := splitStr(s, "\n")
		for i, r := range result {
			result[i] = trimWhitespace(r)
		}
		return result
	}
	result := splitStr(s, ",")
	for i, r := range result {
		result[i] = trimWhitespace(r)
	}
	return result
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && indexAfterStr(s, substr) >= 0
}

func indexAfterStr(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func splitStr(s, sep string) []string {
	var result []string
	for {
		idx := indexAfterStr(s, sep)
		if idx < 0 {
			result = append(result, s)
			break
		}
		result = append(result, s[:idx])
		s = s[idx+len(sep):]
	}
	return result
}

func trimWhitespace(s string) string {
	s = trim(s, " \t\r\n")
	return s
}

// HashPassword creates a bcrypt hash of a password.
func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

func generateSessionID() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func generateMagicLinkToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var generateMagicLinkTokenFn = generateMagicLinkToken

func hashMagicLinkToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewMagicLinkToken creates a cryptographically random token for a durable
// management invitation operation. Callers persist only its hash.
func NewMagicLinkToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate magic-link token: %w", err)
	}
	return hex.EncodeToString(value), nil
}

// MagicLinkTokenHash returns the persisted token digest, never the token.
func MagicLinkTokenHash(token string) string { return hashMagicLinkToken(token) }

// TenantMagicLinkURL creates a link on a tenant-owned DNS alias. Non-loopback
// tenant hosts always use HTTPS so an enrollment token is never sent in clear.
func TenantMagicLinkURL(host, token string) (string, error) {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if err := contract.ValidateTenantAlias(host); err != nil {
		return "", fmt.Errorf("magic-link host is not a canonical tenant alias: %w", err)
	}
	if token == "" || len(token) > 512 {
		return "", fmt.Errorf("magic-link token is invalid")
	}
	scheme := "https"
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(host, ".localhost") {
		scheme = "http"
	} else if address := net.ParseIP(host); address != nil && address.IsLoopback() {
		scheme = "http"
	}
	return (&url.URL{Scheme: scheme, Host: host, Path: "/workspace/auth/login", RawQuery: url.Values{"magic_token": []string{token}}.Encode()}).String(), nil
}

// TenantPathMagicLinkURL creates a sign-in link through the public application
// origin and the tenant path used by the standalone workspace. The configured
// origin is trusted operator configuration; it is never taken from a request
// or a tenant-provided value.
func TenantPathMagicLinkURL(appBaseURL, tenant, token string) (string, error) {
	base, err := publicAppBaseURL(appBaseURL)
	if err != nil {
		return "", err
	}
	tenant = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(tenant), "."))
	if err := contract.ValidateTenantAlias(tenant); err != nil {
		return "", fmt.Errorf("magic-link tenant is not a canonical alias: %w", err)
	}
	if token == "" || len(token) > 512 {
		return "", errors.New("magic-link token is invalid")
	}
	base.Path = "/s/" + tenant + "/workspace/auth/login"
	base.RawQuery = url.Values{"magic_token": []string{token}}.Encode()
	return base.String(), nil
}

// ValidatePublicAppBaseURL rejects ambiguous or unsafe origins before an
// invitation worker is started. An empty value keeps host-based deployments on
// TenantMagicLinkURL; path-based deployments must configure this origin.
func ValidatePublicAppBaseURL(value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	_, err := publicAppBaseURL(value)
	return err
}

func publicAppBaseURL(value string) (*url.URL, error) {
	value = strings.TrimSpace(value)
	base, err := url.Parse(value)
	if err != nil || base == nil || !base.IsAbs() || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return nil, errors.New("public application base URL must be an origin without credentials, path, query, or fragment")
	}
	if base.Scheme != "https" && base.Scheme != "http" {
		return nil, errors.New("public application base URL must use HTTP or HTTPS")
	}
	if base.Scheme == "http" {
		host := strings.TrimSuffix(strings.ToLower(base.Hostname()), ".")
		address := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && !strings.HasSuffix(host, ".localhost") && (address == nil || !address.IsLoopback()) {
			return nil, errors.New("public application base URL must use HTTPS outside loopback")
		}
	}
	base.Path = ""
	base.RawPath = ""
	return base, nil
}

// AuthMiddleware returns a Gin middleware that validates session cookies.
func AuthMiddleware(sm *SessionManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip auth for login endpoint and health check.
		path := c.Request.URL.Path
		if path == "/api/ping" ||
			path == "/workspace/auth/login" ||
			path == "/console/login" {
			c.Next()
			return
		}

		if !validateSession(c, sm) {
			return
		}
		c.Next()
	}
}

// validateSession validates the session cookie/header and sets user context.
// Returns false and writes 401 if auth fails; true if auth succeeded or was skipped.
// Does NOT call c.Next() — callers must do that themselves.
func validateSession(c *gin.Context, sm *SessionManager) bool {
	// Get session cookie.
	sid, err := c.Cookie("kora_sid")
	if err != nil {
		// Try Authorization header.
		authHeader := c.GetHeader("Authorization")
		if stringsHasPrefix(authHeader, "Bearer ") {
			sid = authHeader[7:]
		}
	}

	if sid == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		c.Abort()
		return false
	}

	// Use site-specific DB for session validation if available.
	sessionSM := sm
	if siteDB, exists := c.Get("site_db"); exists {
		if sdb, ok := siteDB.(*sql.DB); ok {
			sessionSM = sessionManagerForRequest(c, sdb)
		}
	}

	site := c.GetString("site_name")
	user, createdAt, err := sessionSM.GetSession(site, sid)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired session"})
		c.Abort()
		return false
	}

	c.Set("user", user.Name)
	c.Set("user_obj", user)
	c.Set("session_sid", sid)
	c.Set("session_created_at", createdAt)

	// Set role info for permission checks.
	if len(user.Roles) > 0 {
		c.Set("user_role", user.Roles[0])
		c.Set("user_roles", user.Roles)
	} else {
		c.Set("user_role", "")
		c.Set("user_roles", []string{})
	}
	actor := contract.ActorContext{
		PrincipalID: user.Name, PrincipalType: contract.PrincipalHuman,
		SubjectUserID: user.Name, Site: site, Roles: append([]string(nil), user.Roles...),
		AuthenticatedAt: createdAt,
	}
	tenantID := c.GetString("site_id")
	if tenantID == "" {
		// Compatibility for auth middleware callers that do not yet receive the
		// routed stable tenant ID. Production tenant routing always sets site_id.
		tenantID = site
	}
	execution, err := policy.NewVerifiedExecution(contract.TenantID(tenantID), actor)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "session tenant binding is invalid"})
		c.Abort()
		return false
	}
	c.Set("policy_execution", execution)
	return true
}

func stringsHasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func requireAuthenticatedAuthUser(c *gin.Context, sm *SessionManager) (*SessionManager, *User, bool) {
	targetSM := sm
	if siteDB, exists := c.Get("site_db"); exists {
		if sdb, ok := siteDB.(*sql.DB); ok && sdb != nil {
			targetSM = sessionManagerForRequest(c, sdb)
		}
	}
	if !validateSession(c, targetSM) {
		return nil, nil, false
	}
	userVal, exists := c.Get("user_obj")
	user, ok := userVal.(*User)
	if !exists || !ok || user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		c.Abort()
		return nil, nil, false
	}
	return targetSM, user, true
}

// RegisterAuthRoutes registers authentication endpoints.
type MagicLinkMailer interface {
	SendContext(context.Context, *email.Message) error
}

func RegisterAuthRoutes(router *gin.Engine, sm *SessionManager, db *sql.DB, mailer MagicLinkMailer) {
	registerAuthRoutes(router, sm, db, mailer, nil, "")
}

// RegisterAuthRoutesWithPublicAppBaseURL registers authentication routes that
// send sign-in links through the configured public app origin. This is needed
// for path-routed workspaces where request Host may identify Engine or a tenant
// vanity hostname rather than the browser-facing application.
func RegisterAuthRoutesWithPublicAppBaseURL(router *gin.Engine, sm *SessionManager, db *sql.DB, mailer MagicLinkMailer, publicAppBaseURL string) error {
	if err := ValidatePublicAppBaseURL(publicAppBaseURL); err != nil {
		return fmt.Errorf("invalid public application base URL: %w", err)
	}
	registerAuthRoutes(router, sm, db, mailer, nil, publicAppBaseURL)
	return nil
}

func registerAuthRoutes(router *gin.Engine, sm *SessionManager, db *sql.DB, mailer MagicLinkMailer, registry *ProviderRegistry, publicAppBaseURL string) {
	auth := router.Group("/api/auth")
	{

		auth.POST("/logout", func(c *gin.Context) {
			sid, _ := c.Cookie("kora_sid")
			if sid == "" {
				authHeader := c.GetHeader("Authorization")
				if strings.HasPrefix(authHeader, "Bearer ") {
					sid = strings.TrimPrefix(authHeader, "Bearer ")
				}
			}
			if sid != "" {
				// Use site DB if available.
				logoutSM := sm
				if siteDB, exists := c.Get("site_db"); exists {
					if sdb, ok := siteDB.(*sql.DB); ok {
						logoutSM = sessionManagerForRequest(c, sdb)
					}
				}
				logoutSM.DeleteSession(sid)
			}
			SetSecureCookie(c, "kora_sid", "", -1, "/", true)
			c.JSON(http.StatusOK, gin.H{"message": "logged out"})
		})

		auth.GET("/me", func(c *gin.Context) {
			sid, _ := c.Cookie("kora_sid")
			if sid == "" {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
				return
			}
			// Use site DB if available.
			meSM := sm
			if siteDB, exists := c.Get("site_db"); exists {
				if sdb, ok := siteDB.(*sql.DB); ok {
					meSM = sessionManagerForRequest(c, sdb)
				}
			}
			site := c.GetString("site_name")
			user, _, err := meSM.GetSession(site, sid)
			if err != nil {
				slog.Warn("session validation failed for /me", "error", err)
				c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired session"})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"data": gin.H{
					"name":      user.Name,
					"email":     user.Email,
					"full_name": user.FullName,
					"roles":     user.Roles,
				},
			})
		})
	}
	if mailer != nil && strings.TrimSpace(publicAppBaseURL) != "" {
		registerMagicLinkRoutes(auth, db, mailer, publicAppBaseURL)
	}
}

func registerMagicLinkRoutes(auth *gin.RouterGroup, db *sql.DB, mailer MagicLinkMailer, publicAppBaseURL string) {
	auth.POST("/magic-link/request", CSRFMiddleware(), func(c *gin.Context) {
		var request struct {
			Email string `json:"email"`
		}
		if err := c.ShouldBindJSON(&request); err != nil || strings.TrimSpace(request.Email) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "auth.invalid_request", "message": "A valid email is required."}})
			return
		}
		site := c.GetString("site_name")
		emailAddr := strings.TrimSpace(request.Email)
		if !allowMagicLinkRequest(site, emailAddr, c.ClientIP(), time.Now()) {
			c.JSON(http.StatusAccepted, gin.H{"data": gin.H{"message": "If that account can sign in, a link will arrive by email."}})
			return
		}
		targetDB := db
		if value, ok := c.Get("site_db"); ok {
			if siteDB, valid := value.(*sql.DB); valid && siteDB != nil {
				targetDB = siteDB
			}
		}
		if targetDB == nil || site == "" {
			c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "site.not_found", "message": "Site not found."}})
			return
		}
		manager := sessionManagerForRequest(c, targetDB)
		user, err := manager.FindUserByEmailContext(c.Request.Context(), site, emailAddr)
		if err == nil {
			if _, err := issueOneTimeLinkWithPublicAppBaseURL(c, targetDB, site, user, mailer, publicAppBaseURL); err != nil {
				slog.Error("issuing sign-in link", "site", site, "error", err)
			}
		}
		c.JSON(http.StatusAccepted, gin.H{"data": gin.H{"message": "If that account can sign in, a link will arrive by email."}})
	})

	auth.POST("/magic-link/verify", CSRFMiddleware(), func(c *gin.Context) {
		var request struct {
			Token string `json:"token"`
		}
		if err := c.ShouldBindJSON(&request); err != nil || strings.TrimSpace(request.Token) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "auth.invalid_request", "message": "A sign-in token is required."}})
			return
		}
		site := c.GetString("site_name")
		targetDB := db
		if value, ok := c.Get("site_db"); ok {
			if siteDB, valid := value.(*sql.DB); valid && siteDB != nil {
				targetDB = siteDB
			}
		}
		if targetDB == nil || site == "" {
			c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "site.not_found", "message": "Site not found."}})
			return
		}
		manager := sessionManagerForRequest(c, targetDB)
		user, sid, expiresAt, err := manager.consumeMagicLink(c.Request.Context(), site, strings.TrimSpace(request.Token))
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "auth.magic_link_invalid", "message": "This sign-in link is invalid, expired, or already used."}})
			return
		}
		SetSecureCookie(c, "kora_sid", sid, int(SessionLifetime.Seconds()), "/", true)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"user": user, "expires_at": expiresAt.UTC().Format(time.RFC3339)}})
	})
}

// parseTime parses a datetime string from the database (MySQL DATETIME or SQLite TEXT).
func parseTime(s string) (time.Time, error) {
	formats := []string{
		time.RFC3339Nano,                      // "2006-01-02T15:04:05.999999999Z07:00"
		"2006-01-02 15:04:05.999999999-07:00", // SQLite with nanoseconds + timezone
		"2006-01-02 15:04:05.999999-07:00",    // SQLite with microseconds + timezone
		"2006-01-02 15:04:05-07:00",           // SQLite with timezone
		"2006-01-02 15:04:05.999999",          // MySQL with microseconds
		"2006-01-02 15:04:05",                 // MySQL without fractional seconds
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time format: %q", s)
}

func parseDBTimeValue(v any) (time.Time, error) {
	switch t := v.(type) {
	case time.Time:
		return t, nil
	case string:
		return parseTime(t)
	case []byte:
		return parseTime(string(t))
	default:
		return time.Time{}, fmt.Errorf("unrecognized time value type %T", v)
	}
}

func buildMagicLinkURL(c *gin.Context, token string) string {
	scheme := "http"
	if proto := strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")); proto != "" {
		scheme = proto
	} else if c.Request.TLS != nil {
		scheme = "https"
	}
	host := strings.TrimSpace(c.GetHeader("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(c.Request.Host)
	}
	if host == "" {
		host = c.Request.URL.Host
	}
	return fmt.Sprintf("%s://%s%s?magic_token=%s", scheme, host, magicLinkLoginPath(c), token)
}

func buildMagicLinkURLWithPublicAppBaseURL(c *gin.Context, token, publicAppBaseURL string) (string, error) {
	if strings.TrimSpace(publicAppBaseURL) == "" {
		return buildMagicLinkURL(c, token), nil
	}
	if c == nil {
		return "", errors.New("magic-link request context is unavailable")
	}
	return TenantPathMagicLinkURL(publicAppBaseURL, c.GetString("site_name"), token)
}

func magicLinkLoginPath(c *gin.Context) string {
	site := strings.TrimSpace(c.GetString("site_name"))
	if site == "" {
		return "/workspace/auth/login"
	}
	if cookieSite, err := c.Cookie("kora_site"); err == nil && strings.TrimSpace(cookieSite) == site {
		return "/s/" + site + "/workspace/auth/login"
	}
	return "/workspace/auth/login"
}

func enforceAuthRateLimit(c *gin.Context, key string, limit int, window time.Duration) bool {
	ok, retryAfter := allowAuthRequest(key, limit, window)
	if ok {
		return true
	}
	if retryAfter > 0 {
		c.Header("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds())+1))
	}
	c.JSON(http.StatusTooManyRequests, gin.H{
		"error": gin.H{
			"type":    "rate_limited",
			"message": "Too many authentication attempts. Please try again later.",
		},
	})
	return false
}

func issueOneTimeLink(c *gin.Context, targetDB *sql.DB, site string, user *User, mailer MagicLinkMailer) (string, error) {
	return issueOneTimeLinkWithPublicAppBaseURL(c, targetDB, site, user, mailer, "")
}

func allowMagicLinkRequest(site, emailAddr, remoteIP string, now time.Time) bool {
	site = strings.ToLower(strings.TrimSpace(site))
	keys := []string{
		"ip:" + hashMagicLinkToken(site+"\x00"+strings.TrimSpace(remoteIP)),
		"email:" + hashMagicLinkToken(site+"\x00"+strings.ToLower(strings.TrimSpace(emailAddr))),
	}
	magicLinkRateLimit.Lock()
	defer magicLinkRateLimit.Unlock()
	cutoff := now.Add(-authRateLimitWindow)
	if len(magicLinkRateLimit.requests) > 4096 {
		for bucket, times := range magicLinkRateLimit.requests {
			if len(times) == 0 || !times[len(times)-1].After(cutoff) {
				delete(magicLinkRateLimit.requests, bucket)
			}
		}
	}
	pruned := make(map[string][]time.Time, len(keys))
	allowed := true
	for _, key := range keys {
		requests := make([]time.Time, 0, len(magicLinkRateLimit.requests[key])+1)
		for _, requestedAt := range magicLinkRateLimit.requests[key] {
			if requestedAt.After(cutoff) {
				requests = append(requests, requestedAt)
			}
		}
		pruned[key] = requests
		if len(requests) >= authRateLimitMax {
			allowed = false
		}
	}
	newBuckets := 0
	for _, key := range keys {
		if _, exists := magicLinkRateLimit.requests[key]; !exists {
			newBuckets++
		}
	}
	if len(magicLinkRateLimit.requests)+newBuckets > 4096 {
		allowed = false
	}
	for key, requests := range pruned {
		if allowed {
			requests = append(requests, now)
		}
		magicLinkRateLimit.requests[key] = requests
	}
	return allowed
}

func issueOneTimeLinkWithPublicAppBaseURL(c *gin.Context, targetDB *sql.DB, site string, user *User, mailer MagicLinkMailer, publicAppBaseURL string) (string, error) {
	if targetDB == nil {
		return "", fmt.Errorf("no database connection available: %w", ErrNoDBConnection)
	}
	if mailer == nil {
		return "", fmt.Errorf("magic link email delivery is not configured")
	}
	if configured, ok := mailer.(interface{ Configured() bool }); ok && !configured.Configured() {
		return "", fmt.Errorf("magic link email delivery is not configured")
	}

	token := generateMagicLinkTokenFn()
	tokenHash := hashMagicLinkToken(token)
	expiresAt := time.Now().Add(magicLinkLifetime)
	linkID := "ml-" + generateSessionID()
	linkURL, err := buildMagicLinkURLWithPublicAppBaseURL(c, token, publicAppBaseURL)
	if err != nil {
		return "", fmt.Errorf("build magic-link URL: %w", err)
	}

	manager := sessionManagerForRequest(c, targetDB)
	ctx, cancel := context.WithTimeout(c.Request.Context(), dbTimeout)
	_, err = targetDB.ExecContext(ctx,
		manager.sql(`INSERT INTO _kora_magic_link (id, site, email, token_hash, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`),
		linkID, site, user.Email, tokenHash, expiresAt, time.Now(),
	)
	cancel()
	if err != nil {
		return "", fmt.Errorf("creating magic link token failed: %w", err)
	}

	subject, textBody, htmlBody := email.MagicLinkTemplate("Kigae", linkURL, int(magicLinkLifetime.Minutes()))
	if err := mailer.SendContext(c.Request.Context(), &email.Message{
		To:       []string{user.Email},
		Subject:  subject,
		TextBody: textBody,
		HTMLBody: htmlBody,
	}); err != nil {
		revokeCtx, revokeCancel := context.WithTimeout(context.Background(), dbTimeout)
		_, revokeErr := targetDB.ExecContext(revokeCtx,
			manager.sql(`UPDATE _kora_magic_link SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ? AND used_at IS NULL`),
			time.Now(), linkID,
		)
		revokeCancel()
		if revokeErr != nil {
			return "", fmt.Errorf("sending magic link email failed: %w (also failed to revoke link after uncertain delivery: %v)", err, revokeErr)
		}
		return "", fmt.Errorf("sending magic link email failed: %w", err)
	}
	return token, nil
}
