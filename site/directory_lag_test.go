package site

import (
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestSQLSiteRegistryConsumerLagReportsPendingRevisionAndAge(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	changeAt := time.Now().UTC().Add(-7 * time.Second).Truncate(time.Microsecond)
	updatedAt := changeAt.Add(-time.Second)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT c.directory_revision, m.directory_revision, MIN(o.created_at), c.updated_at
		FROM _kora_site_directory_consumers c
		JOIN _kora_site_directory_meta m ON m.id = 1
		LEFT JOIN _kora_site_directory_outbox o ON o.directory_revision > c.directory_revision
		WHERE c.consumer_id = ?
		GROUP BY c.directory_revision, m.directory_revision, c.updated_at`)).
		WithArgs("engine:test").
		WillReturnRows(sqlmock.NewRows([]string{"cursor", "latest", "oldest", "updated"}).AddRow(8, 11, changeAt, updatedAt))

	status, err := NewSQLSiteRegistry(database, "mysql").ConsumerLag("engine:test")
	if err != nil {
		t.Fatal(err)
	}
	if status.ConsumerRevision != 8 || status.LatestRevision != 11 || status.RevisionLag != 3 {
		t.Fatalf("revision status = %+v", status)
	}
	if status.OldestChangeAge < 6*time.Second || status.OldestChangeAge > 8*time.Second {
		t.Fatalf("oldest change age = %v, want about 7s", status.OldestChangeAge)
	}
	if status.ObservedAt.IsZero() {
		t.Fatal("observation time was not populated")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLSiteRegistryConsumerLagUsesCursorAgeWhenOutboxWasCompacted(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	updatedAt := time.Now().UTC().Add(-12 * time.Second)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT c.directory_revision, m.directory_revision, MIN(o.created_at), c.updated_at
		FROM _kora_site_directory_consumers c
		JOIN _kora_site_directory_meta m ON m.id = 1
		LEFT JOIN _kora_site_directory_outbox o ON o.directory_revision > c.directory_revision
		WHERE c.consumer_id = ?
		GROUP BY c.directory_revision, m.directory_revision, c.updated_at`)).
		WithArgs("engine:compacted").
		WillReturnRows(sqlmock.NewRows([]string{"cursor", "latest", "oldest", "updated"}).AddRow(4, 7, nil, updatedAt))

	status, err := NewSQLSiteRegistry(database, "mysql").ConsumerLag("engine:compacted")
	if err != nil {
		t.Fatal(err)
	}
	if status.RevisionLag != 3 || status.OldestChangeAge < 11*time.Second {
		t.Fatalf("compacted-history status = %+v", status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLSiteRegistryConsumerLagIsZeroWhenCurrent(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT c.directory_revision, m.directory_revision, MIN(o.created_at), c.updated_at
		FROM _kora_site_directory_consumers c
		JOIN _kora_site_directory_meta m ON m.id = 1
		LEFT JOIN _kora_site_directory_outbox o ON o.directory_revision > c.directory_revision
		WHERE c.consumer_id = ?
		GROUP BY c.directory_revision, m.directory_revision, c.updated_at`)).
		WithArgs("engine:current").
		WillReturnRows(sqlmock.NewRows([]string{"cursor", "latest", "oldest", "updated"}).AddRow(11, 11, nil, time.Now()))

	status, err := NewSQLSiteRegistry(database, "mysql").ConsumerLag("engine:current")
	if err != nil {
		t.Fatal(err)
	}
	if status.RevisionLag != 0 || status.OldestChangeAge != 0 {
		t.Fatalf("current status = %+v", status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
