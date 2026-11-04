package outbox

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
)

type captureString struct{ value *string }

func (c captureString) Match(value driver.Value) bool {
	text, ok := value.(string)
	if !ok {
		return false
	}
	*c.value = text
	return true
}

func TestSQLWriterRejectsOversizedEventBeforeSequenceAllocation(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	mock.ExpectRollback()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	event := contract.EventEnvelope{
		ID: "large-event", Type: "record.updated", Version: 1, Source: "kora.kernel", Site: "tenant",
		AggregateType: "Record", AggregateID: "REC-1", OccurredAt: time.Now().UTC(),
		Data: json.RawMessage(`{"value":"` + strings.Repeat("x", MaxEventPayloadBytes) + `"}`),
	}
	err = NewSQLWriter(kdb.Resolve("mysql")).Append(context.Background(), tx, event)
	if !errors.Is(err, ErrEventPayloadTooLarge) {
		t.Fatalf("Append error = %v, want ErrEventPayloadTooLarge", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal("rollback rejected event transaction:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("oversized event touched sequence/outbox SQL:", err)
	}
}

func TestSQLWriterReservesSequenceBytesBeforeAllocation(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	mock.ExpectRollback()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	event := contract.EventEnvelope{
		ID: "sequence-boundary", Type: "record.updated", Version: 1, Source: "kora.kernel", Site: "tenant",
		AggregateType: "Record", AggregateID: "REC-1", OccurredAt: time.Now().UTC(), Data: json.RawMessage(`""`),
	}
	withoutSequence, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	dataBytes := MaxEventPayloadBytes - 1 - (len(withoutSequence) - len(event.Data))
	event.Data = json.RawMessage(`"` + strings.Repeat("x", dataBytes-2) + `"`)
	withoutSequence, err = json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if len(withoutSequence) != MaxEventPayloadBytes-1 {
		t.Fatalf("test payload size = %d, want %d", len(withoutSequence), MaxEventPayloadBytes-1)
	}
	err = NewSQLWriter(kdb.Resolve("mysql")).Append(context.Background(), tx, event)
	if !errors.Is(err, ErrEventPayloadTooLarge) {
		t.Fatalf("Append error = %v, want ErrEventPayloadTooLarge", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal("rollback rejected event transaction:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("sequence boundary rejection touched sequence/outbox SQL:", err)
	}
}

func TestSQLWriterAppendCompletesEnvelopeDefaultsBeforeValidation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO _kora_tenant_event_sequence`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT last_sequence FROM _kora_tenant_event_sequence WHERE id = 1 FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"last_sequence"}).AddRow(0))
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(last_sequence\), 0\) FROM _kora_event_sequence`).
		WillReturnRows(sqlmock.NewRows([]string{"last_sequence"}).AddRow(6))
	mock.ExpectExec(`UPDATE _kora_tenant_event_sequence SET last_sequence = \? WHERE id = 1`).WithArgs(int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	var outboxPayload string
	mock.ExpectExec(`INSERT INTO _kora_outbox`).
		WithArgs(sqlmock.AnyArg(), "live-test", int64(7), "record.created", 1, "Record", "REC-1", captureString{&outboxPayload}, "pending", 0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	err = NewSQLWriter(kdb.Resolve("mysql")).Append(context.Background(), tx, contract.EventEnvelope{
		Type: "record.created", Source: "kora.kernel", Site: "live-test",
		AggregateType: "Record", AggregateID: "REC-1",
		Data: contract.MustEncodeData(map[string]any{"name": "REC-1"}),
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got contract.EventEnvelope
	if err := json.Unmarshal([]byte(outboxPayload), &got); err != nil {
		t.Fatalf("decode inserted event payload: %v", err)
	}
	if got.Sequence != "7" {
		t.Fatalf("event sequence = %q, want 7", got.Sequence)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
