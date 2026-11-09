package engine

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/asenawritescode/kora/internal/api"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/doctype"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/script"
)

var (
	errAsyncHookQueueClosed = errors.New("async hook queue is closed")
	errAsyncHookQueueFull   = errors.New("async hook queue is full")
	ErrAsyncHookQueueSize   = errors.New("async hook queue size must be positive")
	ErrAsyncHookPayloadSize = errors.New("async hook payload exceeds its byte limit")
)

const (
	DefaultAsyncHookQueueSize      = 1000
	DefaultAsyncHookMaxQueuedBytes = 64 << 20
	DefaultAsyncHookMaxEventBytes  = 1 << 20
)

// AsyncHookWorker owns the bounded hook queue and its consumer goroutine. Stop
// accepting before waiting on Done; Cancel is reserved for a timed-out drain.
type AsyncHookWorker struct {
	sink     *asyncHookQueueSink
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	startMu  sync.Mutex
	started  bool
	doneOnce sync.Once
	runFn    func(context.Context)
}

// StartAsyncHookWorker creates one process-owned worker. Its independent
// context lets the process owner drain accepted work before cancellation.
func StartAsyncHookWorker(queueSize int, runner script.Runner, siteRouter *knet.SiteRouter, dbType string, httpAllowlist []string) (*AsyncHookWorker, error) {
	worker, err := NewAsyncHookWorker(queueSize, runner, siteRouter, dbType, httpAllowlist)
	if err != nil {
		return nil, err
	}
	if err := worker.Start(); err != nil {
		return nil, err
	}
	return worker, nil
}

// NewAsyncHookWorker prepares the bounded queue without launching its consumer.
func NewAsyncHookWorker(queueSize int, runner script.Runner, siteRouter *knet.SiteRouter, dbType string, httpAllowlist []string) (*AsyncHookWorker, error) {
	return NewAsyncHookWorkerWithLimits(queueSize, DefaultAsyncHookMaxQueuedBytes, DefaultAsyncHookMaxEventBytes, runner, siteRouter, dbType, httpAllowlist)
}

