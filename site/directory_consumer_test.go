package site

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func expectConsumerBatch(mock sqlmock.Sqlmock, consumerID, ownerID string, cursor uint64, changes ...*sqlmock.Rows) {
	mock.ExpectExec(regexp.QuoteMeta(`INSERT IGNORE INTO _kora_site_directory_consumers (consumer_id, directory_revision) VALUES (?, ?)`)).
		WithArgs(consumerID, uint64(cursor)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision FROM _kora_site_directory_consumers WHERE consumer_id = ?`)).
		WithArgs(consumerID).WillReturnRows(sqlmock.NewRows([]string{"directory_revision"}).AddRow(cursor))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_directory_consumers SET lease_owner = ?, lease_expires_at = TIMESTAMPADD(SECOND, ?, CURRENT_TIMESTAMP(6)), updated_at = CURRENT_TIMESTAMP(6) WHERE consumer_id = ? AND (lease_owner = ? OR lease_owner IS NULL OR lease_expires_at IS NULL OR lease_expires_at <= CURRENT_TIMESTAMP(6))`)).
		WithArgs(ownerID, int64(90), consumerID, ownerID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision, id, payload, created_at FROM _kora_site_directory_outbox WHERE directory_revision > ? ORDER BY directory_revision LIMIT ?`)).
		WithArgs(cursor, 100).WillReturnRows(changes[0])
}

func TestDirectoryConsumerAppliesBeforeAdvancingCursor(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rows := sqlmock.NewRows([]string{"directory_revision", "id", "payload", "created_at"}).
		AddRow(1, "change-1", `{"version":1,"site_id":"site-1","aliases":["one.example"],"status":"active","config_revision":1}`, time.Now().UTC())
	expectConsumerBatch(mock, "engine:test", "owner:test", 0, rows)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_directory_consumers SET directory_revision = ?, updated_at = CURRENT_TIMESTAMP WHERE consumer_id = ? AND lease_owner = ? AND lease_expires_at > CURRENT_TIMESTAMP AND directory_revision = ?`)).
		WithArgs(uint64(1), "engine:test", "owner:test", uint64(0)).WillReturnResult(sqlmock.NewResult(0, 1))
	consumer := &DirectoryConsumer{Registry: NewSQLSiteRegistry(database, "mysql"), ID: "engine:test", OwnerID: "owner:test", Apply: func(_ context.Context, change SiteDirectoryChange) error {
		if change.Cursor != 1 {
			t.Fatalf("applied cursor = %d, want 1", change.Cursor)
		}
		return nil
	}}
	count, err := consumer.ProcessBatch(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("ProcessBatch = %d, %v", count, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryConsumerDoesNotAdvanceOnApplyFailure(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rows := sqlmock.NewRows([]string{"directory_revision", "id", "payload", "created_at"}).
		AddRow(1, "change-1", `{"version":1,"site_id":"site-1","aliases":["one.example"],"status":"active","config_revision":1}`, time.Now().UTC())
	expectConsumerBatch(mock, "engine:test", "owner:test", 0, rows)
	consumer := &DirectoryConsumer{Registry: NewSQLSiteRegistry(database, "mysql"), ID: "engine:test", OwnerID: "owner:test", Apply: func(context.Context, SiteDirectoryChange) error {
		return errors.New("runtime unavailable")
	}}
	count, err := consumer.ProcessBatch(context.Background())
	if err == nil || count != 0 {
		t.Fatalf("ProcessBatch = %d, %v; expected apply failure without progress", count, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("cursor should not advance after apply failure:", err)
	}
}

func TestDirectoryConsumerRetriesAfterApplyOutageWithoutSkippingChange(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	payload := `{"version":1,"site_id":"site-1","aliases":["one.example"],"status":"active","config_revision":1}`
	firstBatch := sqlmock.NewRows([]string{"directory_revision", "id", "payload", "created_at"}).
		AddRow(1, "change-1", payload, time.Now().UTC())
	expectConsumerBatch(mock, "engine:retry", "owner:retry", 0, firstBatch)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM _kora_site_directory_consumers WHERE consumer_id = ? AND lease_owner = ? AND lease_expires_at > CURRENT_TIMESTAMP`)).
		WithArgs("engine:retry", "owner:retry").WillReturnRows(sqlmock.NewRows([]string{"valid"}).AddRow(1))
	retryBatch := sqlmock.NewRows([]string{"directory_revision", "id", "payload", "created_at"}).
		AddRow(1, "change-1", payload, time.Now().UTC())
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision, id, payload, created_at FROM _kora_site_directory_outbox WHERE directory_revision > ? ORDER BY directory_revision LIMIT ?`)).
		WithArgs(uint64(0), 100).WillReturnRows(retryBatch)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_directory_consumers SET directory_revision = ?, updated_at = CURRENT_TIMESTAMP WHERE consumer_id = ? AND lease_owner = ? AND lease_expires_at > CURRENT_TIMESTAMP AND directory_revision = ?`)).
		WithArgs(uint64(1), "engine:retry", "owner:retry", uint64(0)).WillReturnResult(sqlmock.NewResult(0, 1))
	applyCalls := 0
	consumer := &DirectoryConsumer{Registry: NewSQLSiteRegistry(database, "mysql"), ID: "engine:retry", OwnerID: "owner:retry", Apply: func(context.Context, SiteDirectoryChange) error {
		applyCalls++
		if applyCalls == 1 {
			return errors.New("temporary runtime outage")
		}
		return nil
	}}
	if count, err := consumer.ProcessBatch(context.Background()); err == nil || count != 0 || consumer.cursor != 0 {
		t.Fatalf("first attempt = %d, %v, cursor=%d; want failed apply and unchanged cursor", count, err, consumer.cursor)
	}
	if count, err := consumer.ProcessBatch(context.Background()); err != nil || count != 1 || consumer.cursor != 1 {
		t.Fatalf("retry = %d, %v, cursor=%d; want successful replay", count, err, consumer.cursor)
	}
	if applyCalls != 2 {
		t.Fatalf("apply called %d times, want 2 attempts for one durable change", applyCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("retry did not replay the uncommitted change:", err)
	}
}

func TestDirectoryConsumerRestartDoesNotReapplyDurablyAppliedChange(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT IGNORE INTO _kora_site_directory_consumers (consumer_id, directory_revision) VALUES (?, ?)`)).
		WithArgs("engine:restart", uint64(0)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision FROM _kora_site_directory_consumers WHERE consumer_id = ?`)).
		WithArgs("engine:restart").WillReturnRows(sqlmock.NewRows([]string{"directory_revision"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_directory_consumers SET lease_owner = ?, lease_expires_at = TIMESTAMPADD(SECOND, ?, CURRENT_TIMESTAMP(6)), updated_at = CURRENT_TIMESTAMP(6) WHERE consumer_id = ? AND (lease_owner = ? OR lease_owner IS NULL OR lease_expires_at IS NULL OR lease_expires_at <= CURRENT_TIMESTAMP(6))`)).
		WithArgs("owner:restart", int64(90), "engine:restart", "owner:restart").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision, id, payload, created_at FROM _kora_site_directory_outbox WHERE directory_revision > ? ORDER BY directory_revision LIMIT ?`)).
		WithArgs(uint64(1), 100).WillReturnRows(sqlmock.NewRows([]string{"directory_revision", "id", "payload", "created_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision FROM _kora_site_directory_meta WHERE id = 1`)).
		WillReturnRows(sqlmock.NewRows([]string{"directory_revision"}).AddRow(1))
	applyCalls := 0
	consumer := &DirectoryConsumer{Registry: NewSQLSiteRegistry(database, "mysql"), ID: "engine:restart", OwnerID: "owner:restart", Apply: func(context.Context, SiteDirectoryChange) error {
		applyCalls++
		return nil
	}}
	if count, err := consumer.ProcessBatch(context.Background()); err != nil || count != 0 {
		t.Fatalf("restarted consumer = %d, %v; want no duplicate applications", count, err)
	}
	if applyCalls != 0 || consumer.cursor != 1 {
		t.Fatalf("restart applied %d changes at cursor %d; want cursor 1 and no duplicate", applyCalls, consumer.cursor)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryConsumerResyncsAfterCompactedHistoryGap(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT IGNORE INTO _kora_site_directory_consumers (consumer_id, directory_revision) VALUES (?, ?)`)).
		WithArgs("engine:test", uint64(0)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision FROM _kora_site_directory_consumers WHERE consumer_id = ?`)).
		WithArgs("engine:test").WillReturnRows(sqlmock.NewRows([]string{"directory_revision"}).AddRow(0))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_directory_consumers SET lease_owner = ?, lease_expires_at = TIMESTAMPADD(SECOND, ?, CURRENT_TIMESTAMP(6)), updated_at = CURRENT_TIMESTAMP(6) WHERE consumer_id = ? AND (lease_owner = ? OR lease_owner IS NULL OR lease_expires_at IS NULL OR lease_expires_at <= CURRENT_TIMESTAMP(6))`)).
		WithArgs("owner:test", int64(90), "engine:test", "owner:test").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision, id, payload, created_at FROM _kora_site_directory_outbox WHERE directory_revision > ? ORDER BY directory_revision LIMIT ?`)).
		WithArgs(uint64(0), 100).WillReturnRows(sqlmock.NewRows([]string{"directory_revision", "id", "payload", "created_at"}).
		AddRow(4, "change-4", `{"version":1,"site_id":"site-1","aliases":["one.example"],"status":"active","config_revision":4}`, time.Now().UTC()))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision FROM _kora_site_directory_meta WHERE id = 1`)).
		WillReturnRows(sqlmock.NewRows([]string{"directory_revision"}).AddRow(4))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_directory_consumers SET directory_revision = ?, updated_at = CURRENT_TIMESTAMP WHERE consumer_id = ? AND lease_owner = ? AND lease_expires_at > CURRENT_TIMESTAMP AND directory_revision = ?`)).
		WithArgs(uint64(4), "engine:test", "owner:test", uint64(0)).WillReturnResult(sqlmock.NewResult(0, 1))
	resynced := uint64(0)
	consumer := &DirectoryConsumer{
		Registry: NewSQLSiteRegistry(database, "mysql"), ID: "engine:test", OwnerID: "owner:test",
		Apply:  func(context.Context, SiteDirectoryChange) error { return nil },
		Resync: func(_ context.Context, revision uint64) error { resynced = revision; return nil },
	}
	count, err := consumer.ProcessBatch(context.Background())
	if err != nil || count != 0 || resynced != 4 || consumer.cursor != 4 {
		t.Fatalf("ProcessBatch = %d, %v; resynced=%d cursor=%d", count, err, resynced, consumer.cursor)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryConsumerStopsAtGapWhenSnapshotResyncFails(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rows := sqlmock.NewRows([]string{"directory_revision", "id", "payload", "created_at"}).
		AddRow(3, "change-3", `{"version":1,"site_id":"site-1","aliases":["one.example"],"status":"active","config_revision":3}`, time.Now().UTC())
	expectConsumerBatch(mock, "engine:test", "owner:test", 0, rows)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision FROM _kora_site_directory_meta WHERE id = 1`)).
		WillReturnRows(sqlmock.NewRows([]string{"directory_revision"}).AddRow(3))
	consumer := &DirectoryConsumer{
		Registry: NewSQLSiteRegistry(database, "mysql"), ID: "engine:test", OwnerID: "owner:test",
		Apply:  func(context.Context, SiteDirectoryChange) error { return nil },
		Resync: func(context.Context, uint64) error { return errors.New("snapshot unavailable") },
	}
	count, err := consumer.ProcessBatch(context.Background())
	if err == nil || count != 0 || consumer.cursor != 0 {
		t.Fatalf("ProcessBatch = %d, %v; expected retained cursor 0 after failed resync", count, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
