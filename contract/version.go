package contract

import "time"

// VersionMismatchError is the typed result of a failed document-level
// optimistic-concurrency write (OFFLINE-003). ExpectedVersion is what the
// writer assumed; ActualVersion is the current committed version.
//
//contract:wire
type VersionMismatchError struct {
	Kind            string `json:"kind"` // "version_mismatch"
	Doctype         string `json:"-"`
	Name            string `json:"-"`
	ExpectedVersion int64  `json:"expected_version"`
	ActualVersion   int64  `json:"actual_version"`
}

func (e *VersionMismatchError) Error() string {
	return "version mismatch: expected " + itoa64(e.ExpectedVersion) + ", actual " + itoa64(e.ActualVersion)
}

// ConflictRecord is the durable record of a losing offline write
// (OFFLINE-003). It survives restarts until acknowledged or merged.
//
//contract:wire
type ConflictRecord struct {
	Name            string    `json:"name"`
	Site            string    `json:"site"`
	Doctype         string    `json:"doctype"`
	DocName         string    `json:"doc_name"`
	LosingExpectedV int64     `json:"losing_expected_version"`
	WinningActualV  int64     `json:"winning_actual_version"`
	LoserCommandKey string    `json:"loser_command_key"`
	DetectedAt      time.Time `json:"detected_at"`
	State           string    `json:"state"` // open | acknowledged | merged
}

// DetectVersionConflict reports a conflict when a writer's expected version is
// stale (positive and not equal to the actual committed version). It returns
// nil when the write is up-to-date or versioning is not enforced (expected<=0).
func DetectVersionConflict(expected, actual int64) *VersionMismatchError {
	if expected > 0 && expected != actual {
		return &VersionMismatchError{
			Kind:            "version_mismatch",
			ExpectedVersion: expected,
			ActualVersion:   actual,
		}
	}
	return nil
}

// NewConflictRecord builds an open conflict from a losing write.
func NewConflictRecord(site, doctype, docName string, losingExpected, winningActual int64, loserKey string, now time.Time) ConflictRecord {
	return ConflictRecord{
		Name:            NewID(),
		Site:            site,
		Doctype:         doctype,
		DocName:         docName,
		LosingExpectedV: losingExpected,
		WinningActualV:  winningActual,
		LoserCommandKey: loserKey,
		DetectedAt:      now,
		State:           "open",
	}
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
