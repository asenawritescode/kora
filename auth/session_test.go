package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

func TestBuildMagicLinkURL_PathBasedSite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "https://app.kora.test/api/auth/magic-link/request", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "app.kora.test")
	req.AddCookie(&http.Cookie{Name: "kora_site", Value: "live-demo"})
	c.Request = req
	c.Set("site_name", "live-demo")

	got := buildMagicLinkURL(c, "abc123")
	want := "https://app.kora.test/s/live-demo/workspace/auth/login?magic_token=abc123"
	if got != want {
		t.Fatalf("buildMagicLinkURL() = %q, want %q", got, want)
	}
}

func TestFindUserByEmailContextHonorsCancellation(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	manager := &SessionManager{DB: database}
	if _, err := manager.FindUserByEmailContext(ctx, "tenant-a", "owner@example.test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("FindUserByEmailContext error = %v, want context.Canceled", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTenantMagicLinkURLUsesOwnedHostAndHTTPS(t *testing.T) {
	link, err := TenantMagicLinkURL("Acme.example.test.", "token+value")
	if err != nil {
		t.Fatal(err)
	}
	if link != "https://acme.example.test/workspace/auth/login?magic_token=token%2Bvalue" {
		t.Fatalf("tenant magic-link URL = %q", link)
	}
	if _, err := TenantMagicLinkURL("https://attacker.example", "token"); err == nil {
		t.Fatal("URL-valued invitation host was accepted")
	}
}

func TestTenantPathMagicLinkURLUsesConfiguredApplicationOrigin(t *testing.T) {
	link, err := TenantPathMagicLinkURL("http://localhost:5173/", "Clientele", "token+value")
	if err != nil {
		t.Fatal("build path-based magic link:", err)
	}
	const want = "http://localhost:5173/s/clientele/workspace/auth/login?magic_token=token%2Bvalue"
	if link != want {
		t.Fatalf("path-based magic-link URL = %q, want %q", link, want)
	}
}

func TestPublicAppBaseURLRejectsUnsafeOrAmbiguousOrigins(t *testing.T) {
	for _, base := range []string{
		"http://app.example.test",
		"https://user:pass@app.example.test",
		"https://app.example.test/subpath",
		"https://app.example.test?next=//attacker.test",
		"https://app.example.test#fragment",
	} {
		t.Run(base, func(t *testing.T) {
			if err := ValidatePublicAppBaseURL(base); err == nil {
				t.Fatalf("unsafe public app base URL %q was accepted", base)
			}
		})
	}
	if _, err := TenantPathMagicLinkURL("https://app.example.test", "../attacker", "token"); err == nil {
		t.Fatal("noncanonical tenant path segment was accepted")
	}
}

type exhaustedSessionCleanupAdmission struct{ called chan struct{} }

func (a exhaustedSessionCleanupAdmission) Acquire(context.Context) (func(), error) {
	select {
	case a.called <- struct{}{}:
	default:
	}
	return nil, dbaccess.ErrExhausted
}

func TestSessionDatabaseCleanupDefersWhenWorkAdmissionIsExhausted(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	admissionCalled := make(chan struct{}, 1)
	sm := newPausedSessionManager(database)
	sm.SetCleanupAdmission(exhaustedSessionCleanupAdmission{called: admissionCalled})
	sm.cleanupDatabaseRows()
	select {
	case <-admissionCalled:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not attempt work admission")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("database cleanup ran without admission: %v", err)
	}
	_ = sm.Close()
}

func TestOwnedSessionManagerCloseStopsAndJoinsSweeper(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sm := NewOwnedSessionManager(db)
	select {
	case <-sm.done:
		t.Fatal("sweeper stopped before close")
	default:
	}
	if err := sm.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sm.done:
	default:
		t.Fatal("Close returned before sweeper exited")
	}
	if err := sm.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestPausedOwnedSessionManagerStartsOnlyOnRequestAndClosePreventsLateStart(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sm := NewOwnedSessionManagerPaused(db)
	select {
	case <-sm.done:
		t.Fatal("paused manager closed before start")
	default:
	}
	if sm.started {
		t.Fatal("paused manager started its sweeper during construction")
	}
	sm.StartCleanup()
	if !sm.started {
		t.Fatal("StartCleanup did not start the sweeper")
	}
	if err := sm.Close(); err != nil {
		t.Fatal("Close:", err)
	}
	sm.StartCleanup()
	select {
	case <-sm.done:
	default:
		t.Fatal("late StartCleanup restarted a closed manager")
	}
}

func TestSessionManagersAreIndependentAndClosable(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first := NewSessionManager(db)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := NewSessionManager(db)
	if second == first {
		t.Fatal("NewSessionManager returned shared state for the same database")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCacheBudgetBoundsAggregateAcrossManagersAndReleasesOnClose(t *testing.T) {
	user := &User{Name: "user", Email: "user@example.test", Roles: []string{"Reader"}}
	firstSize := estimateSessionCacheEntryBytes("session-0", "tenant", user)
	budget, err := NewSessionCacheBudget(firstSize * 2)
	if err != nil {
		t.Fatal("create aggregate cache budget:", err)
	}
	managers := []*SessionManager{
		newSessionManagerWithCacheLimits(nil, 1<<20, 100),
		newSessionManagerWithCacheLimits(nil, 1<<20, 100),
		newSessionManagerWithCacheLimits(nil, 1<<20, 100),
	}
	for _, manager := range managers {
		manager.SetCacheBudget(budget)
	}
	now := time.Now()
	for i, manager := range managers {
		manager.cacheSession(
			fmt.Sprintf("session-%d", i), "tenant", user,
			now, now.Add(time.Hour), now,
		)
	}
	if got := budget.Snapshot().Bytes; got != firstSize*2 {
		t.Fatalf("shared cache bytes = %d, want cap of two entries %d", got, firstSize*2)
	}
	if managers[0].CacheSnapshot().Entries != 1 || managers[1].CacheSnapshot().Entries != 1 || managers[2].CacheSnapshot().Entries != 0 {
		t.Fatalf("manager cache entries = %d, %d, %d; expected global budget to refuse third retention", managers[0].CacheSnapshot().Entries, managers[1].CacheSnapshot().Entries, managers[2].CacheSnapshot().Entries)
	}
	if err := managers[0].Close(); err != nil {
		t.Fatal("close first manager:", err)
	}
	managers[2].cacheSession("session-3", "tenant", &User{Name: "user", Email: "user@example.test", Roles: []string{"Reader"}}, now, now.Add(time.Hour), now)
	if managers[2].CacheSnapshot().Entries != 1 {
		t.Fatal("released global cache capacity was not reused")
	}
	for _, manager := range managers[1:] {
		if err := manager.Close(); err != nil {
			t.Fatal("close manager:", err)
		}
	}
	if got := budget.Snapshot().Bytes; got != 0 {
		t.Fatalf("shared cache bytes after manager close = %d, want 0", got)
	}
}

func TestSessionCacheBudgetConcurrentManagersStayWithinAggregateLimit(t *testing.T) {
	user := &User{Name: "user", Email: "user@example.test", Roles: []string{"Reader"}}
	entryBytes := estimateSessionCacheEntryBytes("session-000", "tenant", user)
	budget, err := NewSessionCacheBudget(entryBytes * 8)
	if err != nil {
		t.Fatal(err)
	}
	const count = 64
	managers := make([]*SessionManager, count)
	for index := range managers {
		managers[index] = newSessionManagerWithCacheLimits(nil, 1<<20, 100)
		managers[index].SetCacheBudget(budget)
	}
	now := time.Now()
	var workers sync.WaitGroup
	for index, manager := range managers {
		workers.Add(1)
		go func(index int, manager *SessionManager) {
			defer workers.Done()
			manager.cacheSession(fmt.Sprintf("session-%03d", index), "tenant", user, now, now.Add(time.Hour), now)
		}(index, manager)
	}
	workers.Wait()
	snapshot := budget.Snapshot()
	if snapshot.Bytes > snapshot.MaxBytes || snapshot.Bytes == 0 {
		t.Fatalf("concurrent aggregate cache snapshot = %+v", snapshot)
	}
	var managerBytes uint64
	for _, manager := range managers {
		managerBytes += manager.CacheSnapshot().Bytes
	}
	if managerBytes != snapshot.Bytes {
		t.Fatalf("manager cache bytes = %d, shared accounting = %d", managerBytes, snapshot.Bytes)
	}
	for _, manager := range managers {
		if err := manager.Close(); err != nil {
			t.Fatal("close manager:", err)
		}
	}
	if got := budget.Snapshot().Bytes; got != 0 {
		t.Fatalf("shared cache bytes after concurrent manager close = %d, want 0", got)
	}
}

func TestSessionManagerChangingSharedCacheBudgetReleasesPriorEntries(t *testing.T) {
	first, err := NewSessionCacheBudget(4096)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSessionCacheBudget(4096)
	if err != nil {
		t.Fatal(err)
	}
	manager := newSessionManagerWithCacheLimits(nil, 1<<20, 100)
	manager.SetCacheBudget(first)
	now := time.Now()
	manager.cacheSession("session", "tenant", &User{Name: "user"}, now, now.Add(time.Hour), now)
	if first.Snapshot().Bytes == 0 {
		t.Fatal("first shared budget did not account for cached entry")
	}
	manager.SetCacheBudget(second)
	if first.Snapshot().Bytes != 0 || second.Snapshot().Bytes != 0 || manager.CacheSnapshot().Entries != 0 {
		t.Fatalf("budget switch state: first=%+v second=%+v cache=%+v", first.Snapshot(), second.Snapshot(), manager.CacheSnapshot())
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRequestUsesRuntimeOwnedSessionManager(t *testing.T) {
	gin.SetMode(gin.TestMode)
	expected := NewOwnedSessionManager(nil)
	defer expected.Close()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("site_session_manager", expected)
	got := sessionManagerForRequest(c, nil)
	if got != expected {
		t.Fatal("request did not use the routed tenant runtime's manager")
	}
}

func TestRequestFallbackSessionManagerIsRequestScopedWithoutSweeper(t *testing.T) {
	firstContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	first := sessionManagerForRequest(firstContext, nil)
	if sessionManagerForRequest(firstContext, nil) != first {
		t.Fatal("same request did not reuse its fallback session manager")
	}
	select {
	case <-first.done:
	default:
		t.Fatal("request-scoped fallback started a background sweeper")
	}
	secondContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	if second := sessionManagerForRequest(secondContext, nil); second == first {
		t.Fatal("separate requests shared a session manager")
	}
}

func TestBuildMagicLinkURL_HostBasedSite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "https://live-demo.kora.test/api/auth/magic-link/request", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "live-demo.kora.test")
	c.Request = req
	c.Set("site_name", "live-demo")

	got := buildMagicLinkURL(c, "abc123")
	want := "https://live-demo.kora.test/workspace/auth/login?magic_token=abc123"
	if got != want {
		t.Fatalf("buildMagicLinkURL() = %q, want %q", got, want)
	}
}

func TestBuildMagicLinkURLUsesConfiguredPublicAppBaseURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "https://tenant-alias.example.test/api/auth/magic-link/request", nil)
	c.Set("site_name", "clientele")
	got, err := buildMagicLinkURLWithPublicAppBaseURL(c, "token+value", "http://localhost:5173")
	if err != nil {
		t.Fatal("build configured path-based magic link:", err)
	}
	const want = "http://localhost:5173/s/clientele/workspace/auth/login?magic_token=token%2Bvalue"
	if got != want {
		t.Fatalf("configured magic-link URL = %q, want %q", got, want)
	}
}

func TestHashPassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{"simple", "password123", false},
		{"complex", "P@ssw0rd!-#&$%", false},
		{"empty", "", false},
		{"long", "a-very-long-password-that-should-still-work-fine-with-bcrypt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash, err := HashPassword(tt.password)
			if (err != nil) != tt.wantErr {
				t.Fatalf("HashPassword() error = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr && hash == "" {
				t.Fatal("HashPassword returned empty hash")
			}
			if tt.password != "" {
				// Verify bcrypt round-trip.
				err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(tt.password))
				if err != nil {
					t.Errorf("bcrypt compare failed: %v", err)
				}
			}
		})
	}
}

