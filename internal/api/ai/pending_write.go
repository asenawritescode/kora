package ai

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// A chat write is previewed first and runs only when the person confirms it. The preview
// is a row in _kora_ai_pending_write, keyed by the hash of its confirm token.
//
// States and the one table of allowed moves:
//
//	previewed  -> executing   claimed by confirm_write
//	executing  -> executing   reclaimed after the lease, while attempts remain
//	executing  -> done        the kernel committed; outcome is stored
//	executing  -> failed      the kernel refused; the error is stored
//
// done and failed are final. A preview older than the TTL cannot be claimed.
const (
	pendingPreviewed = "previewed"
	pendingExecuting = "executing"
	pendingDone      = "done"
	pendingFailed    = "failed"
)

var pendingTransitions = map[string][]string{
	pendingPreviewed: {pendingExecuting},
	pendingExecuting: {pendingExecuting, pendingDone, pendingFailed},
	pendingDone:      nil,
	pendingFailed:    nil,
}

// pendingMove reports whether a row may move from one state to another.
func pendingMove(from, to string) error {
	for _, allowed := range pendingTransitions[from] {
		if allowed == to {
			return nil
		}
	}
	return fmt.Errorf("pending write cannot move from %s to %s", from, to)
}

const (
	// pendingTTLSeconds is how long a preview stays confirmable (15 minutes).
	pendingTTLSeconds = 900
	// pendingLeaseSeconds is how long a claim is held before another confirm may reclaim it.
	pendingLeaseSeconds = 60
	// pendingMaxAttempts caps the claims of one preview.
	pendingMaxAttempts = 3
)

// pendingWrite is one stored preview.
type pendingWrite struct {
	ID             string
	Site           string
	ConversationID string
	RunID          string
	ActorID        string
	ToolName       string
	ArgsHash       string
	ArgsJSON       string
	State          string
	Attempts       int
	Outcome        string
	Expired        bool
}

// newConfirmToken returns a random single-use token. Only its hash is stored.
func newConfirmToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate confirm token: %w", err)
	}
	return "cw_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// pendingIDFor returns the storage key of a token.
