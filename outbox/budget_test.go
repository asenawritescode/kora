package outbox

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/db"
)

func TestOutboxBatchLimitBoundsPublisherAndDispatcher(t *testing.T) {
	for _, test := range []struct {
		input int
		want  int
		err   bool
	}{{input: -1, want: DefaultBatchSize}, {input: 0, want: DefaultBatchSize}, {input: 1, want: 1}, {input: MaxBatchSize, want: MaxBatchSize}, {input: MaxBatchSize + 1, err: true}} {
		got, err := boundedBatchLimit(test.input)
		if test.err {
			if err == nil {
				t.Errorf("boundedBatchLimit(%d) unexpectedly succeeded", test.input)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("boundedBatchLimit(%d) = (%d, %v), want (%d, nil)", test.input, got, err, test.want)
		}
	}

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock:", err)
	}
	defer database.Close()
	publisher := &Publisher{DB: database}
	if _, err := publisher.PublishDue(context.Background(), MaxBatchSize+1); err == nil {
		t.Fatal("publisher accepted a batch above the maximum")
	}
	dispatcher := NewDispatcher(database, kdb.Resolve("mysql"))
	if _, err := dispatcher.Run(context.Background(), MaxBatchSize+1); err == nil {
		t.Fatal("dispatcher accepted a batch above the maximum")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("invalid batch limits reached the database: %v", err)
	}
}