// NewAsyncHookWorkerWithLimits bounds both queued request count and estimated
// serialized bytes. The estimate is conservative and stops once a limit is hit.
func NewAsyncHookWorkerWithLimits(queueSize int, maxQueuedBytes, maxEventBytes uint64, runner script.Runner, siteRouter *knet.SiteRouter, dbType string, httpAllowlist []string) (*AsyncHookWorker, error) {
	if queueSize <= 0 {
		return nil, ErrAsyncHookQueueSize
	}
	if maxQueuedBytes == 0 || maxEventBytes == 0 || maxEventBytes > maxQueuedBytes {
		return nil, errors.New("async hook byte limits must be positive and event limit cannot exceed queue limit")
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker := &AsyncHookWorker{
		sink: &asyncHookQueueSink{
			queue: make(chan asyncHookQueueItem, queueSize), maxQueuedBytes: maxQueuedBytes,
			maxEventBytes: maxEventBytes,
		},
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	worker.runFn = func(ctx context.Context) { worker.run(ctx, runner, siteRouter, dbType, httpAllowlist) }
	return worker, nil
}

// Start launches the consumer exactly once.
func (w *AsyncHookWorker) Start() error {
	if w == nil {
		return errors.New("async hook worker is required")
	}
	w.startMu.Lock()
	defer w.startMu.Unlock()
	if w.started {
		return nil
	}
	if w.sink.isClosed() {
		w.doneOnce.Do(func() { close(w.done) })
		return nil
	}
	w.started = true
	go func() {
		defer w.doneOnce.Do(func() { close(w.done) })
		w.runFn(w.ctx)
	}()
	return nil
}

func (w *AsyncHookWorker) Sink() orm.AsyncHookSink {
	if w == nil {
		return nil
	}
	return w.sink
}

func (w *AsyncHookWorker) StopAccepting() {
	if w != nil {
		w.sink.Close()
		w.startMu.Lock()
		if !w.started {
			w.doneOnce.Do(func() { close(w.done) })
		}
		w.startMu.Unlock()
	}
}

func (w *AsyncHookWorker) Cancel() {
	if w != nil && w.cancel != nil {
		w.cancel()
	}
}

func (w *AsyncHookWorker) Done() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.done
}

func (w *AsyncHookWorker) run(ctx context.Context, runner script.Runner, siteRouter *knet.SiteRouter, dbType string, httpAllowlist []string) {
	for {
		var req orm.AsyncHookRequest
		select {
		case <-ctx.Done():
			return
		case next, ok := <-w.sink.queue:
			if !ok {
				return
			}
			w.sink.releaseBytes(next.bytes)
			req = next.request
		}
		if runner == nil || siteRouter == nil {
			slog.Warn("async hook dropped because script execution is unavailable", "site", req.Site, "script", req.Rec.Name)
			continue
		}
		_, handle, ok := siteRouter.AcquireByName(ctx, req.Site)
		if !ok || handle == nil {
			continue
		}
		func() {
			defer handle.Release()
			loadedSite := handle.Snapshot()
			if loadedSite == nil || loadedSite.DB == nil || loadedSite.Registry == nil {
				return
			}

			dt := loadedSite.Registry.Get(req.Doctype)
			if dt == nil {
				slog.Warn("async hook worker: doctype not found", "doctype", req.Doctype, "site", req.Site)
				return
			}

			loadedDBType := loadedSite.DBType
			if loadedDBType == "" {
				loadedDBType = dbType
			}
			tm := &orm.TxManager{
				DB:              loadedSite.DB,
				Registry:        loadedSite.Registry,
				Dialect:         kdb.Resolve(loadedDBType),
				ScriptRunner:    runner,
				SiteName:        req.Site,
				CurrentUser:     req.User,
				CurrentUserRole: req.UserRole,
				Execution:       req.Execution,
				SkipHookScripts: append([]string(nil), req.SkipHookScripts...),
			}
			services := loadedSite.Services
			releaseWork, admissionErr := acquireAsyncHookWork(ctx, services.WorkAdmission)
			if admissionErr != nil {
				slog.Warn("async hook deferred because database work admission was unavailable", "site", req.Site, "error", admissionErr)
				return
			}
			defer releaseWork()
			tm.ScriptStore = services.ScriptStore
			tm.ScriptProvider = api.NewScriptProvider(tm, loadedSite.Registry, req.Site, nil, httpAllowlist)

			doc := orm.DocumentFromMap(loadedSite.Registry, req.Doctype, req.Doc)
			if doc == nil {
				doc = doctype.NewDocument(req.Doctype)
			}
			var oldDoc *doctype.Document
			if req.OldDoc != nil {
				oldDoc = orm.DocumentFromMap(loadedSite.Registry, req.Doctype, req.OldDoc)
			}

			var provider script.KoraProvider
			if !req.DisableProvider {
				provider = tm.ScriptProvider
			}
			userRoles := append([]string(nil), req.UserRoles...)
			if len(userRoles) == 0 && !req.Execution.Valid() && req.UserRole != "" {
				userRoles = []string{req.UserRole}
			}
			execReq := script.ExecuteRequest{
				Script:     req.Rec.Script,
				ScriptType: req.Rec.ScriptType,
				ScriptName: req.Rec.Name,
				DocType:    req.Doctype,
				Event:      req.Event,
				Document:   doc.ToMap(),
				User:       req.User,
				UserRoles:  userRoles,
				Site:       req.Site,
				Timeout:    time.Duration(req.Rec.TimeoutMs) * time.Millisecond,
				Provider:   provider,
			}
			if oldDoc != nil {
				execReq.OldDocument = oldDoc.ToMap()
			}

			result, execErr := runner.Execute(ctx, execReq)
			status := "success"
			errMsg := ""
			durationMs := 0
			if execErr != nil {
				status = "error"
				errMsg = execErr.Error()
			}
			if result != nil {
				durationMs = int(result.Duration.Milliseconds())
			}

			if services.ScriptStore != nil {
				if err := services.ScriptStore.LogExecution(req.Site, req.Rec, req.Doctype, doc.Name, req.Event, req.User, durationMs, status, errMsg); err != nil {
					slog.Error("async hook execution log write failed", "site", req.Site, "script", req.Rec.Name, "error", err)
				}
			}
		}()
	}
}

func acquireAsyncHookWork(ctx context.Context, admission interface {
	Acquire(context.Context) (func(), error)
}) (func(), error) {
	if admission == nil {
		return func() {}, nil
	}
	// *dbaccess.Admission is commonly passed through this narrow interface.
	// A nil pointer stored in an interface is not equal to nil and would panic
	// in Acquire during worker shutdown for test or partially assembled runtimes.
	if typed, ok := admission.(*dbaccess.Admission); ok && typed == nil {
		return func() {}, nil
	}
	for {
		release, err := admission.Acquire(ctx)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, dbaccess.ErrExhausted) {
			return nil, err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

type asyncHookQueueSink struct {
	mu             sync.Mutex
	queue          chan asyncHookQueueItem
	closed         bool
	maxQueuedBytes uint64
	maxEventBytes  uint64
	queuedBytes    uint64
	rejectedItems  uint64
}

type asyncHookQueueItem struct {
	request orm.AsyncHookRequest
	bytes   uint64
}

type AsyncHookQueueSnapshot struct {
	QueuedBytes    uint64
	MaxQueuedBytes uint64
	MaxEventBytes  uint64
	RejectedItems  uint64
	QueuedItems    int
	MaxQueuedItems int
}

func (q *asyncHookQueueSink) isClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}

func (q *asyncHookQueueSink) Enqueue(ctx context.Context, req orm.AsyncHookRequest) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return errAsyncHookQueueClosed
	}
	if q.maxEventBytes == 0 || q.maxQueuedBytes == 0 {
		return errors.New("async hook queue byte limits are not configured")
	}
	size, err := estimateHookPayloadBytes(reflect.ValueOf(req), q.maxEventBytes, 0)
	if err != nil || size > q.maxEventBytes {
		q.rejectedItems++
		return ErrAsyncHookPayloadSize
	}
	if size > q.maxQueuedBytes-q.queuedBytes {
		q.rejectedItems++
		return errAsyncHookQueueFull
	}
	select {
	case q.queue <- asyncHookQueueItem{request: req, bytes: size}:
		q.queuedBytes += size
		return nil
	default:
		return errAsyncHookQueueFull
	}
}

