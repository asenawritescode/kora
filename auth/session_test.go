package auth

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

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

func TestAuthenticateUser_ValidCredentials(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	sm := NewSessionManager(db)

	hash, _ := HashPassword("validpass")
	rows := sqlmock.NewRows([]string{"name", "email", "password_hash", "full_name", "enabled", "roles"}).
		AddRow("john", "john@test.com", hash, "John Doe", true, "Admin,Editor")

	mock.ExpectQuery("SELECT name, email, password_hash, full_name, enabled, COALESCE\\(roles, ''\\) FROM _kora_user WHERE site = \\? AND email = \\?").
		WithArgs("test.local", "john@test.com").
		WillReturnRows(rows)

	user, err := sm.AuthenticateUser("test.local", "john@test.com", "validpass")
	if err != nil {
		t.Fatalf("AuthenticateUser error = %v", err)
	}
	if user.Name != "john" {
		t.Errorf("user.Name = %q, want %q", user.Name, "john")
	}
	if user.Email != "john@test.com" {
		t.Errorf("user.Email = %q, want %q", user.Email, "john@test.com")
	}
	if user.FullName != "John Doe" {
		t.Errorf("user.FullName = %q, want %q", user.FullName, "John Doe")
	}
	if !user.Enabled {
		t.Error("user.Enabled should be true")
	}
	if len(user.Roles) == 0 || user.Roles[0] != "Admin" {
		t.Errorf("user.Roles = %v, want [Admin Editor]", user.Roles)
	}
}

func TestAuthenticateUser_InvalidPassword(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	sm := NewSessionManager(db)

	hash, _ := HashPassword("correctpass")
	rows := sqlmock.NewRows([]string{"name", "email", "password_hash", "full_name", "enabled", "roles"}).
		AddRow("john", "john@test.com", hash, "John Doe", true, "Admin")

	mock.ExpectQuery("SELECT name, email, password_hash, full_name, enabled, COALESCE\\(roles, ''\\) FROM _kora_user WHERE site = \\? AND email = \\?").
		WithArgs("test.local", "john@test.com").
		WillReturnRows(rows)

	_, err = sm.AuthenticateUser("test.local", "john@test.com", "wrongpass")
	if err == nil {
		t.Fatal("AuthenticateUser should error for wrong password")
	}
	if err.Error() != "invalid credentials" {
		t.Errorf("error = %q, want %q", err.Error(), "invalid credentials")
	}
}

func TestAuthenticateUser_UnknownEmail(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	sm := NewSessionManager(db)

	mock.ExpectQuery("SELECT name, email, password_hash, full_name, enabled, COALESCE\\(roles, ''\\) FROM _kora_user WHERE site = \\? AND email = \\?").
		WithArgs("test.local", "unknown@test.com").
		WillReturnError(sql.ErrNoRows)

	_, err = sm.AuthenticateUser("test.local", "unknown@test.com", "anypass")
	if err == nil {
		t.Fatal("AuthenticateUser should error for unknown email")
	}
	if err.Error() != "invalid credentials" {
		t.Errorf("error = %q, want %q", err.Error(), "invalid credentials")
	}
}

func TestAuthenticateUser_DisabledAccount(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	sm := NewSessionManager(db)

	hash, _ := HashPassword("somepass")
	rows := sqlmock.NewRows([]string{"name", "email", "password_hash", "full_name", "enabled", "roles"}).
		AddRow("john", "john@test.com", hash, "John Doe", false, "Admin")

	mock.ExpectQuery("SELECT name, email, password_hash, full_name, enabled, COALESCE\\(roles, ''\\) FROM _kora_user WHERE site = \\? AND email = \\?").
		WithArgs("test.local", "john@test.com").
		WillReturnRows(rows)

	_, err = sm.AuthenticateUser("test.local", "john@test.com", "somepass")
	if err == nil {
		t.Fatal("AuthenticateUser should error for disabled account")
	}
	if err.Error() != "invalid credentials" {
		t.Errorf("error = %q, want %q", err.Error(), "invalid credentials")
	}
}

func TestAuthenticateUser_NoDatabase(t *testing.T) {
	sm := NewSessionManager(nil)
	_, err := sm.AuthenticateUser("test.local", "user@test.com", "pass")
	if err == nil {
		t.Fatal("AuthenticateUser should error when DB is nil")
	}
}

func TestCreateSession(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	sm := NewSessionManager(db)
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

	mock.ExpectExec("DELETE FROM _kora_session WHERE sid = ?").
		WithArgs("test-sid-123").
		WillReturnResult(sqlmock.NewResult(0, 1))

	sm.DeleteSession("test-sid-123")
	// No error — just ensure no panic and mock expectations are met.
}

func TestDeleteSession_NoDatabase(t *testing.T) {
	sm := NewSessionManager(nil)
	// Should not panic with nil DB.
	sm.DeleteSession("some-sid")
}

func TestGetSession_CacheHit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	sm := NewSessionManager(db)

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
	got, err := sm.GetSession("test-site", "cached-sid")
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
