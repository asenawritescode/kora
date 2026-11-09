package analytics

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
)

const maxPublishedProjectionBytes = 16 << 20

// PublishedProjectionStore binds projection access to one tenant database.
type PublishedProjectionStore struct {
	DB      *sql.DB
	Dialect kdb.Dialect
	Site    string
}

func (s PublishedProjectionStore) Save(ctx context.Context, snapshot PublishedProjectionSnapshot) error {
	if s.Site == "" || snapshot.Site != "" && snapshot.Site != s.Site {
		return errors.New("published projection snapshot tenant does not match its store")
	}
	snapshot.Site = s.Site
	return SavePublishedProjection(ctx, s.DB, s.Dialect, snapshot)
}

func (s PublishedProjectionStore) MarkStale(ctx context.Context, definition contract.ProjectionDefinition, scope contract.BusinessScope, sourceID string) error {
	return MarkPublishedProjectionStale(ctx, s.DB, s.Dialect, s.Site, definition, scope, sourceID)
}

func (s PublishedProjectionStore) Load(ctx context.Context, definition contract.ProjectionDefinition, scope contract.BusinessScope, sourceID string) (PublishedProjectionSnapshot, error) {
	return LoadPublishedProjection(ctx, s.DB, s.Dialect, s.Site, definition, scope, sourceID)
}

// PublishedProjectionSnapshot is an immutable, provider-revision-pinned
// projection value persisted in the analytics database. It shares the tenant
// rebuild lock with rollup backfills, so a reader never sees a partial rebuild.
type PublishedProjectionSnapshot struct {
	Site           string
	Definition     contract.ProjectionDefinition
	Scope          contract.BusinessScope
	SourceID       string
	SourceRevision string
	Cursor         string
	AsOf           time.Time
	Freshness      contract.QueryFreshness
	Data           json.RawMessage
}

