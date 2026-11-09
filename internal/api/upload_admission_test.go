package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

type testUploadAdmission struct {
	acquireErr error
	reserved   uint64
	released   int
}

func (a *testUploadAdmission) Acquire(_ context.Context, reserved uint64) (func(), error) {
	a.reserved = reserved
	if a.acquireErr != nil {
		return nil, a.acquireErr
	}
	return func() { a.released++ }, nil
}

func TestHandleUploadRejectsBeforeParsingWhenProcessBudgetIsFull(t *testing.T) {
	gin.SetMode(gin.TestMode)
	admission := &testUploadAdmission{acquireErr: errors.New("capacity")}
	services := NewSiteRuntimeServices()
	services.Replace("tenant-a", SiteRuntimeService{UploadAdmission: admission})
	h := NewHandler(nil, nil)
	h.RuntimeServices = services
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/upload", nil)
	c.Set("site_name", "tenant-a")
	h.HandleUpload(c)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("upload capacity response = status %d, retry-after %q, body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if admission.reserved == 0 {
		t.Fatal("upload handler did not reserve bounded processing memory")
	}
}

func TestHandleUploadReleasesProcessBudgetAfterRequestFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	admission := &testUploadAdmission{}
	services := NewSiteRuntimeServices()
	services.Replace("tenant-a", SiteRuntimeService{UploadAdmission: admission})
	h := NewHandler(nil, nil)
	h.RuntimeServices = services
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/upload", nil)
	c.Set("site_name", "tenant-a")
	h.HandleUpload(c)
	if admission.released != 1 {
		t.Fatalf("admission releases = %d, want 1", admission.released)
	}
}

func TestMaxUploadBytesHasHardMemoryBound(t *testing.T) {
	t.Setenv("KORA_MAX_UPLOAD_MB", "100")
	if got := maxUploadBytes(); got != 100<<20 {
		t.Fatalf("configured upload limit = %d, want %d", got, int64(100<<20))
	}
	t.Setenv("KORA_MAX_UPLOAD_MB", "2048")
	if got := maxUploadBytes(); got != 1<<30 {
		t.Fatalf("oversized upload setting resolved to %d, want hard cap %d", got, int64(1<<30))
	}
	t.Setenv("KORA_MAX_UPLOAD_MB", "not-a-number")
	if got := maxUploadBytes(); got != 50<<20 {
		t.Fatalf("invalid upload setting = %d, want default %d", got, int64(50<<20))
	}
}
