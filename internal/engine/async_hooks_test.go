package engine

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/doctype"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/script"
)

type retryAsyncHookAdmission struct {
	calls    int
	releases int
	failures int
}

func (a *retryAsyncHookAdmission) Acquire(context.Context) (func(), error) {
	a.calls++
	if a.failures > 0 {
		a.failures--
		return nil, dbaccess.ErrExhausted
	}
	return func() { a.releases++ }, nil
}

func TestAsyncHookQueueReportsFullAndClosedWithoutBlocking(t *testing.T) {
	sink := testAsyncHookSink(1, 1<<20, 1<<20)
	if err := sink.Enqueue(context.Background(), orm.AsyncHookRequest{}); err != nil {
		t.Fatal("first enqueue:", err)
	}
	if err := sink.Enqueue(context.Background(), orm.AsyncHookRequest{}); !errors.Is(err, errAsyncHookQueueFull) {
		t.Fatalf("full queue error = %v, want queue-full error", err)
	}
	sink.Close()
	if err := sink.Enqueue(context.Background(), orm.AsyncHookRequest{}); !errors.Is(err, errAsyncHookQueueClosed) {
		t.Fatalf("enqueue after close error = %v, want queue-closed error", err)
	}
	if _, ok := <-sink.queue; !ok {
		t.Fatal("queue did not preserve its accepted item for draining")
	}
	if _, ok := <-sink.queue; ok {
		t.Fatal("queue should be closed after accepted items drain")
	}
}

func TestAsyncHookQueueCloseSerializesWithConcurrentEnqueue(t *testing.T) {
	sink := testAsyncHookSink(8, 1<<20, 1<<20)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sink.Enqueue(context.Background(), orm.AsyncHookRequest{})
		}()
	}
	sink.Close()
	wg.Wait()
	if err := sink.Enqueue(context.Background(), orm.AsyncHookRequest{}); !errors.Is(err, errAsyncHookQueueClosed) {
		t.Fatalf("enqueue after concurrent close error = %v", err)
	}
}

func TestAsyncHookQueueBoundsPayloadAndAggregateBytes(t *testing.T) {
	large := orm.AsyncHookRequest{Doc: map[string]any{"body": string(make([]byte, 256))}}
	sink := testAsyncHookSink(8, 4096, 512)
	if err := sink.Enqueue(context.Background(), large); !errors.Is(err, ErrAsyncHookPayloadSize) {
		t.Fatalf("oversized payload error = %v", err)
	}

	req := orm.AsyncHookRequest{}
	size, err := estimateHookPayloadBytes(reflect.ValueOf(req), 4096, 0)
	if err != nil {
		t.Fatal("estimate empty request:", err)
	}
	sink = testAsyncHookSink(8, size*2-1, size)
	if err := sink.Enqueue(context.Background(), req); err != nil {
		t.Fatal("first bounded enqueue:", err)
	}
	if err := sink.Enqueue(context.Background(), req); !errors.Is(err, errAsyncHookQueueFull) {
		t.Fatalf("aggregate byte limit error = %v", err)
	}
	if got := sink.Snapshot(); got.QueuedBytes != size || got.QueuedItems != 1 || got.RejectedItems == 0 {
		t.Fatalf("snapshot after rejection = %+v", got)
	}
	item := <-sink.queue
	sink.releaseBytes(item.bytes)
	if err := sink.Enqueue(context.Background(), req); err != nil {
		t.Fatalf("enqueue after releasing capacity: %v", err)
	}
}

func testAsyncHookSink(count int, queuedBytes, eventBytes uint64) *asyncHookQueueSink {
	return &asyncHookQueueSink{
		queue: make(chan asyncHookQueueItem, count), maxQueuedBytes: queuedBytes,
		maxEventBytes: eventBytes,
	}
}

func TestStartAsyncHookWorkerRejectsUnboundedConfiguration(t *testing.T) {
	if worker, err := StartAsyncHookWorker(0, nil, nil, "", nil); !errors.Is(err, ErrAsyncHookQueueSize) || worker != nil {
		t.Fatalf("StartAsyncHookWorker = (%v, %v), want queue-size error", worker, err)
	}
}

func TestAsyncHookAdmissionRetriesWithoutDroppingActiveItem(t *testing.T) {
	admission := &retryAsyncHookAdmission{failures: 2}
	release, err := acquireAsyncHookWork(context.Background(), admission)
	if err != nil {
		t.Fatal("acquire after temporary saturation:", err)
	}
	release()
	if admission.calls != 3 || admission.releases != 1 {
		t.Fatalf("admission attempts/releases = %d/%d, want 3/1", admission.calls, admission.releases)
	}
}

