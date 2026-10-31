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

// AnalyticsQueueAdmission bounds conservative queued-payload bytes across
// tenant wake-up channels and MultiBus listeners owned by one Engine process.
// Reservations use the configured maximum event size, not an estimate.
type AnalyticsQueueAdmission struct {
	mu                sync.Mutex
	maxBytes          uint64
	used              uint64
	rejected          uint64
	maxListeners      int
	listeners         int
	rejectedListeners uint64
}

type AnalyticsQueueAdmissionSnapshot struct {
	UsedBytes         uint64
	MaxBytes          uint64
	RejectedItems     uint64
	Listeners         int
	MaxListeners      int
	RejectedListeners uint64
}

func NewAnalyticsQueueAdmission(maxBytes uint64) (*AnalyticsQueueAdmission, error) {
	return NewAnalyticsQueueAdmissionWithLimits(maxBytes, DefaultAnalyticsProcessMaxListeners)
}

func NewAnalyticsQueueAdmissionWithLimits(maxBytes uint64, maxListeners int) (*AnalyticsQueueAdmission, error) {
	if maxBytes == 0 || maxBytes > MaxAnalyticsQueueAdmissionBytes {
		return nil, ErrAnalyticsQueueBudgetInvalid
	}
	if maxListeners <= 0 || maxListeners > MaxAnalyticsProcessListeners {
		return nil, ErrAnalyticsListenerBudgetInvalid
	}
	return &AnalyticsQueueAdmission{maxBytes: maxBytes, maxListeners: maxListeners}, nil
}

var ErrAnalyticsListenerBudgetInvalid = errors.New("analytics process listener limit must be between 1 and 65536")

// TryAcquireListener reserves one process-wide subscription slot.
func (a *AnalyticsQueueAdmission) TryAcquireListener() bool {
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
func (a *AnalyticsQueueAdmission) ReleaseListener() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.listeners > 0 {
		a.listeners--
	}
	a.mu.Unlock()
}

func (a *AnalyticsQueueAdmission) TryAcquire(bytes uint64) bool {
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

func (a *AnalyticsQueueAdmission) Release(bytes uint64) {
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

func (a *AnalyticsQueueAdmission) Snapshot() AnalyticsQueueAdmissionSnapshot {
	if a == nil {
		return AnalyticsQueueAdmissionSnapshot{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return AnalyticsQueueAdmissionSnapshot{
		UsedBytes: a.used, MaxBytes: a.maxBytes, RejectedItems: a.rejected,
		Listeners: a.listeners, MaxListeners: a.maxListeners, RejectedListeners: a.rejectedListeners,
	}
}
