package site

import "time"

// OnboardingState tracks durable self-hosted onboarding progress.
type OnboardingState string

const (
	OnboardingRequested         OnboardingState = "requested"
	OnboardingValidating        OnboardingState = "validating"
	OnboardingAllocating        OnboardingState = "allocating"
	OnboardingProvisioning      OnboardingState = "provisioning"
	OnboardingValidatingRuntime OnboardingState = "validating_runtime"
	OnboardingActive            OnboardingState = "active"
	OnboardingFailed            OnboardingState = "failed"
)

// OnboardingJob records a resumable, idempotent onboarding operation.
type OnboardingJob struct {
	ID                string          `json:"id"`
	Site              string          `json:"site"`
	Resource          string          `json:"resource"`
	State             OnboardingState `json:"state"`
	OperationID       string          `json:"operation_id"`
	IdempotencyKey    string          `json:"idempotency_key"`
	InputFingerprint  string          `json:"input_fingerprint"`
	ProviderRequestID string          `json:"provider_request_id,omitempty"`
	OutputID          string          `json:"output_id,omitempty"`
	Attempt           int             `json:"attempt"`
	LeaseOwner        string          `json:"lease_owner,omitempty"`
	LeaseUntil        time.Time       `json:"lease_until,omitempty"`
	RetryAt           time.Time       `json:"retry_at,omitempty"`
	LastError         string          `json:"last_error,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at,omitempty"`
}

// OnboardingCheckpoint captures the last completed stage so a restart can resume.
type OnboardingCheckpoint struct {
	JobID      string    `json:"job_id"`
	Stage      string    `json:"stage"`
	Completed  bool      `json:"completed"`
	RecordedAt time.Time `json:"recorded_at"`
}

func OnboardingCanTransition(from, to OnboardingState) bool {
	switch from {
	case OnboardingRequested:
		return to == OnboardingValidating || to == OnboardingFailed
	case OnboardingValidating:
		return to == OnboardingAllocating || to == OnboardingFailed
	case OnboardingAllocating:
		return to == OnboardingProvisioning || to == OnboardingFailed
	case OnboardingProvisioning:
		return to == OnboardingValidatingRuntime || to == OnboardingFailed
	case OnboardingValidatingRuntime:
		return to == OnboardingActive || to == OnboardingFailed
	case OnboardingActive:
		return false
	case OnboardingFailed:
		return to == OnboardingRequested
	default:
		return false
	}
}

func OnboardingIsResumable(job OnboardingJob) bool {
	return job.OperationID != "" && job.IdempotencyKey != "" && job.InputFingerprint != "" && job.State != OnboardingActive
}

func OnboardingIsDuplicate(previous, next OnboardingJob) bool {
	return previous.Site == next.Site &&
		previous.Resource == next.Resource &&
		previous.OperationID == next.OperationID &&
		previous.IdempotencyKey == next.IdempotencyKey &&
		previous.InputFingerprint == next.InputFingerprint
}

func OnboardingCheckpointCanResume(cp OnboardingCheckpoint) bool {
	return cp.JobID != "" && cp.Stage != "" && cp.Completed
}