func TestAsyncHookAdmissionTreatsTypedNilAsUnconfigured(t *testing.T) {
	var admission *dbaccess.Admission
	release, err := acquireAsyncHookWork(context.Background(), admission)
	if err != nil {
		t.Fatalf("typed nil admission returned error: %v", err)
	}
	release()
}

func TestAsyncHookAdmissionWaitStopsOnCancellation(t *testing.T) {
	admission := &retryAsyncHookAdmission{failures: 100}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := acquireAsyncHookWork(ctx, admission); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire canceled hook work error = %v, want context.Canceled", err)
	}
}

type captureAsyncScriptRunner struct {
	requests chan script.ExecuteRequest
}

func (r *captureAsyncScriptRunner) Execute(_ context.Context, request script.ExecuteRequest) (*script.ExecuteResult, error) {
	r.requests <- request
	return &script.ExecuteResult{}, nil
}

func (*captureAsyncScriptRunner) Validate(string) error { return nil }
func (*captureAsyncScriptRunner) Close() error          { return nil }

func TestAsyncLifecycleHookCanKeepProviderDisabled(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	mock.ExpectClose()

	runner := &captureAsyncScriptRunner{requests: make(chan script.ExecuteRequest, 2)}
	runtime := &knet.LoadedSite{
		SiteID: "async-workflow-tenant", Name: "async-workflow-tenant", DBType: "mysql",
		DB: database, Registry: doctype.NewRegistry(),
	}
	runtime.Registry.Register(&doctype.DocType{Name: "Task"})
	router := knet.NewSiteRouter([]*knet.LoadedSite{runtime})
	worker, err := NewAsyncHookWorkerWithLimits(2, 1<<20, 1<<20, runner, router, "mysql", nil)
	if err != nil {
		t.Fatal("create async hook worker:", err)
	}
	if err := worker.Start(); err != nil {
		t.Fatal("start async hook worker:", err)
	}
	execution, err := policy.NewVerifiedExecution(contract.TenantID(runtime.Name), contract.ActorContext{
		PrincipalID: "operator@example.test", PrincipalType: contract.PrincipalHuman,
		SubjectUserID: "operator@example.test", Site: runtime.Name, Roles: []string{"Reader", "Auditor"},
	})
	if err != nil {
		t.Fatal("create verified async workflow actor:", err)
	}
	if err := worker.Sink().Enqueue(context.Background(), orm.AsyncHookRequest{
		Site: runtime.Name, Doctype: "Task", Doc: map[string]any{"name": "TASK-1"},
		Rec:       script.Record{Name: "notify", ScriptType: script.TypeDocEvent, Script: "return 1", TimeoutMs: 650},
		Execution: execution, UserRoles: []string{"Reader", "Auditor"}, DisableProvider: true,
	}); err != nil {
		t.Fatal("enqueue async workflow action:", err)
	}
	if err := worker.Sink().Enqueue(context.Background(), orm.AsyncHookRequest{
		Site: runtime.Name, Doctype: "Task", Doc: map[string]any{"name": "TASK-2"},
		Rec: script.Record{Name: "lifecycle", ScriptType: script.TypeDocEvent, Script: "return 1"},
	}); err != nil {
		t.Fatal("enqueue lifecycle hook:", err)
	}
	for index, wantProvider := range []bool{false, true} {
		select {
		case request := <-runner.requests:
			if (request.Provider != nil) != wantProvider {
				t.Fatalf("async request %d provider present = %t, want %t", index, request.Provider != nil, wantProvider)
			}
			if index == 0 && request.ScriptType != script.TypeDocEvent {
				t.Fatalf("async workflow request changed script type: %#v", request)
			}
			if index == 0 && request.Timeout != 650*time.Millisecond {
				t.Fatalf("async workflow timeout = %s, want %s", request.Timeout, 650*time.Millisecond)
			}
			if index == 0 && !reflect.DeepEqual(request.UserRoles, []string{"Reader", "Auditor"}) {
				t.Fatalf("async workflow roles = %#v, want full verified role set", request.UserRoles)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("async hook request %d did not reach the worker", index)
		}
	}
	worker.StopAccepting()
	select {
	case <-worker.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("async hook worker did not stop")
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := router.RuntimeManager().Close(closeCtx); err != nil {
		t.Fatalf("close tenant runtime router: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL mock expectations: %v", err)
	}
}
