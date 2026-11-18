//go:build integration

package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
)

const fenceTenant = contract.TenantID("tenant-fence")

// TestModuleSnapshotFenceAdmitsConcurrentWritersAndExcludesLifecycle runs on
// disposable MySQL and PostgreSQL (KORA_KERNEL_DISPOSABLE_*_DSN). Two
// module-owned writes in one tenant must both be admitted while both
// transactions are open. A lifecycle change must wait for them, and once it
// commits, new writes must be refused.
func TestModuleSnapshotFenceAdmitsConcurrentWritersAndExcludesLifecycle(t *testing.T) {
	for _, tc := range []struct{ name, env string }{
		{"mysql", "KORA_KERNEL_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_KERNEL_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			conn := disposabledb.Open(t, tc.name, dsn).DB
			dialect := kdb.Resolve(tc.name)
			if _, err := conn.Exec(fenceSnapshotDDL(tc.name)); err != nil {
				t.Fatal("snapshot ddl:", err)
			}
			if _, err := conn.Exec(kdb.Rebind(dialect, "INSERT INTO _kora_module_snapshot (tenant_id, revision, snapshot_json) VALUES (?, ?, ?)"),
				string(fenceTenant), 1, fenceSnapshotJSON(t, contract.ModuleActive)); err != nil {
				t.Fatal("seed snapshot:", err)
			}
			reg := doctype.NewRegistry()
			reg.Register(&doctype.DocType{Name: "Sale", ModuleOwnerID: "pos.core"})
			scope := admissionScope{Dialect: dialect, Registry: reg, Snapshots: &moduleSnapshotCache{}}
			payload, err := json.Marshal(RecordCreatePayload{Doctype: "Sale", Data: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			op := Operation{Command: CommandRecordCreate, Payload: payload, Context: OperationContext{TenantID: fenceTenant, ModuleExecution: &contract.ModuleExecution{}}}

			release := make(chan struct{})
			var admitted, writers sync.WaitGroup
			for range 2 {
				admitted.Add(1)
				writers.Add(1)
				go func() {
					defer writers.Done()
					signalAdmitted := sync.OnceFunc(admitted.Done)
					defer signalAdmitted()
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					tx, err := conn.BeginTx(ctx, nil)
					if err != nil {
						t.Errorf("begin writer: %v", err)
						return
					}
					defer func() { _ = tx.Rollback() }()
					cerr := enforceModuleLifecycleAdmissionTx(ctx, tx, op, scope)
					signalAdmitted()
					if cerr != nil {
						t.Errorf("concurrent module write refused: %v", cerr)
						return
					}
					<-release
					if err := tx.Commit(); err != nil {
						t.Errorf("writer commit: %v", err)
					}
				}()
			}
			admitted.Wait()

			lifecycleExec := make(chan error, 1)
			lifecycleDone := make(chan error, 1)
			disabled := fenceSnapshotJSON(t, contract.ModuleDisabled)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				tx, err := conn.BeginTx(ctx, nil)
				if err != nil {
					lifecycleExec <- err
					return
				}
				_, err = tx.ExecContext(ctx, kdb.Rebind(dialect, "UPDATE _kora_module_snapshot SET revision = ?, snapshot_json = ? WHERE tenant_id = ?"),
					2, disabled, string(fenceTenant))
				lifecycleExec <- err
				if err != nil {
					_ = tx.Rollback()
					lifecycleDone <- err
					return
				}
				lifecycleDone <- tx.Commit()
			}()

			select {
			case err := <-lifecycleExec:
				t.Fatalf("lifecycle update finished while module writers hold the fence: %v", err)
			case <-time.After(500 * time.Millisecond):
			}
			close(release)
			writers.Wait()
			if err := <-lifecycleExec; err != nil {
				t.Fatalf("lifecycle update after writers committed: %v", err)
			}
			if err := <-lifecycleDone; err != nil {
				t.Fatalf("lifecycle commit: %v", err)
			}

			assertModuleWriteRefused(t, conn, op, scope)
		})
	}
}

// assertModuleWriteRefused checks that a write admitted before the lifecycle
// change is now refused: the new revision must bypass the cached snapshot.
func assertModuleWriteRefused(t *testing.T, conn *sql.DB, op Operation, scope admissionScope) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	cerr := enforceModuleLifecycleAdmissionTx(ctx, tx, op, scope)
	if cerr == nil || cerr.Type != contract.CodePermissionDenied {
		t.Fatalf("write after lifecycle change: got %v, want permission denied", cerr)
	}
}

func fenceSnapshotJSON(t *testing.T, state contract.ModuleLifecycleState) string {
	t.Helper()
	raw, err := json.Marshal(contract.ModuleSnapshot{TenantID: fenceTenant, Revision: 1, Installations: []contract.ModuleInstallation{
		{TenantID: fenceTenant, ModuleID: "pos.core", State: string(state), Revision: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func fenceSnapshotDDL(driver string) string {
	if driver == "mysql" {
		return "CREATE TABLE _kora_module_snapshot (tenant_id VARCHAR(140) PRIMARY KEY, revision BIGINT NOT NULL, snapshot_json LONGTEXT NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
	}
	return "CREATE TABLE _kora_module_snapshot (tenant_id TEXT PRIMARY KEY, revision BIGINT NOT NULL, snapshot_json TEXT NOT NULL)"
}
