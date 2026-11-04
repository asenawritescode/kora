package plugin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

// SQLPublishedQueryProvider is the durable Engine-owned adapter used by owner
// modules that publish query facts from the tenant database. Facts are an
// append-only owner projection: callers must write them in the same SQL
// transaction as the authoritative business effect. The provider never reads
// private domain tables or infers query semantics from DocTypes.
type SQLPublishedQueryProvider struct {
	database *sql.DB
	dialect  db.Dialect
}

func (p *SQLPublishedQueryProvider) SupportsOfflineActorScope(actor PublishedQueryActorScope) bool {
	return len(actor.AssignedFields) == 0
}

// NewSQLPublishedQueryProvider creates a provider over an already-open tenant
// database. EnsureSchema must succeed before the provider is registered.
func NewSQLPublishedQueryProvider(database *sql.DB, dialect db.Dialect) (*SQLPublishedQueryProvider, error) {
	if database == nil || dialect == nil {
		return nil, fmt.Errorf("published query provider requires a database and dialect")
	}
	return &SQLPublishedQueryProvider{database: database, dialect: dialect}, nil
}

// EnsureSchema creates only the Engine-owned append log. Domain modules remain
// responsible for deciding which published facts are written to it.
func (p *SQLPublishedQueryProvider) EnsureSchema(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("published query schema requires context")
	}
	statement := `CREATE TABLE IF NOT EXISTS _kora_published_query_change (
		sequence INTEGER PRIMARY KEY AUTOINCREMENT,
		tenant_id TEXT NOT NULL,
		scope_kind TEXT NOT NULL,
		legal_entity_id TEXT NOT NULL,
		owner_module_id TEXT NOT NULL,
		query_name TEXT NOT NULL,
		input_hash TEXT NOT NULL,
		fact_key TEXT NOT NULL,
		owner_id TEXT NOT NULL,
		deleted INTEGER NOT NULL,
		data_json TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`
	switch p.dialect.DriverName() {
	case "mysql":
		statement = `CREATE TABLE IF NOT EXISTS _kora_published_query_change (
			sequence BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			tenant_id VARCHAR(140) NOT NULL,
			scope_kind VARCHAR(32) NOT NULL,
			legal_entity_id VARCHAR(160) NOT NULL,
			owner_module_id VARCHAR(160) NOT NULL,
			query_name VARCHAR(160) NOT NULL,
			input_hash CHAR(64) NOT NULL,
			fact_key VARCHAR(256) NOT NULL,
			owner_id VARCHAR(160) NOT NULL,
			deleted BOOLEAN NOT NULL,
			data_json LONGTEXT NOT NULL,
			created_at DATETIME(6) NOT NULL,
			INDEX idx_kora_published_query_snapshot (tenant_id(40),scope_kind,legal_entity_id(40),owner_module_id(40),query_name(40),input_hash,owner_id(40),fact_key(64),sequence),
			INDEX idx_kora_published_query_replay (tenant_id(40),scope_kind,legal_entity_id(40),owner_module_id(40),query_name(40),input_hash,owner_id(40),sequence)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`
	case "postgres":
		statement = `CREATE TABLE IF NOT EXISTS _kora_published_query_change (
			sequence BIGSERIAL PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			scope_kind TEXT NOT NULL,
			legal_entity_id TEXT NOT NULL,
			owner_module_id TEXT NOT NULL,
			query_name TEXT NOT NULL,
			input_hash CHAR(64) NOT NULL,
			fact_key TEXT NOT NULL,
			owner_id TEXT NOT NULL,
			deleted BOOLEAN NOT NULL,
			data_json JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		)`
	}
	if _, err := p.database.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("create published query change log: %w", err)
	}
	if p.dialect.DriverName() == "postgres" || p.dialect.DriverName() == "libsql" {
		for _, index := range []string{
			`CREATE INDEX IF NOT EXISTS idx_kora_published_query_snapshot ON _kora_published_query_change (tenant_id,scope_kind,legal_entity_id,owner_module_id,query_name,input_hash,owner_id,fact_key,sequence)`,
			`CREATE INDEX IF NOT EXISTS idx_kora_published_query_replay ON _kora_published_query_change (tenant_id,scope_kind,legal_entity_id,owner_module_id,query_name,input_hash,owner_id,sequence)`,
		} {
			if _, err := p.database.ExecContext(ctx, index); err != nil {
				return fmt.Errorf("create published query change index: %w", err)
			}
		}
	}
	return nil
}

