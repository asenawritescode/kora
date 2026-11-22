package ai

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"

	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
)

// A chat write is previewed first and runs only when the person confirms it. The preview
// is a row in _kora_ai_pending_write, keyed by its write_id. The write_id is not a secret:
// confirm also requires the same site, actor and conversation, and a turn other than the
// one that made the preview.
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
	PreviewJSON    string
	State          string
	Attempts       int
	Outcome        string
	Expired        bool
}

// newWriteID returns the random id of a preview. It is the row's primary key.
func newWriteID() string {
	return "pw_" + ulid.Make().String()
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

// storePendingWrite records a preview as previewed and returns its write_id.
func storePendingWrite(ctx context.Context, database *sql.DB, p pendingPreview) (string, error) {
	id := newWriteID()
	_, err := (aiSQL{database}).ExecContext(ctx, `
INSERT INTO _kora_ai_pending_write (id, site, conversation_id, run_id, actor_id, tool_name, args_hash, args_json, preview_json, state, attempts, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, `+dialectFromContext(ctx).NowTimestamp()+`)`,
		id, p.Site, p.ConversationID, p.RunID, p.ActorID, p.ToolName, pendingArgsHash(p.ArgsJSON), p.ArgsJSON, p.PreviewJSON, pendingPreviewed)
	if err != nil {
		return "", fmt.Errorf("store pending write: %w", err)
	}
	return id, nil
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
// Every refusal is an aiToolError: a write_id that belongs to another person, site or
// conversation, or is in the same turn, or whose arguments changed, is confirm_invalid; an
// expired preview is confirm_expired. A write already done returns its first outcome.
func claimPendingWrite(ctx context.Context, database *sql.DB, writeID string, caller pendingCaller) (pendingWrite, *aiToolError) {
	row, err := loadPendingWrite(ctx, database, caller.Site, writeID)
	if errors.Is(err, sql.ErrNoRows) {
		return pendingWrite{}, invalidConfirm("No pending write matches this write_id.")
	}
	if err != nil {
		return pendingWrite{}, &aiToolError{Code: errConfirmInvalid, Message: "The pending write could not be read."}
	}
	if row.ActorID != caller.ActorID || row.ConversationID != caller.ConversationID {
		return pendingWrite{}, invalidConfirm("No pending write matches this write_id.")
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
		return pendingWrite{}, &aiToolError{Code: errConfirmExpired, Message: "This preview expired. Preview the write again.", Fix: "Call the create, update or run_action tool again to get a new write_id."}
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
	return &aiToolError{Code: errConfirmInvalid, Message: message, Fix: "Ask the person to confirm the write, then call confirm_write with the write_id from the latest preview."}
}

// pendingContextLimit is how many open previews one turn lists in its context.
const pendingContextLimit = 3

// executionOwner is the identity a write belongs to: the subject user, else the principal.
func executionOwner(exec policy.Execution) string {
	actor := exec.Actor()
	if actor.SubjectUserID != "" {
		return actor.SubjectUserID
	}
	return actor.PrincipalID
}

// previewOwner is the actor a chat turn's previews are stored under. It resolves the way the
// tool path does: the verified execution, then the transaction's, else the current user.
func previewOwner(c *gin.Context, tx *orm.TxManager, currentUser string) string {
	var exec policy.Execution
	if verified, ok := c.Get("policy_execution"); ok {
		exec, _ = verified.(policy.Execution)
	}
	if !exec.Valid() {
		exec = tx.Execution
	}
	if exec.Valid() {
		return executionOwner(exec)
	}
	return currentUser
}

// listOpenPreviews returns the caller's previews of this conversation that can still be
// confirmed, newest first. The expiry is judged by the database clock.
func listOpenPreviews(ctx context.Context, database *sql.DB, caller pendingCaller) ([]pendingWrite, error) {
	rows, err := (aiSQL{database}).QueryContext(ctx, fmt.Sprintf(`
SELECT id, tool_name, preview_json FROM _kora_ai_pending_write
WHERE site = ? AND actor_id = ? AND conversation_id = ? AND state = ? AND created_at > %s
ORDER BY created_at DESC, id DESC
LIMIT %d`, dialectFromContext(ctx).NowMinusSeconds(), pendingContextLimit),
		caller.Site, caller.ActorID, caller.ConversationID, pendingPreviewed, pendingTTLSeconds)
	if err != nil {
		return nil, fmt.Errorf("list open previews: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var open []pendingWrite
	for rows.Next() {
		var p pendingWrite
		if err := rows.Scan(&p.ID, &p.ToolName, &p.PreviewJSON); err != nil {
			return nil, fmt.Errorf("scan open preview: %w", err)
		}
		open = append(open, p)
	}
	return open, rows.Err()
}

// previewView is the part of a stored preview that the context line shows.
type previewView struct {
	Action     string         `json:"action"`
	RecordType string         `json:"record_type"`
	Name       string         `json:"name"`
	Title      string         `json:"title"`
	Fields     map[string]any `json:"fields"`
	Input      map[string]any `json:"input"`
	Changes    map[string]any `json:"changes"`
}

// summary says what confirming the preview would do, with its key values, on one line.
func (p pendingWrite) summary() string {
	var v previewView
	if err := json.Unmarshal([]byte(p.PreviewJSON), &v); err != nil {
		return "a previewed write"
	}
	var line string
	switch {
	case p.ToolName == toolRunAction:
		title := v.Title
		if title == "" {
			title = v.Action
		}
		line = title + " (" + pairsText(v.Input) + ")"
	case v.Action == "update":
		line = fmt.Sprintf("update %s %s (%s)", v.RecordType, v.Name, pairsText(v.Changes))
	default:
		line = fmt.Sprintf("create %s (%s)", v.RecordType, pairsText(v.Fields))
	}
	return truncateScript(line, 240)
}

// pairsText formats values as "name value" pairs in name order. An update change reads "old -> new".
func pairsText(values map[string]any) string {
	keys := make([]string, 0, len(values))
	for k, v := range values {
		if v != nil {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" "+changeText(values[k]))
	}
	return strings.Join(parts, ", ")
}

// changeText shows an update change as "old -> new" and any other value as it is.
func changeText(v any) string {
	if change, ok := v.(map[string]any); ok {
		if _, has := change["new"]; has {
			return fmt.Sprintf("%v -> %v", change["old"], change["new"])
		}
	}
	return fmt.Sprint(v)
}

// openPreviewsBlurb is the per-turn context block that lists the open previews. It is empty
// when there are none.
func openPreviewsBlurb(ctx context.Context, database *sql.DB, caller pendingCaller) (string, error) {
	open, err := listOpenPreviews(ctx, database, caller)
	if err != nil || len(open) == 0 {
		return "", err
	}
	var b strings.Builder
	b.WriteString("\n\nOPEN PREVIEWS (not saved yet):")
	for _, p := range open {
		fmt.Fprintf(&b, "\n- write_id %s: %s", p.ID, p.summary())
	}
	b.WriteString("\nConfirm one with confirm_write {write_id} only after the person agrees in a later message. Never show a write_id in a reply.")
	return b.String(), nil
}

// confirmIdempotencyKey is the kernel idempotency key of a confirmed write. It is derived
// from the stored row, so a replayed confirm returns the first receipt.
func confirmIdempotencyKey(id string) string {
	return "ai-confirm-" + id
}
