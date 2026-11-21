package facts

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// shapeTable stores, per fact model, the fingerprint of the model it was last synced with and the columns
// its tables carry. Its lease lets one replica sync a model at a time: lease_started is the database clock
// when the lease was taken, and a lease is free once that is shapeLeaseSeconds old. Only non-negative
// bounds go to NowMinusSeconds, which every dialect evaluates the same way.
const (
	shapeTable        = "_kora_fact_shape"
	shapeLeaseSeconds = 300
)

// ModelSync brings every fact model's tables in line with its definition. It runs when a tenant runtime
// starts, so each config activation and each process start passes through it. A model whose fingerprint
// changed (or that has no stored shape) gets its tables evolved and one whole-model rebuild queued.
type ModelSync struct {
	DB       *sql.DB
	Dialect  db.Dialect
	Registry *doctype.Registry
	Owner    string // lease owner; unique per process
}

// Sync evolves and enqueues every fact model that changed. A model held by another replica's lease is
// skipped: that replica is syncing it.
func (s *ModelSync) Sync(ctx context.Context) error {
	if err := s.ensure(ctx); err != nil {
		return err
	}
	store := &JobStore{DB: s.DB, Dialect: s.Dialect}
	for _, dt := range s.models() {
		if err := s.syncModel(ctx, store, dt); err != nil {
			return err
		}
	}
	return nil
}

