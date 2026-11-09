package natsprovider

import (
	"errors"
	"fmt"
	"time"
)

// RetryableError asks Consumer.Run to requeue a delivery after Delay without
// consuming the configured MaxDeliver budget. Use it for transient local
// capacity rejection, where the durable work item must remain discoverable.
type RetryableError struct {
	cause error
	delay time.Duration
}

func (e *RetryableError) Error() string {
	if e == nil || e.cause == nil {
		return "natsprovider: retryable delivery"
	}
	return fmt.Sprintf("natsprovider: retryable delivery: %v", e.cause)
}

func (e *RetryableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// RetryAfter wraps err as a capacity/transient retry. Non-positive delays use
// one second to avoid a hot redelivery loop.
func RetryAfter(err error, delay time.Duration) error {
	if delay <= 0 {
		delay = time.Second
	}
	return &RetryableError{cause: err, delay: delay}
}

func retryableDelay(err error) (time.Duration, bool) {
	var retryable *RetryableError
	if !errors.As(err, &retryable) || retryable == nil {
		return 0, false
	}
	if retryable.delay <= 0 {
		return time.Second, true
	}
	return retryable.delay, true
}