// PutFactTx appends a full replacement row. The caller owns tx and must commit
// it with the business effect that made the fact authoritative.
func (p *SQLPublishedQueryProvider) PutFactTx(ctx context.Context, tx *sql.Tx, execution contract.ModuleExecution, query contract.PublishedQuery, input, data json.RawMessage) (uint64, error) {
	if tx == nil || !execution.Verified() || execution.OwnerModuleID != query.OwnerModuleID || execution.AllowedOperation != query.Operation {
		return 0, fmt.Errorf("published fact write requires verified owner execution and transaction")
	}
	var row map[string]json.RawMessage
	if len(input) == 0 || !json.Valid(input) || json.Unmarshal(data, &row) != nil {
		return 0, fmt.Errorf("published fact write contains invalid JSON")
	}
	allowed := make(map[string]bool, len(query.AllowedFields))
	for _, field := range query.AllowedFields {
		allowed[field] = true
	}
	for field, value := range row {
		if !allowed[field] || !json.Valid(value) {
			return 0, fmt.Errorf("published fact contains undeclared field %q", field)
		}
	}
	factKey, err := publishedScalar(row[query.OfflineFactKey])
	if err != nil {
		return 0, fmt.Errorf("published fact key: %w", err)
	}
	ownerID, err := publishedScalar(row[query.OwnerField])
	if err != nil {
		return 0, fmt.Errorf("published fact owner: %w", err)
	}
	previousOwner, found, err := p.latestFactOwnerTx(ctx, tx, execution.Scope, query, input, factKey)
	if err != nil {
		return 0, err
	}
	if found && previousOwner != ownerID {
		if _, err := p.appendChange(ctx, tx, execution.Scope, query, input, factKey, previousOwner, true, json.RawMessage(`{}`)); err != nil {
			return 0, fmt.Errorf("append previous-owner tombstone: %w", err)
		}
	}
	return p.appendChange(ctx, tx, execution.Scope, query, input, factKey, ownerID, false, data)
}

// DeleteFactTx appends a tombstone. ownerID is retained on the tombstone so a
// deletion can only be replayed to actors who could see the prior fact.
func (p *SQLPublishedQueryProvider) DeleteFactTx(ctx context.Context, tx *sql.Tx, execution contract.ModuleExecution, query contract.PublishedQuery, input json.RawMessage, factKey, ownerID string) (uint64, error) {
	if tx == nil || !execution.Verified() || execution.OwnerModuleID != query.OwnerModuleID || execution.AllowedOperation != query.Operation || strings.TrimSpace(factKey) == "" || strings.TrimSpace(ownerID) == "" {
		return 0, fmt.Errorf("published fact tombstone requires verified owner identity")
	}
	return p.appendChange(ctx, tx, execution.Scope, query, input, factKey, ownerID, true, json.RawMessage(`{}`))
}

func (p *SQLPublishedQueryProvider) latestFactOwnerTx(ctx context.Context, tx *sql.Tx, scope contract.BusinessScope, query contract.PublishedQuery, input json.RawMessage, factKey string) (string, bool, error) {
	var owner string
	err := tx.QueryRowContext(ctx, db.Rebind(p.dialect, `SELECT owner_id FROM _kora_published_query_change WHERE tenant_id=? AND scope_kind=? AND legal_entity_id=? AND owner_module_id=? AND query_name=? AND input_hash=? AND fact_key=? ORDER BY sequence DESC LIMIT 1`), scope.TenantID, scope.Kind, scope.LegalEntityID, query.OwnerModuleID, query.Name, publishedInputHash(input), factKey).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read prior published fact owner: %w", err)
	}
	return owner, true, nil
}

func (p *SQLPublishedQueryProvider) appendChange(ctx context.Context, tx *sql.Tx, scope contract.BusinessScope, query contract.PublishedQuery, input json.RawMessage, factKey, ownerID string, deleted bool, data json.RawMessage) (uint64, error) {
	if ctx == nil || scope.Validate() != nil || contract.ValidateResourceBusinessScope(scope, query.ScopePolicy) != nil || !query.OfflineEligible || query.OfflineFactKey == "" || query.OwnerField == "" || len(factKey) > 256 || len(ownerID) > 160 || len(input) == 0 || !json.Valid(input) || len(data) > 1<<20 || !json.Valid(data) {
		return 0, fmt.Errorf("published fact change is outside its registered contract")
	}
	hash := publishedInputHash(input)
	args := []any{scope.TenantID, scope.Kind, scope.LegalEntityID, query.OwnerModuleID, query.Name, hash, factKey, ownerID, deleted, string(data), time.Now().UTC()}
	insert := `INSERT INTO _kora_published_query_change (tenant_id,scope_kind,legal_entity_id,owner_module_id,query_name,input_hash,fact_key,owner_id,deleted,data_json,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`
	if p.dialect.DriverName() == "postgres" {
		var sequence uint64
		if err := tx.QueryRowContext(ctx, db.Rebind(p.dialect, insert+` RETURNING sequence`), args...).Scan(&sequence); err != nil {
			return 0, fmt.Errorf("append published fact change: %w", err)
		}
		return sequence, nil
	}
	result, err := tx.ExecContext(ctx, db.Rebind(p.dialect, insert), args...)
	if err != nil {
		return 0, fmt.Errorf("append published fact change: %w", err)
	}
	sequence, err := result.LastInsertId()
	if err != nil || sequence < 1 {
		return 0, fmt.Errorf("read published fact sequence: %w", err)
	}
	return uint64(sequence), nil
}

