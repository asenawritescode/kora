package site

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestOnboardingJSONShape(t *testing.T) {
	job := OnboardingJob{
		ID:                "job-1",
		Site:              "acme.example.com",
		Resource:          "deployment-1",
		State:             OnboardingRequested,
		OperationID:       "op-1",
		IdempotencyKey:    "idem-1",
		InputFingerprint:  "fingerprint-1",
		ProviderRequestID: "provider-req-1",
		OutputID:          "output-1",
		Attempt:           2,
		LeaseOwner:        "worker-1",
		LeaseUntil:        time.Date(2026, 8, 13, 15, 0, 0, 0, time.UTC),
		RetryAt:           time.Date(2026, 8, 13, 15, 5, 0, 0, time.UTC),
		CreatedAt:         time.Date(2026, 8, 13, 14, 55, 0, 0, time.UTC),
	}
	b, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	for _, key := range []string{"id", "site", "resource", "state", "operation_id", "idempotency_key", "input_fingerprint", "provider_request_id", "output_id", "attempt", "lease_owner", "lease_until", "retry_at", "created_at"} {
		if !strings.Contains(string(b), `"`+key+`"`) {
			t.Fatalf("OnboardingJob missing key %q: %s", key, b)
		}
	}

	cp := OnboardingCheckpoint{
		JobID:      job.ID,
		Stage:      "provisioning_nats",
		Completed:  true,
		RecordedAt: job.CreatedAt,
	}
	b, err = json.Marshal(cp)
	if err != nil {
		t.Fatalf("marshal checkpoint: %v", err)
	}
	for _, key := range []string{"job_id", "stage", "completed", "recorded_at"} {
		if !strings.Contains(string(b), `"`+key+`"`) {
			t.Fatalf("OnboardingCheckpoint missing key %q: %s", key, b)
		}
	}
}

func TestOnboardingResumableDuplicateAndTransitions(t *testing.T) {
	previous := OnboardingJob{
		ID:               "job-1",
		Site:             "acme.example.com",
		Resource:         "deployment-1",
		State:            OnboardingProvisioning,
		OperationID:      "op-1",
		IdempotencyKey:   "idem-1",
		InputFingerprint: "fingerprint-1",
	}
	next := previous
	if !OnboardingIsResumable(previous) {
		t.Fatal("onboarding job should be resumable")
	}
	if !OnboardingIsDuplicate(previous, next) {
		t.Fatal("matching onboarding job should be treated as duplicate restart")
	}
	next.IdempotencyKey = "idem-2"
	if OnboardingIsDuplicate(previous, next) {
		t.Fatal("different idempotency key should not be duplicate")
	}
	if !OnboardingCanTransition(OnboardingRequested, OnboardingValidating) {
		t.Fatal("requested should transition to validating")
	}
	if OnboardingCanTransition(OnboardingActive, OnboardingRequested) {
		t.Fatal("active should not rewind")
	}

	cp := OnboardingCheckpoint{JobID: "job-1", Stage: "provisioning_nats", Completed: true, RecordedAt: time.Now().UTC()}
	if !OnboardingCheckpointCanResume(cp) {
		t.Fatal("completed checkpoint should resume")
	}
	if OnboardingCheckpointCanResume(OnboardingCheckpoint{JobID: "job-1", Stage: "", Completed: true}) {
		t.Fatal("checkpoint without stage should not resume")
	}
}