func (q *asyncHookQueueSink) releaseBytes(size uint64) {
	q.mu.Lock()
	if size >= q.queuedBytes {
		q.queuedBytes = 0
	} else {
		q.queuedBytes -= size
	}
	q.mu.Unlock()
}

func (q *asyncHookQueueSink) Snapshot() AsyncHookQueueSnapshot {
	q.mu.Lock()
	defer q.mu.Unlock()
	return AsyncHookQueueSnapshot{
		QueuedBytes: q.queuedBytes, MaxQueuedBytes: q.maxQueuedBytes,
		MaxEventBytes: q.maxEventBytes, RejectedItems: q.rejectedItems,
		QueuedItems: len(q.queue), MaxQueuedItems: cap(q.queue),
	}
}

// QueueSnapshot reports the async-hook queue's bounded count and byte usage.
func (w *AsyncHookWorker) QueueSnapshot() AsyncHookQueueSnapshot {
	if w == nil || w.sink == nil {
		return AsyncHookQueueSnapshot{}
	}
	return w.sink.Snapshot()
}

func estimateHookPayloadBytes(value reflect.Value, limit uint64, depth int) (uint64, error) {
	if depth > 64 {
		return limit, ErrAsyncHookPayloadSize
	}
	add := func(left, right uint64) (uint64, error) {
		if right > limit || left > limit-right {
			return limit, ErrAsyncHookPayloadSize
		}
		return left + right, nil
	}
	if !value.IsValid() {
		return 4, nil
	}
	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		if value.IsNil() {
			return 4, nil
		}
		return estimateHookPayloadBytes(value.Elem(), limit, depth+1)
	case reflect.String:
		length := uint64(value.Len())
		if limit < 2 || length > (limit-2)/6 {
			return limit, ErrAsyncHookPayloadSize
		}
		return 2 + length*6, nil // worst-case JSON escaping per input byte
	case reflect.Bool:
		if value.Bool() {
			return 5, nil
		}
		return 6, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return 32, nil
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			return 4, nil
		}
		if value.Type().Elem().Kind() == reflect.Uint8 {
			length := uint64(value.Len())
			encoded := (length+2)/3*4 + 2
			if encoded > limit {
				return limit + 1, ErrAsyncHookPayloadSize
			}
			return encoded, nil
		}
		total := uint64(2)
		for index := 0; index < value.Len(); index++ {
			if index > 0 {
				var err error
				total, err = add(total, 1)
				if err != nil {
					return limit, err
				}
			}
			itemSize, err := estimateHookPayloadBytes(value.Index(index), limit, depth+1)
			if err != nil {
				return limit + 1, err
			}
			total, err = add(total, itemSize)
			if err != nil {
				return limit + 1, err
			}
		}
		return total, nil
	case reflect.Map:
		if value.IsNil() {
			return 4, nil
		}
		total := uint64(2)
		iter := value.MapRange()
		for iter.Next() {
			key := iter.Key()
			if key.Kind() != reflect.String {
				return limit + 1, ErrAsyncHookPayloadSize
			}
			keySize, err := estimateHookPayloadBytes(key, limit, depth+1)
			if err != nil {
				return limit + 1, err
			}
			if total > 2 {
				total, err = add(total, 1)
				if err != nil {
					return limit + 1, err
				}
			}
			total, err = add(total, keySize+1)
			if err != nil {
				return limit + 1, err
			}
			itemSize, err := estimateHookPayloadBytes(iter.Value(), limit, depth+1)
			if err != nil {
				return limit + 1, err
			}
			total, err = add(total, itemSize)
			if err != nil {
				return limit + 1, err
			}
		}
		return total, nil
	case reflect.Struct:
		total := uint64(2)
		for index := 0; index < value.NumField(); index++ {
			field := value.Type().Field(index)
			if field.PkgPath != "" {
				continue
			}
			tag := strings.Split(field.Tag.Get("json"), ",")[0]
			if tag == "-" {
				continue
			}
			name := field.Name
			if tag != "" {
				name = tag
			}
			nameSize := uint64(2 + len(name)*6 + 1)
			fieldSize, err := estimateHookPayloadBytes(value.Field(index), limit, depth+1)
			if err != nil {
				return limit + 1, err
			}
			if total > 2 {
				total, err = add(total, 1)
				if err != nil {
					return limit + 1, err
				}
			}
			total, err = add(total, nameSize+fieldSize)
			if err != nil {
				return limit + 1, err
			}
		}
		return total, nil
	default:
		return limit, ErrAsyncHookPayloadSize
	}
}

func (q *asyncHookQueueSink) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		close(q.queue)
	}
}