func (s PublishedProjectionSnapshot) Validate() error {
	if s.Site == "" || s.Definition.ID == "" || s.Definition.OwnerModuleID == "" || s.Scope.Validate() != nil || contract.ValidateResourceBusinessScope(s.Scope, s.Definition.ScopePolicy) != nil || s.SourceID == "" || s.SourceRevision == "" || s.AsOf.IsZero() {
		return fmt.Errorf("published projection snapshot identity is incomplete")
	}
	if err := s.Definition.Validate(); err != nil || len(s.SourceID) > 280 || len(s.SourceRevision) > 256 || len(s.Cursor) > 2048 || len(s.Data) == 0 || len(s.Data) > maxPublishedProjectionBytes || !json.Valid(s.Data) {
		return fmt.Errorf("published projection snapshot data is invalid or exceeds its size limit")
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(s.Data, &rows); err != nil {
		return fmt.Errorf("published projection snapshot must be a JSON row array")
	}
	allowed := make(map[string]bool, len(s.Definition.AllowedFields))
	for _, field := range s.Definition.AllowedFields {
		allowed[field] = true
	}
	for _, row := range rows {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(row, &fields); err != nil || fields[s.Definition.FactKey] == nil {
			return fmt.Errorf("published projection snapshot contains a malformed row")
		}
		for field, value := range fields {
			if !allowed[field] || !json.Valid(value) {
				return fmt.Errorf("published projection snapshot contains an undeclared field")
			}
		}
	}
	if s.Definition.OutputSchema.ID == "" || s.Definition.OutputSchema.Version == 0 {
		return fmt.Errorf("published projection output schema is required")
	}
	switch s.Freshness {
	case contract.QueryFresh, contract.QueryStale, contract.QueryUnknown, contract.QueryIncomplete:
	default:
		return fmt.Errorf("published projection freshness is invalid")
	}
	return nil
}

func (s PublishedProjectionSnapshot) key() string {
	identity := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s", s.Site, s.Definition.ID, s.Definition.OwnerModuleID, s.Scope.Kind, s.Scope.LegalEntityID, s.SourceID, s.SourceRevision)
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

// SavePublishedProjection atomically publishes a complete revision. A failed
// save leaves the previous provider revision available to report consumers.
func SavePublishedProjection(ctx context.Context, database *sql.DB, dialect kdb.Dialect, snapshot PublishedProjectionSnapshot) error {
	if ctx == nil || database == nil || dialect == nil {
		return errors.New("published projection save requires context, database, and dialect")
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if err := BootstrapTables(ctx, database, dialect); err != nil {
		return err
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin published projection save: %w", err)
	}
	defer tx.Rollback()
	if _, err := lockRebuildStateTx(ctx, tx, dialect, snapshot.Site); err != nil {
		return err
	}
	generation, err := rebuildFenceTx(ctx, tx, dialect, snapshot.Site)
	if err != nil {
		return err
	}
	query := `INSERT INTO _kora_analytics_module_projection (snapshot_key, generation, site, projection_id, owner_module_id, scope_kind, legal_entity_id, source_id, source_revision, source_cursor, output_schema_id, output_schema_version, as_of, freshness, data, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	upsert := dialect.UpsertClause([]string{"snapshot_key"}, []string{"generation", "source_cursor", "output_schema_id", "output_schema_version", "as_of", "freshness", "data", "updated_at"})
	query += " " + upsert
	_, err = tx.ExecContext(ctx, kdb.Rebind(dialect, query), snapshot.key(), generation, snapshot.Site, snapshot.Definition.ID, string(snapshot.Definition.OwnerModuleID), string(snapshot.Scope.Kind), snapshot.Scope.LegalEntityID, snapshot.SourceID, snapshot.SourceRevision, snapshot.Cursor, snapshot.Definition.OutputSchema.ID, snapshot.Definition.OutputSchema.Version, snapshot.AsOf.UTC(), string(snapshot.Freshness), string(snapshot.Data), time.Now().UTC())
	if err != nil {
		return fmt.Errorf("write published projection snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit published projection snapshot: %w", err)
	}
	return nil
}

// LoadPublishedProjection returns the latest authorized-scope snapshot for a
// source. Provider unavailability does not erase retained historical output.
func LoadPublishedProjection(ctx context.Context, database *sql.DB, dialect kdb.Dialect, site string, definition contract.ProjectionDefinition, scope contract.BusinessScope, sourceID string) (PublishedProjectionSnapshot, error) {
	if ctx == nil || database == nil || dialect == nil || site == "" || definition.Validate() != nil || scope.Validate() != nil || contract.ValidateResourceBusinessScope(scope, definition.ScopePolicy) != nil || sourceID == "" {
		return PublishedProjectionSnapshot{}, errors.New("published projection load requires a valid tenant, definition, scope, and source")
	}
	var snapshot PublishedProjectionSnapshot
	var owner, scopeKind, freshness string
	var schemaVersion int
	var asOf time.Time
	var data string
	query := `SELECT owner_module_id, scope_kind, legal_entity_id, source_id, source_revision, source_cursor, output_schema_id, output_schema_version, as_of, freshness, data FROM _kora_analytics_module_projection WHERE site = ? AND projection_id = ? AND scope_kind = ? AND legal_entity_id = ? AND source_id = ? ORDER BY generation DESC LIMIT 1`
	err := database.QueryRowContext(ctx, kdb.Rebind(dialect, query), site, definition.ID, string(scope.Kind), scope.LegalEntityID, sourceID).Scan(&owner, &scopeKind, &snapshot.Scope.LegalEntityID, &snapshot.SourceID, &snapshot.SourceRevision, &snapshot.Cursor, &snapshot.Definition.OutputSchema.ID, &schemaVersion, &asOf, &freshness, &data)
	if err != nil {
		return PublishedProjectionSnapshot{}, err
	}
	if schemaVersion < 1 {
		return PublishedProjectionSnapshot{}, fmt.Errorf("published projection snapshot has an invalid output schema version")
	}
	snapshot.Definition.OutputSchema.Version = uint32(schemaVersion)
	if owner != string(definition.OwnerModuleID) || scopeKind != string(scope.Kind) || snapshot.Definition.OutputSchema != definition.OutputSchema {
		return PublishedProjectionSnapshot{}, fmt.Errorf("published projection snapshot does not match its current owner contract")
	}
	snapshot.Site = site
	snapshot.Definition = definition
	snapshot.Scope = scope
	snapshot.AsOf = asOf
	snapshot.Freshness = contract.QueryFreshness(freshness)
	snapshot.Data = json.RawMessage(data)
	if err := snapshot.Validate(); err != nil {
		return PublishedProjectionSnapshot{}, err
	}
	return snapshot, nil
}

// MarkPublishedProjectionStale preserves the most recent output while making
// provider outages and unavailable sources visible to consumers.
func MarkPublishedProjectionStale(ctx context.Context, database *sql.DB, dialect kdb.Dialect, site string, definition contract.ProjectionDefinition, scope contract.BusinessScope, sourceID string) error {
	if ctx == nil || database == nil || dialect == nil || site == "" || definition.Validate() != nil || scope.Validate() != nil || contract.ValidateResourceBusinessScope(scope, definition.ScopePolicy) != nil || sourceID == "" {
		return errors.New("published projection stale marker requires a valid identity and scope")
	}
	if err := BootstrapTables(ctx, database, dialect); err != nil {
		return err
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := lockRebuildStateTx(ctx, tx, dialect, site); err != nil {
		return err
	}
	generation, err := rebuildFenceTx(ctx, tx, dialect, site)
	if err != nil {
		return err
	}
	var revision string
	selectLatest := `SELECT source_revision FROM _kora_analytics_module_projection WHERE site = ? AND projection_id = ? AND owner_module_id = ? AND scope_kind = ? AND legal_entity_id = ? AND source_id = ? ORDER BY generation DESC LIMIT 1`
	err = tx.QueryRowContext(ctx, kdb.Rebind(dialect, selectLatest), site, definition.ID, string(definition.OwnerModuleID), string(scope.Kind), scope.LegalEntityID, sourceID).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		missing := PublishedProjectionSnapshot{Site: site, Definition: definition, Scope: scope, SourceID: sourceID, SourceRevision: "unavailable", AsOf: time.Now().UTC(), Freshness: contract.QueryIncomplete, Data: json.RawMessage(`[]`)}
		if validateErr := missing.Validate(); validateErr != nil {
			return validateErr
		}
		insert := `INSERT INTO _kora_analytics_module_projection (snapshot_key, generation, site, projection_id, owner_module_id, scope_kind, legal_entity_id, source_id, source_revision, source_cursor, output_schema_id, output_schema_version, as_of, freshness, data, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
		upsert := dialect.UpsertClause([]string{"snapshot_key"}, []string{"generation", "freshness", "updated_at"})
		if _, err := tx.ExecContext(ctx, kdb.Rebind(dialect, insert+" "+upsert), missing.key(), generation, site, definition.ID, string(definition.OwnerModuleID), string(scope.Kind), scope.LegalEntityID, sourceID, missing.SourceRevision, missing.Cursor, definition.OutputSchema.ID, definition.OutputSchema.Version, missing.AsOf, string(missing.Freshness), string(missing.Data), time.Now().UTC()); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	update := `UPDATE _kora_analytics_module_projection SET generation = ?, freshness = ?, updated_at = ? WHERE site = ? AND projection_id = ? AND owner_module_id = ? AND scope_kind = ? AND legal_entity_id = ? AND source_id = ? AND source_revision = ?`
	if _, err := tx.ExecContext(ctx, kdb.Rebind(dialect, update), generation, string(contract.QueryStale), time.Now().UTC(), site, definition.ID, string(definition.OwnerModuleID), string(scope.Kind), scope.LegalEntityID, sourceID, revision); err != nil {
		return err
	}
	return tx.Commit()
}
