package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func TestHandleUserDirectoryAllowsAuthenticatedNonAdminAndReturnsMinimalTenantScopedChoices(t *testing.T) {
	handler, reg, mock, database := setupTestHandler(t)
	defer database.Close()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/system/user-directory?q=alice&limit=25", nil)
	injectContext(ctx)
	ctx.Set("policy_execution", routerTestExecution([]string{"Sales Rep"}, nil))
	injectDB(ctx, database, reg)

	query := "SELECT name, email, full_name FROM _kora_user WHERE site = ? AND enabled = ? AND (full_name LIKE ? ESCAPE '!' OR email LIKE ? ESCAPE '!') ORDER BY full_name, name LIMIT ?"
	mock.ExpectQuery(regexp.QuoteMeta(query)).
		WithArgs("test.local", true, "alice%", "alice%", 25).
		WillReturnRows(sqlmock.NewRows([]string{"name", "email", "full_name"}).AddRow("user-1", "alice@example.test", "Alice Example"))

	handler.HandleUserDirectory(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("directory returned HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"full_name":"Alice Example"`) || strings.Contains(recorder.Body.String(), `"roles"`) || strings.Contains(recorder.Body.String(), `"created"`) {
		t.Fatalf("directory response should contain only picker identity fields: %s", recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("directory query expectations: %v", err)
	}
}

func TestHandleUserDirectoryUsesIndexedCaseInsensitivePrefixSearchByDialect(t *testing.T) {
	for _, test := range []struct {
		name       string
		dialect    string
		query      string
		searchTerm string
		enabledArg any
	}{
		{
			name:       "mysql",
			dialect:    "mysql",
			query:      "SELECT name, email, full_name FROM _kora_user WHERE site = ? AND enabled = ? AND (full_name LIKE ? ESCAPE '!' OR email LIKE ? ESCAPE '!') ORDER BY full_name, name LIMIT ?",
			searchTerm: "Alice%",
			enabledArg: true,
		},
		{
			name:       "libsql",
			dialect:    "libsql",
			query:      "SELECT name, email, full_name FROM _kora_user WHERE site = ? AND enabled = ? AND (full_name LIKE ? ESCAPE '!' OR email LIKE ? ESCAPE '!') ORDER BY full_name, name LIMIT ?",
			searchTerm: "Alice%",
			enabledArg: true,
		},
		{
			name:       "postgres",
			dialect:    "postgres",
			query:      "SELECT name, email, full_name FROM _kora_user WHERE site = $1 AND enabled = $2 AND (LOWER(full_name) LIKE $3 ESCAPE '!' OR LOWER(email) LIKE $4 ESCAPE '!') ORDER BY full_name, name LIMIT $5",
			searchTerm: "alice%",
			enabledArg: int64(1),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, reg, mock, database := setupTestHandler(t)
			defer database.Close()
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/api/system/user-directory?q=Alice&limit=25", nil)
			ctx.Set("site_name", "test.local")
			ctx.Set("site_id", "test-tenant")
			ctx.Set("site_db_type", test.dialect)
			ctx.Set("policy_execution", routerTestExecution([]string{"Sales Rep"}, nil))
			injectDB(ctx, database, reg)

			mock.ExpectQuery(regexp.QuoteMeta(test.query)).
				WithArgs("test.local", test.enabledArg, test.searchTerm, test.searchTerm, 25).
				WillReturnRows(sqlmock.NewRows([]string{"name", "email", "full_name"}).AddRow("user-1", "alice@example.test", "Alice Example"))
			handler.HandleUserDirectory(ctx)
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"full_name":"Alice Example"`) {
				t.Fatalf("directory returned HTTP %d: %s", recorder.Code, recorder.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("directory query expectations: %v", err)
			}
		})
	}
}

func TestEscapeLikePatternEscapesPickerSearchMetacharacters(t *testing.T) {
	if got, want := escapeLikePattern("Al!_%"), "Al!!!_!%"; got != want {
		t.Fatalf("escapeLikePattern() = %q, want %q", got, want)
	}
}

func TestHandleUserDirectoryRequiresVerifiedIdentity(t *testing.T) {
	handler, _, _, database := setupTestHandler(t)
	defer database.Close()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/system/user-directory", nil)
	ctx.Set("site_name", "test.local")
	ctx.Set("site_id", "test-tenant")
	injectDB(ctx, database, nil)

	handler.HandleUserDirectory(ctx)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated directory returned HTTP %d, want 401", recorder.Code)
	}
}

func TestHandleUserDirectoryResolvesAValidatedBoundedSetOfUserNames(t *testing.T) {
	handler, reg, mock, database := setupTestHandler(t)
	defer database.Close()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/system/user-directory?names=user-1,user-2,user-1", nil)
	injectContext(ctx)
	ctx.Set("policy_execution", routerTestExecution([]string{"Sales Rep"}, nil))
	injectDB(ctx, database, reg)

	query := "SELECT name, email, full_name FROM _kora_user WHERE site = ? AND name IN (?, ?) ORDER BY full_name, name"
	mock.ExpectQuery(regexp.QuoteMeta(query)).
		WithArgs("test.local", "user-1", "user-2").
		WillReturnRows(sqlmock.NewRows([]string{"name", "email", "full_name"}).
			AddRow("user-1", "alice@example.test", "Alice Example").
			AddRow("user-2", "bob@example.test", "Bob Example"))

	handler.HandleUserDirectory(ctx)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"full_name":"Bob Example"`) {
		t.Fatalf("bounded directory resolution returned HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("directory resolution query expectations: %v", err)
	}
}

func TestHandleUserDirectoryResolvesDisabledHistoricalUserForDisplay(t *testing.T) {
	handler, reg, mock, database := setupTestHandler(t)
	defer database.Close()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/system/user-directory?names=disabled-user", nil)
	injectContext(ctx)
	ctx.Set("policy_execution", routerTestExecution([]string{"Sales Rep"}, nil))
	injectDB(ctx, database, reg)

	query := "SELECT name, email, full_name FROM _kora_user WHERE site = ? AND name IN (?) ORDER BY full_name, name"
	mock.ExpectQuery(regexp.QuoteMeta(query)).
		WithArgs("test.local", "disabled-user").
		WillReturnRows(sqlmock.NewRows([]string{"name", "email", "full_name"}).AddRow("disabled-user", "old@example.test", "Former User"))

	handler.HandleUserDirectory(ctx)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"full_name":"Former User"`) {
		t.Fatalf("historical user label was not resolved: HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("historical label query expectations: %v", err)
	}
}

func TestHandleUserDirectoryRejectsInvalidLimitsAndLongSearch(t *testing.T) {
	for _, path := range []string{
		"/api/system/user-directory?limit=0",
		"/api/system/user-directory?limit=51",
		"/api/system/user-directory?limit=not-a-number",
		"/api/system/user-directory?q=" + strings.Repeat("x", 121),
		"/api/system/user-directory?names=" + strings.Repeat("u,", 100) + "u",
	} {
		t.Run(path, func(t *testing.T) {
			handler, reg, mock, database := setupTestHandler(t)
			defer database.Close()
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, path, nil)
			injectContext(ctx)
			ctx.Set("policy_execution", routerTestExecution([]string{"Sales Rep"}, nil))
			injectDB(ctx, database, reg)

			handler.HandleUserDirectory(ctx)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("invalid directory request returned HTTP %d: %s", recorder.Code, recorder.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("invalid request should not query users: %v", err)
			}
		})
	}
}