func (p *SQLPublishedQueryProvider) ExecutePublishedQuery(ctx context.Context, execution contract.ModuleExecution, query contract.PublishedQuery, input json.RawMessage, cursor string, limit uint16) (contract.QueryResult, error) {
	if !execution.Verified() || execution.OwnerModuleID != query.OwnerModuleID || execution.Scope.Validate() != nil || contract.ValidateResourceBusinessScope(execution.Scope, query.ScopePolicy) != nil || !query.OfflineEligible || query.OfflineFactKey == "" || len(input) == 0 || !json.Valid(input) {
		return contract.QueryResult{}, fmt.Errorf("durable published query execution is invalid")
	}
	return p.executeSnapshot(ctx, execution, query, input, cursor, limit, "")
}

func (p *SQLPublishedQueryProvider) ExecutePublishedQueryForActor(ctx context.Context, execution contract.ModuleExecution, query contract.PublishedQuery, actor PublishedQueryActorScope, input json.RawMessage, cursor string, limit uint16) (contract.QueryResult, error) {
	if err := validateSQLPublishedActor(execution, query, actor); err != nil {
		return contract.QueryResult{}, err
	}
	if len(actor.AssignedFields) != 0 {
		return contract.QueryResult{}, fmt.Errorf("durable published query assignments require an owner-published visibility projection")
	}
	subject := ""
	if actor.OwnerOnly {
		subject = actor.SubjectID
	}
	return p.executeSnapshot(ctx, execution, query, input, cursor, limit, subject)
}

func (p *SQLPublishedQueryProvider) executeSnapshot(ctx context.Context, execution contract.ModuleExecution, query contract.PublishedQuery, input json.RawMessage, cursor string, limit uint16, subject string) (contract.QueryResult, error) {
	high := uint64(0)
	factCursor := ""
	var err error
	if cursor == "" {
		high, err = p.highWater(ctx, execution.Scope, query, input)
	} else {
		high, factCursor, err = parsePublishedSnapshotCursor(cursor)
	}
	if err != nil {
		return contract.QueryResult{}, fmt.Errorf("published query snapshot cursor is invalid: %w", err)
	}
	rows, nextFact, err := p.snapshotRows(ctx, execution.Scope, query, subject, input, factCursor, high, limit)
	if err != nil {
		return contract.QueryResult{}, err
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		return contract.QueryResult{}, fmt.Errorf("encode published query snapshot: %w", err)
	}
	next := ""
	if nextFact != "" {
		next = publishedSnapshotCursor(high, nextFact)
	}
	return contract.QueryResult{Data: encoded, NextCursor: next, SnapshotToken: "snapshot:" + strconv.FormatUint(high, 10), SourceCursor: strconv.FormatUint(high, 10), SourceRevision: "revision:" + strconv.FormatUint(high, 10), AsOf: time.Now().UTC(), Freshness: contract.QueryFresh}, nil
}

func (p *SQLPublishedQueryProvider) ReplayPublishedQuery(ctx context.Context, execution contract.ModuleExecution, query contract.PublishedQuery, input json.RawMessage, after, through, replay string, limit uint16) (contract.QueryReplayPage, error) {
	if !execution.Verified() || execution.OwnerModuleID != query.OwnerModuleID || execution.Scope.Validate() != nil || contract.ValidateResourceBusinessScope(execution.Scope, query.ScopePolicy) != nil || !query.OfflineEligible || query.OfflineFactKey == "" || len(input) == 0 || !json.Valid(input) {
		return contract.QueryReplayPage{}, fmt.Errorf("durable published query replay is invalid")
	}
	return p.replay(ctx, execution, query, input, "snapshot:"+after, after, through, replay, limit, "")
}

