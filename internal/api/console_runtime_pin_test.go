package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/gin-gonic/gin"
)

func TestConsolePasswordResetPinsTenantRuntimeThroughTransaction(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_user SET password_hash = $1, modified = CURRENT_TIMESTAMP WHERE email = $2")).
		WithArgs(sqlmock.AnyArg(), "owner@example.test").WillDelayFor(250 * time.Millisecond).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM _kora_session WHERE \"user\" = (SELECT name FROM _kora_user WHERE email = $1)")).
		WithArgs("owner@example.test").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectClose()
	runtime := &knet.LoadedSite{SiteID: "tenant-reset-pin", Name: "reset.example", DBType: "postgres", DB: database}
	router := knet.NewSiteRouter([]*knet.LoadedSite{runtime})
	handler := &ConsoleHandler{SiteRouter: router}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/console/sites/reset.example/reset-password", strings.NewReader(`{"email":"owner@example.test","new_password":"new-password"}`))
	ctx.Params = gin.Params{{Key: "name", Value: runtime.Name}}
	done := make(chan struct{})
	go func() {
		handler.HandleResetSitePassword(ctx)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for database.Stats().InUse == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if database.Stats().InUse == 0 {
		t.Fatal("password reset did not begin its tenant transaction")
	}
	if retired := router.RemoveSiteByID(runtime.SiteID); retired != runtime {
		t.Fatalf("retired runtime=%p, want %p", retired, runtime)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- runtime.Close(nil) }()
	select {
	case err := <-closeDone:
		t.Fatalf("runtime closed while password reset was active: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("password reset did not finish")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("password reset status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close retired runtime: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime did not close after password reset released its handle")
	}
	router.FinishRetirement(runtime)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