func pendingIDFor(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// pendingArgsHash fingerprints the stored write arguments. confirm recomputes it.
func pendingArgsHash(argsJSON string) string {
	sum := sha256.Sum256([]byte(argsJSON))
	return hex.EncodeToString(sum[:])
}

// pendingPreview is what storePendingWrite records for one previewed write.
type pendingPreview struct {
	Site           string
	ConversationID string
	RunID          string
	ActorID        string
	ToolName       string
	ArgsJSON       string
	PreviewJSON    string
}

// storePendingWrite records a preview as previewed and returns its confirm token.
func storePendingWrite(ctx context.Context, database *sql.DB, p pendingPreview) (string, error) {
	token, err := newConfirmToken()
	if err != nil {
		return "", err
	}
	_, err = (aiSQL{database}).ExecContext(ctx, `
INSERT INTO _kora_ai_pending_write (id, site, conversation_id, run_id, actor_id, tool_name, args_hash, args_json, preview_json, state, attempts, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, `+dialectFromContext(ctx).NowTimestamp()+`)`,
		pendingIDFor(token), p.Site, p.ConversationID, p.RunID, p.ActorID, p.ToolName, pendingArgsHash(p.ArgsJSON), p.ArgsJSON, p.PreviewJSON, pendingPreviewed)
	if err != nil {
		return "", fmt.Errorf("store pending write: %w", err)
	}
	return token, nil
}

// pendingCaller is the verified identity and turn that asks to confirm a preview.
type pendingCaller struct {
	Site           string
	ConversationID string
	RunID          string
	ActorID        string
}

// loadPendingWrite reads one preview of the site, with its expiry judged by the database clock.
func loadPendingWrite(ctx context.Context, database *sql.DB, site, id string) (pendingWrite, error) {
	var p pendingWrite
	var expired int
	var outcome sql.NullString
	err := (aiSQL{database}).QueryRowContext(ctx, `
SELECT id, site, conversation_id, run_id, actor_id, tool_name, args_hash, args_json, state, attempts, outcome,
	CASE WHEN created_at <= `+dialectFromContext(ctx).NowMinusSeconds()+` THEN 1 ELSE 0 END
FROM _kora_ai_pending_write WHERE id = ? AND site = ?`, pendingTTLSeconds, id, site).Scan(
		&p.ID, &p.Site, &p.ConversationID, &p.RunID, &p.ActorID, &p.ToolName, &p.ArgsHash, &p.ArgsJSON, &p.State, &p.Attempts, &outcome, &expired)
	if err != nil {
		return pendingWrite{}, err
	}
	p.Outcome = outcome.String
	p.Expired = expired == 1
	return p, nil
}

// claimPendingWrite moves a previewed write to executing for one confirm call and returns it.
// Every refusal is an aiToolError: a token that belongs to another person, site or
// conversation, or is in the same turn, or whose arguments changed, is confirm_invalid; an
// expired preview is confirm_expired. A write already done returns its first outcome.
func claimPendingWrite(ctx context.Context, database *sql.DB, token string, caller pendingCaller) (pendingWrite, *aiToolError) {
	id := pendingIDFor(token)
	row, err := loadPendingWrite(ctx, database, caller.Site, id)
	if errors.Is(err, sql.ErrNoRows) {
		return pendingWrite{}, invalidConfirm("No pending write matches this confirm_token.")
	}
	if err != nil {
		return pendingWrite{}, &aiToolError{Code: errConfirmInvalid, Message: "The pending write could not be read."}
	}
	if row.ActorID != caller.ActorID || row.ConversationID != caller.ConversationID {
		return pendingWrite{}, invalidConfirm("No pending write matches this confirm_token.")
	}
	if pendingArgsHash(row.ArgsJSON) != row.ArgsHash {
		return pendingWrite{}, invalidConfirm("The previewed arguments changed. Preview the write again.")
	}
	if row.State == pendingDone {
		return row, nil
	}
	if row.State == pendingFailed {
		return pendingWrite{}, invalidConfirm("This write failed. Preview it again.")
	}
	if row.RunID == caller.RunID {
		return pendingWrite{}, invalidConfirm("The person must reply before this write can be confirmed.")
	}
	if row.Expired {
		return pendingWrite{}, &aiToolError{Code: errConfirmExpired, Message: "This preview expired. Preview the write again.", Fix: "Call the create or update tool again to get a new confirm_token."}
	}
	return claimRow(ctx, database, row, caller)
}

// claimRow runs the conditional UPDATE that moves a row to executing. The guard repeats the
// checks of claimPendingWrite in SQL, so two confirms cannot both claim the same row.
func claimRow(ctx context.Context, database *sql.DB, row pendingWrite, caller pendingCaller) (pendingWrite, *aiToolError) {
	dialect := dialectFromContext(ctx)
	result, err := (aiSQL{database}).ExecContext(ctx, `
UPDATE _kora_ai_pending_write
SET state = ?, attempts = attempts + 1, claimed_at = `+dialect.NowTimestamp()+`
WHERE id = ? AND site = ? AND actor_id = ? AND run_id <> ? AND created_at > `+dialect.NowMinusSeconds()+`
	AND (state = ? OR (state = ? AND attempts < ? AND claimed_at <= `+dialect.NowMinusSeconds()+`))`,
		pendingExecuting, row.ID, caller.Site, caller.ActorID, caller.RunID, pendingTTLSeconds, pendingPreviewed, pendingExecuting, pendingMaxAttempts, pendingLeaseSeconds)
	if err != nil {
		return pendingWrite{}, &aiToolError{Code: errConfirmInvalid, Message: "The pending write could not be claimed."}
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return pendingWrite{}, invalidConfirm("This write is already being confirmed or has used its attempts.")
	}
	row.State = pendingExecuting
	row.Attempts++
	return row, nil
}

// finishPendingWrite stores the outcome of a claimed write as done or failed. It writes only
// from executing, so a stale finish cannot overwrite a newer state.
func finishPendingWrite(ctx context.Context, database *sql.DB, id, state, outcome string) error {
	if err := pendingMove(pendingExecuting, state); err != nil {
		return err
	}
	dialect := dialectFromContext(ctx)
	result, err := (aiSQL{database}).ExecContext(ctx, `
UPDATE _kora_ai_pending_write SET state = ?, completed_at = `+dialect.NowTimestamp()+`, outcome = ?, error_message = ?
WHERE id = ? AND state = ?`, state, outcome, errorIf(state == pendingFailed, outcome), id, pendingExecuting)
	if err != nil {
		return fmt.Errorf("finish pending write: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("pending write %s was not executing", id)
	}
	return nil
}

// errorIf returns message when cond holds, so failed rows keep their reason and done rows keep none.
func errorIf(cond bool, message string) string {
	if !cond {
		return ""
	}
	return strings.TrimSpace(message)
}

func invalidConfirm(message string) *aiToolError {
	return &aiToolError{Code: errConfirmInvalid, Message: message, Fix: "Ask the person to confirm the write, then call confirm_write with the confirm_token from the latest preview."}
}

// confirmIdempotencyKey is the kernel idempotency key of a confirmed write. It is derived
// from the stored row, so a replayed confirm returns the first receipt.
func confirmIdempotencyKey(id string) string {
	return "ai-confirm-" + id
}
