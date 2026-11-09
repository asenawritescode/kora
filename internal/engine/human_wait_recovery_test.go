package engine

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	knet "github.com/asenawritescode/kora/internal/net"
)

func TestStartHumanWaitRecoveryWorkerRegistersOnTenantGeneration(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create tenant DB:", err)
	}
	defer database.Close()
	processCtx, cancel := context.WithCancel(context.Background())
	cancel()
	runtime := &knet.LoadedSite{SiteID: "tenant-human-wait", DBType: "mysql", DB: database}
	if err := StartHumanWaitRecoveryWorker(runtime, processCtx); err != nil {
		t.Fatalf("StartHumanWaitRecoveryWorker: %v", err)
	}
	if err := runtime.StopWorkersContext(context.Background()); err != nil {
		t.Fatalf("join tenant generation worker: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStartHumanWaitRecoveryWorkerRequiresStableTenantIdentity(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal("create tenant DB:", err)
	}
	defer database.Close()
	if err := StartHumanWaitRecoveryWorker(&knet.LoadedSite{DB: database}, context.Background()); err == nil {
		t.Fatal("StartHumanWaitRecoveryWorker without tenant identity succeeded")
	}
}
