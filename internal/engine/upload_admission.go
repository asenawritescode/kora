package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

var (
	ErrUploadCapacity          = errors.New("upload process capacity exhausted")
	ErrUploadReservationTooBig = errors.New("upload reservation exceeds process byte budget")
)

const (
	DefaultUploadMaxActive   = 4
	MaxUploadMaxActive       = 256
	DefaultUploadActiveBytes = uint64(512 << 20)
	MaxUploadActiveBytes     = uint64(4 << 30)
)

// UploadAdmission reserves a conservative process-wide memory allowance for
// an upload's multipart body, decoded bytes, and image-processing copy.
type UploadAdmission struct {
	mu          sync.Mutex
	maxActive   int
	maxBytes    uint64
	active      int
	activeBytes uint64
	rejected    atomic.Uint64
}

type UploadAdmissionSnapshot struct {
	Active      int
	Capacity    int
	ActiveBytes uint64
	MaxBytes    uint64
	Rejected    uint64
}

func NewUploadAdmission(maxActive int, maxBytes uint64) (*UploadAdmission, error) {
	if maxActive <= 0 || maxActive > MaxUploadMaxActive {
		return nil, errors.New("upload active limit must be between 1 and 256")
	}
	if maxBytes == 0 || maxBytes > MaxUploadActiveBytes {
		return nil, errors.New("upload byte budget must be between 1 byte and 4 GiB")
	}
	return &UploadAdmission{maxActive: maxActive, maxBytes: maxBytes}, nil
}

func (a *UploadAdmission) Acquire(ctx context.Context, bytes uint64) (func(), error) {
	if a == nil {
		return nil, errors.New("upload admission is unavailable")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if bytes == 0 || bytes > a.maxBytes {
		a.rejected.Add(1)
		return nil, ErrUploadReservationTooBig
	}
	a.mu.Lock()
	if a.active >= a.maxActive || a.activeBytes > a.maxBytes-bytes {
		a.mu.Unlock()
		a.rejected.Add(1)
		return nil, ErrUploadCapacity
	}
	a.active++
	a.activeBytes += bytes
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.active--
			a.activeBytes -= bytes
			a.mu.Unlock()
		})
	}, nil
}

func (a *UploadAdmission) Snapshot() UploadAdmissionSnapshot {
	if a == nil {
		return UploadAdmissionSnapshot{}
	}
	a.mu.Lock()
	snapshot := UploadAdmissionSnapshot{Active: a.active, Capacity: a.maxActive, ActiveBytes: a.activeBytes, MaxBytes: a.maxBytes}
	a.mu.Unlock()
	snapshot.Rejected = a.rejected.Load()
	return snapshot
}
