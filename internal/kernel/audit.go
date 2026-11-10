package kernel

import (
	"database/sql"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
)

// AuditRow is one immutable operation-audit ledger entry
// (_kora_operation_audit). The ledger records who did what, from which
// source, with which outcome — never payloads or secrets. Before/after
// hashes (KERNEL-009) carry sha256 digests of canonicalized document state
// so tampering is detectable without storing sensitive values.
type AuditRow struct {
	ID            string
	Site          string
	OperationID   string
	CorrelationID string
	CausationID   string
	Source        string
	PrincipalType string
	PrincipalID   string
	ActorUser     string
	ActorRoles    string // JSON array of role names; empty when none
	Command       string
	Doctype       string
	DocName       string
	Status        string
	ErrorCode     string
	PayloadHash   string
	BeforeHash    string
	AfterHash     string
}

// writeAudit persists an audit row inside the caller's transaction so that a
// business commit and its audit entry are atomic (KERNEL-007).
func (k *Kernel) writeAudit(dbTx *sql.Tx, row AuditRow) (string, error) {
	if row.ID == "" {
		row.ID = contract.NewEventID()
	}
	_, err := dbTx.Exec(
		db.Rebind(k.dialect(), `INSERT INTO _kora_operation_audit
			(id, site, operation_id, correlation_id, causation_id, source,
			 principal_type, principal_id, actor_user, actor_roles,
			 command_name, doctype, doc_name, status, error_code, payload_hash, before_hash, after_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		row.ID, row.Site, row.OperationID, row.CorrelationID, row.CausationID, row.Source,
		row.PrincipalType, row.PrincipalID, row.ActorUser, row.ActorRoles,
		row.Command, row.Doctype, row.DocName, row.Status, row.ErrorCode, row.PayloadHash,
		row.BeforeHash, row.AfterHash,
	)
	if err != nil {
		return "", err
	}
	return row.ID, nil
}

// auditOutcome is the record an audit row is about and how the operation ended.
type auditOutcome struct {
	Doctype, DocName string
	Status           contract.Status
	ErrorCode        string
}

// buildAuditRow constructs the success/failure row for an executed operation.
func (k *Kernel) buildAuditRow(op Operation, def CommandDefinition, opID string, outcome auditOutcome) AuditRow {
	user := op.Context.User
	if user == "" {
		user = "system"
	}
	return AuditRow{
		ID:            contract.NewEventID(),
		Site:          op.Context.Site,
		OperationID:   opID,
		CorrelationID: op.Context.CorrelationID,
		CausationID:   op.Context.CausationID,
		Source:        string(op.Context.Source),
		PrincipalType: string(op.Context.Actor.PrincipalType),
		PrincipalID:   op.Context.Actor.PrincipalID,
		ActorUser:     user,
		Command:       def.Name,
		Doctype:       outcome.Doctype,
		DocName:       outcome.DocName,
		Status:        string(outcome.Status),
		ErrorCode:     outcome.ErrorCode,
		PayloadHash:   payloadHash(op.Payload),
	}
}

// writeDenialAudit records authorization failures on a best-effort separate
// connection. Denials must be visible even though nothing else commits.
func (k *Kernel) writeDenialAudit(siteDB *sql.DB, op Operation, def CommandDefinition, opID string, authzErr *contract.Error) {
	k.writeFailureAudit(siteDB, op, def, opID, authzErr.Type)
}

// writeFailureAudit records a failed/rejected attempt outside any open
// transaction. Errors are swallowed by design: audit is evidence, and losing
// a denial row must never turn into a user-visible failure loop.
func (k *Kernel) writeFailureAudit(siteDB *sql.DB, op Operation, def CommandDefinition, opID string, code contract.Code) {
	tx, err := siteDB.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	doctypeName := payloadDoctype(op.Payload)
	row := k.buildAuditRow(op, def, opID, auditOutcome{Doctype: doctypeName, Status: contract.StatusRejected, ErrorCode: string(code)})
	_, _ = k.writeAudit(tx, row)
	_ = tx.Commit()
}