func (s *ModelSync) models() []*doctype.DocType {
	var out []*doctype.DocType
	for _, dt := range s.Registry.All() {
		if dt.Facts != nil {
			out = append(out, dt)
		}
	}
	slices.SortFunc(out, func(a, b *doctype.DocType) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (s *ModelSync) syncModel(ctx context.Context, store *JobStore, dt *doctype.DocType) error {
	fingerprint, want, err := modelShape(dt)
	if err != nil {
		return err
	}
	claimed, err := s.claim(ctx, dt.Name)
	if err != nil || !claimed {
		return err
	}
	stored, err := s.read(ctx, dt.Name)
	if err != nil {
		return err
	}
	action := decideShape(stored, fingerprint, want)
	if action != shapeKeep {
		err = s.evolve(ctx, dt, action, want)
		if err != nil {
			return err
		}
		_, err = store.ReplaceWhole(ctx, dt.Name)
		if err != nil {
			return err
		}
	}
	return s.release(ctx, dt.Name, fingerprint, want)
}

// modelShape returns the fingerprint of a fact model and the columns its tables carry. The fingerprint is
// a hash of the model's JSON, so any change to what it declares changes it.
func modelShape(dt *doctype.DocType) (string, []doctype.FactColumn, error) {
	raw, err := json.Marshal(struct {
		Name  string
		Facts *doctype.FactModel
	}{dt.Name, dt.Facts})
	if err != nil {
		return "", nil, fmt.Errorf("facts: fingerprint %s: %w", dt.Name, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), dt.Facts.Columns(), nil
}

// shapeAction is what a model's tables need when its fingerprint differs from the stored one.
type shapeAction int

const (
	shapeKeep     shapeAction = iota // same fingerprint: nothing to do
	shapeAdd                         // every stored column is still there: add the new ones in place
	shapeRecreate                    // no shape stored, or a column was removed or changed type
)

// storedShape is the shape row of one model. A row with no fingerprint was only claimed, never synced.
type storedShape struct {
	fingerprint string
	columns     []doctype.FactColumn
}

// decideShape compares the stored shape with the wanted one. Only additions can be applied in place, so
// any removed or retyped column recreates the tables; the rebuild that follows refills them from source.
func decideShape(stored *storedShape, fingerprint string, want []doctype.FactColumn) shapeAction {
	if stored == nil {
		return shapeRecreate
	}
	if stored.fingerprint == fingerprint {
		return shapeKeep
	}
	wanted := make(map[string]doctype.FactColumnType, len(want))
	for _, c := range want {
		wanted[c.Name] = c.Type
	}
	for _, c := range stored.columns {
		if t, ok := wanted[c.Name]; !ok || t != c.Type {
			return shapeRecreate
		}
	}
	return shapeAdd
}

// evolve applies action to the fact tables and their summary, with DDL outside any transaction (MySQL DDL
// commits implicitly). A recreate empties the summary too. An additive change keeps it: the new measure
// columns are added, and the refresh of the days the rebuild writes fills them, so dashboards keep their
// numbers meanwhile.
func (s *ModelSync) evolve(ctx context.Context, dt *doctype.DocType, action shapeAction, want []doctype.FactColumn) error {
	if action == shapeRecreate {
		return s.recreate(ctx, dt)
	}
	return s.addColumns(ctx, dt, want)
}

// recreate drops the fact and summary tables and creates them again, empty; the rebuild refills them.
func (s *ModelSync) recreate(ctx context.Context, dt *doctype.DocType) error {
	q := s.Dialect.QuoteIdent
	drops := []string{"DROP TABLE IF EXISTS " + q(summaryTable(dt)), "DROP TABLE IF EXISTS " + q(dt.Facts.TableName(dt))}
	if dt.Facts.Lines != "" {
		drops = append(drops, "DROP TABLE IF EXISTS "+q(dt.Facts.LineTableName(dt)))
	}
	if err := s.execAll(ctx, drops); err != nil {
		return err
	}
	return s.execAll(ctx, append(DDL(s.Dialect, dt), summaryDDL(s.Dialect, dt)))
}

// addColumns adds the columns of want that the fact tables and the summary lack. The summary holds one column
// per measure, not per dimension. CREATE IF NOT EXISTS covers a summary that was never written.
func (s *ModelSync) addColumns(ctx context.Context, dt *doctype.DocType, want []doctype.FactColumn) error {
	tables := []string{dt.Facts.TableName(dt)}
	if dt.Facts.Lines != "" {
		tables = append(tables, dt.Facts.LineTableName(dt))
	}
	for _, table := range tables {
		if err := s.addAll(ctx, table, want); err != nil {
			return err
		}
	}
	if err := s.execAll(ctx, []string{summaryDDL(s.Dialect, dt)}); err != nil {
		return err
	}
	var measures []doctype.FactColumn
	for _, c := range want {
		if c.Type == doctype.FactColumnDecimal {
			measures = append(measures, c)
		}
	}
	return s.addAll(ctx, summaryTable(dt), measures)
}

func (s *ModelSync) addAll(ctx context.Context, table string, cols []doctype.FactColumn) error {
	for _, c := range cols {
		if err := s.addMissingColumn(ctx, table, c); err != nil {
			return err
		}
	}
	return nil
}

// addMissingColumn adds c to table unless a probe finds it. A nullable column needs no backfill: the
// rebuild that follows writes every row, and until then the model reports as building.
func (s *ModelSync) addMissingColumn(ctx context.Context, table string, c doctype.FactColumn) error {
	probe := fmt.Sprintf("SELECT %s FROM %s WHERE 1 = 0", s.Dialect.QuoteIdent(c.Name), s.Dialect.QuoteIdent(table)) //nolint:gosec // identifiers only, quoted by the dialect
	if _, err := s.DB.ExecContext(ctx, probe); err == nil {
		return nil
	}
	return s.execAll(ctx, []string{s.Dialect.AddColumn(table, &doctype.Field{Fieldname: c.Name, Fieldtype: factFieldtype(c)})})
}

func (s *ModelSync) execAll(ctx context.Context, stmts []string) error {
	for _, stmt := range stmts {
		if _, err := s.DB.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("facts: evolve fact tables: %w", err)
		}
	}
	return nil
}

// ensure creates the shape table. DDL runs outside any transaction.
func (s *ModelSync) ensure(ctx context.Context) error {
	d := s.Dialect
	cols := []string{
		columnDef(d, "model", "Data"), columnDef(d, "fingerprint", "Data"), columnDef(d, "shape_columns", "Text"),
		columnDef(d, "lease_owner", "Data"), columnDef(d, "lease_started", "Datetime"),
		fmt.Sprintf("PRIMARY KEY (%s)", d.QuoteIdent("model")),
	}
	ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n  %s\n)", d.QuoteIdent(shapeTable), strings.Join(cols, ",\n  "))
	if suffix := d.TableSuffix(); suffix != "" {
		ddl += " " + suffix
	}
	if err := s.execAll(ctx, []string{ddl}); err != nil {
		return err
	}
	// The refresher and the drift check read the dirty table before any event has reached the builder.
	return ensureDirtyTable(ctx, s.DB, d)
}

// claim takes the model's shape lease, creating its row first. A lease older than shapeLeaseSeconds is taken over.
func (s *ModelSync) claim(ctx context.Context, model string) (bool, error) {
	table := s.Dialect.QuoteIdent(shapeTable)
	ago := s.Dialect.NowMinusSeconds()
	insert := fmt.Sprintf("INSERT INTO %s (model, fingerprint, shape_columns, lease_owner, lease_started) VALUES (?, '', '', '', %s)", table, ago)
	if _, err := s.DB.ExecContext(ctx, db.Rebind(s.Dialect, insert), model, shapeLeaseSeconds); err != nil && !s.Dialect.IsUniqueViolation(err) {
		return false, fmt.Errorf("facts: create shape row of %s: %w", model, err)
	}
	take := fmt.Sprintf("UPDATE %s SET lease_owner = ?, lease_started = %s WHERE model = ? AND lease_started <= %s", table, s.Dialect.NowMinusSeconds(), ago)
	changed, err := s.execRows(ctx, take, []any{s.Owner, 0, model, shapeLeaseSeconds})
	if err != nil {
		return false, fmt.Errorf("facts: lease shape of %s: %w", model, err)
	}
	return changed == 1, nil
}

// read returns the stored shape of model, or nil when the model was never synced.
func (s *ModelSync) read(ctx context.Context, model string) (*storedShape, error) {
	query := fmt.Sprintf("SELECT fingerprint, shape_columns FROM %s WHERE model = ?", s.Dialect.QuoteIdent(shapeTable))
	var fingerprint, columns string
	if err := s.DB.QueryRowContext(ctx, db.Rebind(s.Dialect, query), model).Scan(&fingerprint, &columns); err != nil {
		return nil, fmt.Errorf("facts: read shape of %s: %w", model, err)
	}
	if fingerprint == "" {
		return nil, nil
	}
	var cols []doctype.FactColumn
	if err := json.Unmarshal([]byte(columns), &cols); err != nil {
		return nil, fmt.Errorf("facts: decode shape of %s: %w", model, err)
	}
	return &storedShape{fingerprint: fingerprint, columns: cols}, nil
}

// release stores the synced shape and drops the lease, fenced by owner. A lost lease is an error: the
// model was not recorded, so the next start syncs it again.
func (s *ModelSync) release(ctx context.Context, model, fingerprint string, want []doctype.FactColumn) error {
	raw, err := json.Marshal(want)
	if err != nil {
		return fmt.Errorf("facts: encode shape of %s: %w", model, err)
	}
	// The lease is released by moving its start a full lease length back, so the model is free at once.
	query := fmt.Sprintf("UPDATE %s SET fingerprint = ?, shape_columns = ?, lease_owner = '', lease_started = %s WHERE model = ? AND lease_owner = ?",
		s.Dialect.QuoteIdent(shapeTable), s.Dialect.NowMinusSeconds())
	changed, err := s.execRows(ctx, query, []any{fingerprint, string(raw), shapeLeaseSeconds, model, s.Owner})
	if err != nil {
		return fmt.Errorf("facts: store shape of %s: %w", model, err)
	}
	if changed != 1 {
		return errors.New("facts: shape lease lost for " + model)
	}
	return nil
}

func (s *ModelSync) execRows(ctx context.Context, query string, args []any) (int64, error) {
	result, err := s.DB.ExecContext(ctx, db.Rebind(s.Dialect, query), args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