func (p *SQLPublishedQueryProvider) ReplayPublishedQueryForActor(ctx context.Context, execution contract.ModuleExecution, query contract.PublishedQuery, actor PublishedQueryActorScope, input json.RawMessage, snapshot, after, through, replay string, limit uint16) (contract.QueryReplayPage, error) {
	if err := validateSQLPublishedActor(execution, query, actor); err != nil {
		return contract.QueryReplayPage{}, err
	}
	if len(actor.AssignedFields) != 0 {
		return contract.QueryReplayPage{}, fmt.Errorf("durable published query assignments require an owner-published visibility projection")
	}
	subject := ""
	if actor.OwnerOnly {
		subject = actor.SubjectID
	}
	return p.replay(ctx, execution, query, input, snapshot, after, through, replay, limit, subject)
}

func (p *SQLPublishedQueryProvider) replay(ctx context.Context, execution contract.ModuleExecution, query contract.PublishedQuery, input json.RawMessage, snapshot, after, through, replay string, limit uint16, subject string) (contract.QueryReplayPage, error) {
	snapshotHigh, err := parsePublishedCursor(strings.TrimPrefix(snapshot, "snapshot:"))
	if err != nil || snapshot != "snapshot:"+strconv.FormatUint(snapshotHigh, 10) || after != strconv.FormatUint(snapshotHigh, 10) {
		return contract.QueryReplayPage{}, fmt.Errorf("published query snapshot cursor mismatch")
	}
	start := snapshotHigh
	if replay != "" {
		start, err = parsePublishedCursor(replay)
		if err != nil || start < snapshotHigh {
			return contract.QueryReplayPage{}, fmt.Errorf("published query replay cursor is invalid")
		}
	}
	high := uint64(0)
	if through == "" {
		high, err = p.highWater(ctx, execution.Scope, query, input)
	} else {
		high, err = parsePublishedCursor(through)
	}
	if err != nil || high < start {
		return contract.QueryReplayPage{}, fmt.Errorf("published query replay high-water mark is invalid")
	}
	changes, next, err := p.replayRows(ctx, execution.Scope, query, subject, input, start, high, limit)
	if err != nil {
		return contract.QueryReplayPage{}, err
	}
	return contract.QueryReplayPage{Changes: changes, NextCursor: next, ThroughCursor: strconv.FormatUint(high, 10), SourceRevision: "revision:" + strconv.FormatUint(high, 10), AsOf: time.Now().UTC(), Freshness: contract.QueryFresh}, nil
}

func validateSQLPublishedActor(execution contract.ModuleExecution, query contract.PublishedQuery, actor PublishedQueryActorScope) error {
	if !execution.Verified() || execution.Scope != actor.BusinessScope || execution.OwnerModuleID != query.OwnerModuleID || !query.OfflineEligible || query.OfflineFactKey == "" || strings.TrimSpace(actor.SubjectID) == "" {
		return fmt.Errorf("durable published query actor scope is invalid")
	}
	if actor.OwnerOnly && (actor.OwnerField == "" || actor.OwnerField != query.OwnerField) {
		return fmt.Errorf("durable published query owner scope is invalid")
	}
	return nil
}

func (p *SQLPublishedQueryProvider) highWater(ctx context.Context, scope contract.BusinessScope, query contract.PublishedQuery, input json.RawMessage) (uint64, error) {
	var high uint64
	err := p.database.QueryRowContext(ctx, db.Rebind(p.dialect, `SELECT COALESCE(MAX(sequence),0) FROM _kora_published_query_change WHERE tenant_id=? AND scope_kind=? AND legal_entity_id=? AND owner_module_id=? AND query_name=? AND input_hash=?`), scope.TenantID, scope.Kind, scope.LegalEntityID, query.OwnerModuleID, query.Name, publishedInputHash(input)).Scan(&high)
	if err != nil {
		return 0, fmt.Errorf("read published query high-water mark: %w", err)
	}
	return high, nil
}