func TestCreateSession(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	sm := NewSessionManager(db)
	defer sm.Close()
	SessionLifetime = 24 * time.Hour

	user := &User{
		Name:     "john",
		Email:    "john@test.com",
		FullName: "John Doe",
		Roles:    []string{"Admin"},
		Enabled:  true,
	}

	mock.ExpectExec("INSERT INTO _kora_session").
		WithArgs(sqlmock.AnyArg(), "test.local", "john", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	sid, err := sm.CreateSession("test.local", user)
	if err != nil {
		t.Fatalf("CreateSession error = %v", err)
	}
	if sid == "" {
		t.Fatal("CreateSession returned empty sid")
	}
	if len(sid) != 64 {
		t.Errorf("sid length = %d, want 64", len(sid))
	}
}

func TestCreateSession_NoDatabase(t *testing.T) {
	sm := NewSessionManager(nil)
	defer sm.Close()
	_, err := sm.CreateSession("test.local", &User{Name: "test"})
	if err == nil {
		t.Fatal("CreateSession should error when DB is nil")
	}
}

func TestDeleteSession(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	sm := NewSessionManager(db)
	defer sm.Close()

	mock.ExpectExec("DELETE FROM _kora_session WHERE sid = ?").
		WithArgs("test-sid-123").
		WillReturnResult(sqlmock.NewResult(0, 1))

	sm.DeleteSession("test-sid-123")
	// No error — just ensure no panic and mock expectations are met.
}

func TestDeleteSession_NoDatabase(t *testing.T) {
	sm := NewSessionManager(nil)
	defer sm.Close()
	// Should not panic with nil DB.
	sm.DeleteSession("some-sid")
}

func TestGetSessionRejectsCrossSiteCacheHit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	sm := NewSessionManager(db)
	defer sm.Close()
	user := &User{Name: "cached-user", Email: "cached@test.com", Roles: []string{"Admin"}, Enabled: true}
	sm.cacheMu.Lock()
	sm.cache["sid-123"] = &sessionCacheEntry{
		user:      user,
		site:      "site-a",
		cachedAt:  time.Now(),
		expiresAt: time.Now().Add(time.Hour),
		createdAt: time.Now().Add(-time.Minute),
	}
	sm.cacheMu.Unlock()

	mock.ExpectQuery(`SELECT data, expires_at, created_at FROM _kora_session WHERE site = \? AND sid = \?`).
		WithArgs("site-b", "sid-123").
		WillReturnError(sql.ErrNoRows)

	got, _, err := sm.GetSession("site-b", "sid-123")
	if err == nil || got != nil {
		t.Fatalf("expected cross-site cache hit to be rejected, got user=%+v err=%v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestGetSession_CacheHit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	sm := NewSessionManager(db)
	defer sm.Close()

	// Pre-populate cache.
	user := &User{Name: "cached-user", Email: "cached@test.com", Roles: []string{"Admin"}, Enabled: true}
	sm.cacheMu.Lock()
	sm.cache["cached-sid"] = &sessionCacheEntry{
		user:      user,
		site:      "test-site",
		cachedAt:  time.Now(),
		expiresAt: time.Now().Add(1 * time.Hour),
	}
	sm.cacheMu.Unlock()

	// GetSession from cache should NOT hit the database.
	got, _, err := sm.GetSession("test-site", "cached-sid")
	if err != nil {
		t.Fatalf("GetSession error = %v", err)
	}
	if got.Name != "cached-user" {
		t.Errorf("user.Name = %q, want %q", got.Name, "cached-user")
	}

	// Ensure we didn't make any DB calls.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

func TestSessionCacheEnforcesByteLimitAndLRUEviction(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sm := NewSessionManager(db)
	defer sm.Close()
	sm.cacheMaxBytes = 700
	sm.cacheMaxEntries = 4
	now := time.Now()
	user := &User{Name: "User", Email: "user@example.test", FullName: "Example User", Roles: []string{"Reader"}}
	for _, sid := range []string{"sid-a", "sid-b"} {
		sm.cacheSession(sid, "tenant", user, now, now.Add(time.Hour), now)
	}
	cachedUser, _, err := sm.GetSession("tenant", "sid-a")
	if err != nil {
		t.Fatalf("cache hit: %v", err)
	}
	cachedUser.Roles[0] = "MutatedOutsideCache"
	secondRead, _, err := sm.GetSession("tenant", "sid-a")
	if err != nil || secondRead.Roles[0] != "Reader" {
		t.Fatalf("cache user escaped mutable: user=%+v err=%v", secondRead, err)
	}
	sm.cacheSession("sid-c", "tenant", user, now, now.Add(time.Hour), now)
	sm.cacheMu.RLock()
	_, hasA := sm.cache["sid-a"]
	_, hasB := sm.cache["sid-b"]
	_, hasC := sm.cache["sid-c"]
	sm.cacheMu.RUnlock()
	if !hasA || hasB || !hasC {
		t.Fatalf("LRU members: a=%v b=%v c=%v", hasA, hasB, hasC)
	}
	snapshot := sm.CacheSnapshot()
	if snapshot.Bytes > snapshot.MaxBytes || snapshot.Entries != 2 {
		t.Fatalf("cache snapshot = %+v", snapshot)
	}
}

func TestSessionCacheSkipsOversizedEntriesAndSweepsExpiredBytes(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sm := NewSessionManager(db)
	defer sm.Close()
	sm.cacheMaxBytes = 300
	now := time.Now()
	tooLarge := &User{Name: strings.Repeat("x", 512)}
	sm.cacheSession("large", "tenant", tooLarge, now, now.Add(time.Hour), now)
	if got := sm.CacheSnapshot(); got.Entries != 0 || got.Bytes != 0 {
		t.Fatalf("oversized cache snapshot = %+v", got)
	}
	sm.cacheMaxBytes = 1 << 20
	user := &User{Name: "small"}
	sm.cacheSession("expired", "tenant", user, now, now.Add(time.Hour), now.Add(-3*sessionCacheTTL))
	sm.sweepExpiredCache(now)
	if got := sm.CacheSnapshot(); got.Entries != 0 || got.Bytes != 0 {
		t.Fatalf("after TTL sweep = %+v", got)
	}
}

// ---------------------------------------------------------------------------
// CSRF Middleware Tests
// ---------------------------------------------------------------------------

func TestCSRFMiddleware_SkipBearer(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(CSRFMiddleware())
	r.POST("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/test", nil)
	req.Header.Set("Authorization", "Bearer some-token")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (should skip CSRF for Bearer auth)", w.Code, http.StatusOK)
	}
}

func TestCSRFMiddleware_BlockNoToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(CSRFMiddleware())
	r.POST("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d (should block POST without CSRF token)", w.Code, http.StatusForbidden)
	}
}

