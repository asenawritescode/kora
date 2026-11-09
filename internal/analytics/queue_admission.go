package analytics

import (
	"errors"
	"sync"
)

const MaxAnalyticsQueueAdmissionBytes uint64 = 4 << 30
const DefaultAnalyticsQueueAdmissionBytes uint64 = 512 << 20
const DefaultAnalyticsProcessMaxListeners = 4096
const MaxAnalyticsProcessListeners = 65536

var ErrAnalyticsQueueBudgetInvalid = errors.New("analytics process queue admission budget must be between 1 byte and 4 GiB")

// QueueAdmission bounds conservative queued-payload bytes across
// tenant wake-up channels and MultiBus listeners owned by one Engine process.
// Reservations use the configured maximum event size, not an estimate.
type QueueAdmission struct {
	mu                sync.Mutex
	maxBytes          uint64
	used              uint64
	rejected          uint64
	maxListeners      int
	listeners         int
	rejectedListeners uint64
}

// QueueSnapshot is a point-in-time view of a QueueAdmission budget.
type QueueSnapshot struct {
	UsedBytes         uint64
	MaxBytes          uint64
	RejectedItems     uint64
	Listeners         int
	MaxListeners      int
	RejectedListeners uint64
}

// NewAnalyticsQueueAdmission returns a byte budget for queued analytics events
// with the default listener limit.
func NewAnalyticsQueueAdmission(maxBytes uint64) (*QueueAdmission, error) {
	return NewAnalyticsQueueAdmissionWithLimits(maxBytes, DefaultAnalyticsProcessMaxListeners)
}

// NewAnalyticsQueueAdmissionWithLimits returns a budget of maxBytes queued bytes
// and maxListeners process listeners, or an error if either is out of range.
func NewAnalyticsQueueAdmissionWithLimits(maxBytes uint64, maxListeners int) (*QueueAdmission, error) {
	if maxBytes == 0 || maxBytes > MaxAnalyticsQueueAdmissionBytes {
		return nil, ErrAnalyticsQueueBudgetInvalid
	}
	if maxListeners <= 0 || maxListeners > MaxAnalyticsProcessListeners {
		return nil, ErrAnalyticsListenerBudgetInvalid
	}
	return &QueueAdmission{maxBytes: maxBytes, maxListeners: maxListeners}, nil
}

var ErrAnalyticsListenerBudgetInvalid = errors.New("analytics process listener limit must be between 1 and 65536")

// TryAcquireListener reserves one process-wide subscription slot.
func (a *QueueAdmission) TryAcquireListener() bool {
	if a == nil {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listeners >= a.maxListeners {
		a.rejectedListeners++
		return false
	}
	a.listeners++
	return true
}

// ReleaseListener releases one process-wide subscription slot.
func (a *QueueAdmission) ReleaseListener() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.listeners > 0 {
		a.listeners--
	}
	a.mu.Unlock()
}

// TryAcquire reserves bytes from the budget and reports whether they fit. A
// refusal is counted in the snapshot.
func (a *QueueAdmission) TryAcquire(bytes uint64) bool {
	if a == nil || bytes == 0 {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.used > a.maxBytes || bytes > a.maxBytes-a.used {
		a.rejected++
		return false
	}
	a.used += bytes
	return true
}

// Release returns bytes reserved by TryAcquire to the budget.
func (a *QueueAdmission) Release(bytes uint64) {
	if a == nil || bytes == 0 {
		return
	}
	a.mu.Lock()
	if bytes >= a.used {
		a.used = 0
	} else {
		a.used -= bytes
	}
	a.mu.Unlock()
}

// Snapshot returns the current usage, limits and rejection counts.
func (a *QueueAdmission) Snapshot() QueueSnapshot {
	if a == nil {
		return QueueSnapshot{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return QueueSnapshot{
		UsedBytes: a.used, MaxBytes: a.maxBytes, RejectedItems: a.rejected,
		Listeners: a.listeners, MaxListeners: a.maxListeners, RejectedListeners: a.rejectedListeners,
	}
}
