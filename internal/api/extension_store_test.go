package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/extension"
	"github.com/gin-gonic/gin"
)

type fakeExtensionStore struct {
	installed map[string]bool
}

func (f *fakeExtensionStore) List(context.Context, string) ([]extension.Summary, error) {
	return nil, nil
}

func (f *fakeExtensionStore) Create(_ context.Context, r extension.Registration) error {
	f.installed[r.Name] = true
	return nil
}

func (f *fakeExtensionStore) Delete(_ context.Context, _, name string) error {
	if !f.installed[name] {
		return extension.ErrNotFound
	}
	delete(f.installed, name)
	return nil
}

func (f *fakeExtensionStore) RotateSecret(_ context.Context, r extension.SecretRotation) error {
	if !f.installed[r.Name] {
		return extension.ErrNotFound
	}
	return nil
}

func TestExtensionHandlersUseInjectedStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &fakeExtensionStore{installed: map[string]bool{"crm": true}}
	h := &Handler{ExtensionStores: func(*sql.DB, kdb.QueryDialect) ExtensionStore { return store }}

	tests := []struct {
		name   string
		handle gin.HandlerFunc
		ext    string
		want   int
	}{
		{"rotate missing", h.HandleExtensionRotateSecret, "gone", http.StatusNotFound},
		{"rotate installed", h.HandleExtensionRotateSecret, "crm", http.StatusOK},
		{"delete installed", h.HandleExtensionDelete, "crm", http.StatusOK},
		{"delete again", h.HandleExtensionDelete, "crm", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			c.Set("site_db", &sql.DB{})
			c.Set("site_name", "acme")
			c.Params = gin.Params{{Key: "name", Value: tt.ext}}
			tt.handle(c)
			if recorder.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, tt.want, recorder.Body.String())
			}
		})
	}
}