func (p *SQLPublishedQueryProvider) snapshotRows(ctx context.Context, scope contract.BusinessScope, query contract.PublishedQuery, subject string, input json.RawMessage, cursor string, high uint64, limit uint16) ([]json.RawMessage, string, error) {
	base := `SELECT c.fact_key,c.data_json FROM _kora_published_query_change c WHERE c.tenant_id=? AND c.scope_kind=? AND c.legal_entity_id=? AND c.owner_module_id=? AND c.query_name=? AND c.input_hash=?`
	args := []any{scope.TenantID, scope.Kind, scope.LegalEntityID, query.OwnerModuleID, query.Name, publishedInputHash(input)}
	if subject != "" {
		base += ` AND c.owner_id=?`
		args = append(args, subject)
	}
	base += ` AND c.fact_key>? AND c.sequence<=? AND c.deleted=? AND c.sequence=(SELECT MAX(c2.sequence) FROM _kora_published_query_change c2 WHERE c2.tenant_id=c.tenant_id AND c2.scope_kind=c.scope_kind AND c2.legal_entity_id=c.legal_entity_id AND c2.owner_module_id=c.owner_module_id AND c2.query_name=c.query_name AND c2.input_hash=c.input_hash AND c2.fact_key=c.fact_key AND c2.sequence<=?) ORDER BY c.fact_key LIMIT ?`
	args = append(args, cursor, high, false, high, int(limit)+1)
	rows, err := p.database.QueryContext(ctx, db.Rebind(p.dialect, base), args...)
	if err != nil {
		return nil, "", fmt.Errorf("query published snapshot: %w", err)
	}
	defer rows.Close()
	result := make([]json.RawMessage, 0, limit)
	next := ""
	lastKey := ""
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, "", fmt.Errorf("scan published snapshot: %w", err)
		}
		if len(result) == int(limit) {
			next = lastKey
			break
		}
		result = append(result, append(json.RawMessage(nil), raw...))
		lastKey = key
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate published snapshot: %w", err)
	}
	return result, next, nil
}

func (p *SQLPublishedQueryProvider) replayRows(ctx context.Context, scope contract.BusinessScope, query contract.PublishedQuery, subject string, input json.RawMessage, start, high uint64, limit uint16) ([]contract.QueryReplayChange, string, error) {
	statement := `SELECT sequence,fact_key,deleted,data_json FROM _kora_published_query_change WHERE tenant_id=? AND scope_kind=? AND legal_entity_id=? AND owner_module_id=? AND query_name=? AND input_hash=?`
	args := []any{scope.TenantID, scope.Kind, scope.LegalEntityID, query.OwnerModuleID, query.Name, publishedInputHash(input)}
	if subject != "" {
		statement += ` AND owner_id=?`
		args = append(args, subject)
	}
	statement += ` AND sequence>? AND sequence<=? ORDER BY sequence LIMIT ?`
	args = append(args, start, high, int(limit)+1)
	rows, err := p.database.QueryContext(ctx, db.Rebind(p.dialect, statement), args...)
	if err != nil {
		return nil, "", fmt.Errorf("query published replay: %w", err)
	}
	defer rows.Close()
	result := make([]contract.QueryReplayChange, 0, limit)
	var last uint64
	hasMore := false
	for rows.Next() {
		var sequence uint64
		var key string
		var deleted bool
		var raw []byte
		if err := rows.Scan(&sequence, &key, &deleted, &raw); err != nil {
			return nil, "", fmt.Errorf("scan published replay: %w", err)
		}
		if len(result) == int(limit) {
			hasMore = true
			break
		}
		change := contract.QueryReplayChange{FactKey: key, Deleted: deleted}
		if !deleted {
			change.Data = append(json.RawMessage(nil), raw...)
		}
		result = append(result, change)
		last = sequence
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate published replay: %w", err)
	}
	if hasMore {
		return result, strconv.FormatUint(last, 10), nil
	}
	return result, "", nil
}

func publishedInputHash(input json.RawMessage) string {
	digest := sha256.Sum256(input)
	return hex.EncodeToString(digest[:])
}

func publishedScalar(raw json.RawMessage) (string, error) {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value) == "" || len(value) > 256 {
		return "", fmt.Errorf("value must be a bounded non-empty string")
	}
	return value, nil
}

func parsePublishedCursor(value string) (uint64, error) {
	if value == "" || len(value) > 20 || value != "0" && strings.HasPrefix(value, "0") {
		return 0, fmt.Errorf("cursor is invalid")
	}
	return strconv.ParseUint(value, 10, 63)
}

func publishedSnapshotCursor(high uint64, factKey string) string {
	return strconv.FormatUint(high, 10) + "." + base64.RawURLEncoding.EncodeToString([]byte(factKey))
}

func parsePublishedSnapshotCursor(value string) (uint64, string, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 || parts[1] == "" {
		return 0, "", fmt.Errorf("snapshot cursor has invalid shape")
	}
	high, err := parsePublishedCursor(parts[0])
	if err != nil {
		return 0, "", err
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(decoded) == 0 || len(decoded) > 256 || publishedSnapshotCursor(high, string(decoded)) != value {
		return 0, "", fmt.Errorf("snapshot cursor has invalid fact identity")
	}
	return high, string(decoded), nil
}