func TestCSRFMiddleware_SkipSafeMethods(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(CSRFMiddleware())
	r.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	r.HEAD("/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	r.OPTIONS("/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	for _, method := range []string{"GET", "HEAD", "OPTIONS"} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(method, "/test", nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want %d", method, w.Code, http.StatusOK)
		}
	}
}

func TestCSRFMiddleware_ValidToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(CSRFMiddleware())
	r.POST("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/test", nil)
	req.Header.Set("X-Kora-CSRF-Token", "valid-csrf-token")
	req.Header.Set("Cookie", "kora_csrf=valid-csrf-token")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestCSRFMiddleware_MismatchedToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(CSRFMiddleware())
	r.POST("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/test", nil)
	req.Header.Set("X-Kora-CSRF-Token", "token-from-header")
	req.Header.Set("Cookie", "kora_csrf=token-from-cookie")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

// Helper to create a test HTTP request with body.
func TestParseTime(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"mysql format", "2024-01-15 10:30:00", false},
		{"mysql with microseconds", "2024-01-15 10:30:00.123456", false},
		{"rfc3339", "2024-01-15T10:30:00Z", false},
		{"sqlite with tz", "2024-01-15 10:30:00+00:00", false},
		{"empty string", "", true},
		{"invalid", "not-a-date", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := parseTime(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseTime(%q) error = %v, wantErr = %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && parsed.IsZero() {
				t.Error("parsed time should not be zero")
			}
		})
	}
}

func TestSplitRolesStr(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"comma separated", "Admin,Editor,Viewer", []string{"Admin", "Editor", "Viewer"}},
		{"newline separated", "Admin\nEditor\nViewer", []string{"Admin", "Editor", "Viewer"}},
		{"single role", "Admin", []string{"Admin"}},
		{"empty string", "", []string{""}},
		{"with whitespace", " Admin , Editor ", []string{"Admin", "Editor"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitRolesStr(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d: got %v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// Ensure we can use errors in tests.
var _ = errors.New
var _ = strings.TrimSpace

func TestCloudOwnedInteractiveSignInRoutesAreRetired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock:", err)
	}
	defer db.Close()
	sm := NewSessionManager(db)
	defer sm.Close()
	router := gin.New()
	RegisterAuthRoutes(router, sm, db, nil)
	for _, path := range []string{"/api/auth/login", "/api/auth/providers", "/api/auth/magic-link/request", "/api/auth/magic-link/verify", "/api/auth/magic-links"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want %d", path, response.Code, http.StatusNotFound)
		}
	}
}
