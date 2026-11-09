package site

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestOnboardingStoreFilePersistence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KORA_CONFIG_DIR", dir)
	store := NewOnboardingStore(nil)

	job := OnboardingJob{
		ID:               "job-1",
		Site:             "acme.example.com",
		Resource:         "acme.example.com",
		State:            OnboardingRequested,
		OperationID:      "op-1",
		IdempotencyKey:   "idem-1",
		InputFingerprint: "fingerprint-1",
		Attempt:          1,
		CreatedAt:        time.Now().UTC(),
	}
	cp := OnboardingCheckpoint{JobID: job.ID, Stage: "requested", Completed: true, RecordedAt: time.Now().UTC()}
	if err := store.UpsertJob(job); err != nil {
		t.Fatalf("upsert job: %v", err)
	}
	if err := store.UpsertCheckpoint(cp); err != nil {
		t.Fatalf("upsert checkpoint: %v", err)
	}

	loaded, err := store.LoadJobByID(job.ID)
	if err != nil {
		t.Fatalf("load job by id: %v", err)
	}
	if loaded.ID != job.ID || loaded.Site != job.Site {
		t.Fatalf("loaded job mismatch: %+v", loaded)
	}

	byKey, err := store.LoadJob(job.Site, job.Resource, job.OperationID, job.IdempotencyKey, job.InputFingerprint)
	if err != nil {
		t.Fatalf("load job by key: %v", err)
	}
	if byKey.ID != job.ID {
		t.Fatalf("load job by key mismatch: %+v", byKey)
	}

	jobs, err := store.ListJobs()
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}

	if _, err := os.Stat(filepath.Join(dir, "_kora_provisioning_jobs.json")); err != nil {
		t.Fatalf("expected backing file: %v", err)
	}
}

func TestWriteFileAtomicallyPreservesExistingStateWhenPublishFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "existing")
	if err := os.WriteFile(marker, []byte("old state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomically(path, []byte("new state"), 0o600); err == nil {
		t.Fatal("expected publishing over an existing directory to fail")
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old state" {
		t.Fatalf("existing state changed after failed publish: %q", got)
	}
}

func TestOnboardingStorePostgresBootstrapUsesCompatibleDDL(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS _kora_provisioning_job[\\s\\S]*TIMESTAMP[\\s\\S]*CONSTRAINT uq_kora_provisioning_dedupe UNIQUE").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("ALTER TABLE _kora_provisioning_job ADD COLUMN dedupe_hash").WillReturnError(errors.New("column dedupe_hash already exists"))
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS _kora_provisioning_checkpoint[\\s\\S]*BOOLEAN NOT NULL DEFAULT FALSE[\\s\\S]*TIMESTAMP").WillReturnResult(sqlmock.NewResult(0, 0))
	store := NewOnboardingStore(database, "postgres")
	if err := store.Bootstrap(); err != nil {
		t.Fatalf("PostgreSQL bootstrap: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
