package contract

import "time"

// OfflineStore is the minimal persistence surface for the reference sync
// service. Implementations can back it with SQL, an embedded cache, or a test
// double.
type OfflineStore interface {
	SaveOperation(OfflineOperation) error
	SaveConflict(OfflineConflict) error
	SaveTombstone(OfflineTombstone) error
	AdvanceCursor(SyncCursor) error
}

// OfflineReceipt is the acknowledgement returned by the central intake.
//
//contract:wire
type OfflineReceipt struct {
	OperationID string                 `json:"operation_id"`
	Status      OfflineOperationStatus `json:"status"`
	Conflict    *OfflineConflict       `json:"conflict,omitempty"`
	ReceivedAt  time.Time              `json:"received_at"`
	NextCursor  SyncCursor             `json:"next_cursor"`
}

// OfflineSyncService coordinates local apply, intake, and acknowledgement.
type OfflineSyncService struct {
	Store OfflineStore
	Clock func() time.Time
}

// OfflinePullPage is a repeatable page of operations and the cursor that
// should be used to continue from the last returned item.
//
//contract:wire
type OfflinePullPage struct {
	Operations []OfflineOperation `json:"operations"`
	Cursor     SyncCursor         `json:"cursor"`
	NextCursor SyncCursor         `json:"next_cursor"`
}

// ApplyLocal records a device/branch operation locally and preserves audit
// visibility before the batch is sent upstream.
func (s OfflineSyncService) ApplyLocal(op OfflineOperation) error {
	if s.Store != nil {
		if err := s.Store.SaveOperation(op); err != nil {
			return err
		}
	}
	return nil
}

// Intake applies a batch at the central site. Accepted operations are
// acknowledged with a monotonic cursor advance. Rejected operations become
// conflict records and remain retryable or manually reviewable.
func (s OfflineSyncService) Intake(batch OfflineSyncBatch) ([]OfflineReceipt, error) {
	now := s.now()
	receipts := make([]OfflineReceipt, 0, len(batch.Operations))
	nextCursor := batch.NextCursor

	for _, op := range batch.Operations {
		if !AcceptsOfflineSchema(batch.Gate) {
			conflict := OfflineConflict{
				ID:             NewID(),
				OperationID:    op.ID,
				DeviceID:       op.DeviceID,
				BranchID:       op.BranchID,
				Site:           op.Site,
				EntityType:     op.EntityType,
				EntityID:       op.EntityID,
				ReasonCode:     CodeValidationFailed,
				ResolutionMode: "retry",
				ServerVersion:  op.BaseVersion,
				RecordedAt:     now,
			}
			_ = s.saveConflict(conflict)
			receipts = append(receipts, OfflineReceipt{
				OperationID: op.ID,
				Status:      OfflineOperationConflict,
				Conflict:    &conflict,
				ReceivedAt:  now,
				NextCursor:  nextCursor,
			})
			continue
		}

		if s.Store != nil {
			if err := s.Store.SaveOperation(op); err != nil {
				return nil, err
			}
			if err := s.Store.AdvanceCursor(nextCursor); err != nil {
				return nil, err
			}
		}

		receipts = append(receipts, OfflineReceipt{
			OperationID: op.ID,
			Status:      OfflineOperationAcked,
			ReceivedAt:  now,
			NextCursor:  nextCursor,
		})
	}

	return receipts, nil
}

// Acknowledge advances the cursor for a confirmed operation and optionally
// stores the resulting conflict if the operation was rejected upstream.
func (s OfflineSyncService) Acknowledge(op OfflineOperation, cursor SyncCursor, conflict *OfflineConflict) error {
	if conflict != nil && s.Store != nil {
		if err := s.Store.SaveConflict(*conflict); err != nil {
			return err
		}
	}
	if s.Store != nil {
		if err := s.Store.AdvanceCursor(cursor); err != nil {
			return err
		}
	}
	return nil
}

// PullSince returns a repeatable page of operations after the provided cursor.
func PullSince(ops []OfflineOperation, cursor SyncCursor) OfflinePullPage {
	filtered := make([]OfflineOperation, 0, len(ops))
	for _, op := range ops {
		if op.OccurredAt.After(cursor.At) || op.OccurredAt.Equal(cursor.At) && op.ID > cursor.Token {
			filtered = append(filtered, op)
		}
	}

	next := cursor
	if len(filtered) > 0 {
		last := filtered[len(filtered)-1]
		next = SyncCursor{
			Token:    last.ID,
			BranchID: cursor.BranchID,
			DeviceID: cursor.DeviceID,
			Version:  cursor.Version + len(filtered),
			At:       last.OccurredAt,
		}
	}

	return OfflinePullPage{
		Operations: filtered,
		Cursor:     cursor,
		NextCursor: next,
	}
}

func (s OfflineSyncService) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now().UTC()
}

func (s OfflineSyncService) saveConflict(conflict OfflineConflict) error {
	if s.Store == nil {
		return nil
	}
	return s.Store.SaveConflict(conflict)
}
