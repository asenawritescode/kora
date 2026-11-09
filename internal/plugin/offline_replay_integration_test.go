package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	_ "github.com/mattn/go-sqlite3"
)

// sqliteOfflineProvider exercises the same provider contract used by internal
// and external modules, backed by a durable fact table and append-only change
// log rather than an in-memory fake.
type sqliteOfflineProvider struct{ db *sql.DB }

func (p sqliteOfflineProvider) ExecutePublishedQueryForActor(ctx context.Context, execution contract.ModuleExecution, query contract.PublishedQuery, actor PublishedQueryActorScope, _ json.RawMessage, _ string, limit uint16) (contract.QueryResult, error) {
	if !execution.Verified() || execution.Scope != actor.BusinessScope || !actor.OwnerOnly || actor.OwnerField != query.OwnerField || !query.OfflineEligible || query.OfflineFactKey == "" {
		return contract.QueryResult{}, fmt.Errorf("invalid offline snapshot scope")
	}
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return contract.QueryResult{}, err
	}
	defer tx.Rollback()
	var high uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM offline_changes WHERE tenant_id=? AND entity_id=?`, execution.Scope.TenantID, execution.Scope.LegalEntityID).Scan(&high); err != nil {
		return contract.QueryResult{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT data FROM offline_facts WHERE tenant_id=? AND entity_id=? AND owner_id=? ORDER BY fact_key LIMIT ?`, execution.Scope.TenantID, execution.Scope.LegalEntityID, actor.SubjectID, limit)
	if err != nil {
		return contract.QueryResult{}, err
	}
	defer rows.Close()
	data := make([]json.RawMessage, 0)
	for rows.Next() {
		var row []byte
		if err := rows.Scan(&row); err != nil {
			return contract.QueryResult{}, err
		}
		data = append(data, json.RawMessage(row))
	}
	if err := rows.Err(); err != nil {
		return contract.QueryResult{}, err
	}
	if err := rows.Close(); err != nil {
		return contract.QueryResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return contract.QueryResult{}, err
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return contract.QueryResult{}, err
	}
	return contract.QueryResult{Data: encoded, SnapshotToken: fmt.Sprintf("snapshot:%d", high), SourceCursor: fmt.Sprint(high), SourceRevision: fmt.Sprintf("revision:%d", high), AsOf: time.Now().UTC(), Freshness: contract.QueryFresh}, nil
}

func (p sqliteOfflineProvider) ExecutePublishedQuery(context.Context, contract.ModuleExecution, contract.PublishedQuery, json.RawMessage, string, uint16) (contract.QueryResult, error) {
	return contract.QueryResult{}, fmt.Errorf("offline query requires actor scope")
}

func (p sqliteOfflineProvider) ReplayPublishedQuery(context.Context, contract.ModuleExecution, contract.PublishedQuery, json.RawMessage, string, string, string, uint16) (contract.QueryReplayPage, error) {
	return contract.QueryReplayPage{}, fmt.Errorf("offline replay requires actor scope")
}

func (p sqliteOfflineProvider) ReplayPublishedQueryForActor(ctx context.Context, execution contract.ModuleExecution, query contract.PublishedQuery, actor PublishedQueryActorScope, _ json.RawMessage, snapshot, after, through, replay string, limit uint16) (contract.QueryReplayPage, error) {
	if !execution.Verified() || execution.Scope != actor.BusinessScope || !actor.OwnerOnly || actor.OwnerField != query.OwnerField || !query.OfflineEligible || query.OfflineFactKey == "" {
		return contract.QueryReplayPage{}, fmt.Errorf("invalid offline replay scope")
	}
	var snapshotHigh uint64
	if _, err := fmt.Sscanf(snapshot, "snapshot:%d", &snapshotHigh); err != nil || fmt.Sprint(snapshotHigh) != after {
		return contract.QueryReplayPage{}, fmt.Errorf("snapshot cursor mismatch")
	}
	var start uint64
	if replay == "" {
		start = snapshotHigh
	} else if _, err := fmt.Sscan(replay, &start); err != nil {
		return contract.QueryReplayPage{}, err
	}
	var high uint64
	if through == "" {
		if err := p.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM offline_changes WHERE tenant_id=? AND entity_id=?`, execution.Scope.TenantID, execution.Scope.LegalEntityID).Scan(&high); err != nil {
			return contract.QueryReplayPage{}, err
		}
	} else if _, err := fmt.Sscan(through, &high); err != nil {
		return contract.QueryReplayPage{}, err
	}
	rows, err := p.db.QueryContext(ctx, `SELECT seq, fact_key, deleted, data FROM offline_changes WHERE tenant_id=? AND entity_id=? AND owner_id=? AND seq>? AND seq<=? ORDER BY seq LIMIT ?`, execution.Scope.TenantID, execution.Scope.LegalEntityID, actor.SubjectID, start, high, int(limit)+1)
	if err != nil {
		return contract.QueryReplayPage{}, err
	}
	defer rows.Close()
	changes := make([]contract.QueryReplayChange, 0, limit)
	var last uint64 = start
	for rows.Next() {
		var seq uint64
		var key string
		var deleted bool
		var data []byte
		if err := rows.Scan(&seq, &key, &deleted, &data); err != nil {
			return contract.QueryReplayPage{}, err
		}
		if len(changes) == int(limit) {
			break
		}
		change := contract.QueryReplayChange{FactKey: key, Deleted: deleted}
		if !deleted {
			change.Data = json.RawMessage(data)
		}
		changes = append(changes, change)
		last = seq
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return contract.QueryReplayPage{}, err
	}
	if err := rows.Close(); err != nil {
		return contract.QueryReplayPage{}, err
	}
	page := contract.QueryReplayPage{Changes: changes, ThroughCursor: fmt.Sprint(high), SourceRevision: fmt.Sprintf("revision:%d", high), AsOf: time.Now().UTC(), Freshness: contract.QueryFresh}
	if last < high {
		var remaining int
		if err := p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM offline_changes WHERE tenant_id=? AND entity_id=? AND owner_id=? AND seq>? AND seq<=?`, execution.Scope.TenantID, execution.Scope.LegalEntityID, actor.SubjectID, last, high).Scan(&remaining); err != nil {
			return contract.QueryReplayPage{}, err
		}
		if remaining > 0 {
			page.NextCursor = fmt.Sprint(last)
		}
	}
	return page, nil
}

func TestOfflineReplayHasNoGapAndPreservesActorEntityScopeForProviderKinds(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderInternal, ProviderExternal} {
		t.Run(string(kind), func(t *testing.T) {
			db, err := sql.Open("sqlite3", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			_, err = db.Exec(`CREATE TABLE offline_facts(tenant_id TEXT, entity_id TEXT, fact_key TEXT, owner_id TEXT, data TEXT, PRIMARY KEY(tenant_id, entity_id, fact_key));
CREATE TABLE offline_changes(seq INTEGER PRIMARY KEY AUTOINCREMENT, tenant_id TEXT, entity_id TEXT, fact_key TEXT, owner_id TEXT, deleted BOOLEAN, data TEXT);`)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
			if _, err := db.Exec(`INSERT INTO offline_facts VALUES(?,?,?,?,?)`, scope.TenantID, scope.LegalEntityID, "a-1", "alice", `{"id":"a-1","owner_id":"alice","amount":1}`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO offline_changes(tenant_id,entity_id,fact_key,owner_id,deleted,data) VALUES(?,?,?,?,?,?)`, scope.TenantID, scope.LegalEntityID, "a-1", "alice", false, `{"id":"a-1","owner_id":"alice","amount":1}`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO offline_facts VALUES(?,?,?,?,?)`, scope.TenantID, scope.LegalEntityID, "b-1", "bob", `{"id":"b-1","owner_id":"bob","amount":9}`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO offline_facts VALUES(?,?,?,?,?)`, scope.TenantID, "entity-b", "a-2", "alice", `{"id":"a-2","owner_id":"alice","amount":8}`); err != nil {
				t.Fatal(err)
			}

			in, out := contract.SchemaRef{ID: "offline.input", Version: 1}, contract.SchemaRef{ID: "offline.output", Version: 1}
			query := contract.PublishedQuery{OwnerModuleID: "sales.orders", Name: "list", Capability: "sales.read", Operation: "list", InputSchema: in, OutputSchema: out, ScopePolicy: contract.ScopeEntityOwned, AllowedFields: []string{"id", "owner_id", "amount"}, PageLimit: 10, OfflineEligible: true, OfflineFactKey: "id", OwnerField: "owner_id"}
			module := contract.ModuleDescriptor{ID: query.OwnerModuleID, Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: query.Capability, Contract: out, Operations: []contract.OperationSpec{{Name: query.Operation, Input: in, Output: out, Effect: contract.EffectQuery}}}}, Queries: []contract.PublishedQuery{query}}
			provider := sqliteOfflineProvider{db: db}
			registration := ProviderRegistration{TenantID: scope.TenantID, ProviderID: "sqlite." + string(kind), OwnerModuleID: module.ID, Kind: kind, Offers: module.Provides, Queries: map[string]PublishedQueryExecutor{"list": provider}, ValidateSchema: func(_ contract.SchemaRef, data []byte) error {
				if !json.Valid(data) {
					return fmt.Errorf("invalid JSON")
				}
				return nil
			}}
			plan, err := ResolveModules(scope, []contract.ModuleDescriptor{module}, []ProviderRegistration{registration}, nil, 7)
			if err != nil {
				t.Fatal(err)
			}
			actor := PublishedQueryActorScope{BusinessScope: scope, SubjectID: "alice", OwnerOnly: true, OwnerField: "owner_id"}
			input := json.RawMessage(`{}`)
			snapshot, err := plan.ExecutePublishedQueryForActor(ctx, scope, module.ID, query.Name, actor, input, "", 10)
			if err != nil {
				t.Fatal("snapshot:", err)
			}
			if snapshot.SourceCursor != "1" || string(snapshot.Data) != `[{"id":"a-1","owner_id":"alice","amount":1}]` {
				t.Fatalf("scoped snapshot=%s cursor=%s", snapshot.Data, snapshot.SourceCursor)
			}

			// Commit a post-snapshot insert, two out-of-scope changes, and a delete.
			for _, change := range []struct {
				entity, key, owner string
				deleted            bool
				data               string
			}{
				{"entity-a", "a-3", "alice", false, `{"id":"a-3","owner_id":"alice","amount":3}`},
				{"entity-a", "b-2", "bob", false, `{"id":"b-2","owner_id":"bob","amount":7}`},
				{"entity-b", "a-4", "alice", false, `{"id":"a-4","owner_id":"alice","amount":4}`},
				{"entity-a", "a-1", "alice", true, ""},
			} {
				if !change.deleted {
					if _, err := db.Exec(`INSERT INTO offline_facts VALUES(?,?,?,?,?)`, scope.TenantID, change.entity, change.key, change.owner, change.data); err != nil {
						t.Fatal(err)
					}
				} else if _, err := db.Exec(`DELETE FROM offline_facts WHERE tenant_id=? AND entity_id=? AND fact_key=?`, scope.TenantID, change.entity, change.key); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO offline_changes(tenant_id,entity_id,fact_key,owner_id,deleted,data) VALUES(?,?,?,?,?,?)`, scope.TenantID, change.entity, change.key, change.owner, change.deleted, change.data); err != nil {
					t.Fatal(err)
				}
			}

			first, err := plan.ReplayPublishedQueryForActor(ctx, scope, module.ID, query.Name, actor, input, snapshot.SnapshotToken, snapshot.SourceCursor, "", "", 1)
			if err != nil {
				t.Fatal("first replay page:", err)
			}
			if first.ThroughCursor != "5" || len(first.Changes) != 1 || first.Changes[0].FactKey != "a-3" || first.NextCursor == "" {
				t.Fatalf("first replay page=%+v", first)
			}
			second, err := plan.ReplayPublishedQueryForActor(ctx, scope, module.ID, query.Name, actor, input, snapshot.SnapshotToken, snapshot.SourceCursor, first.ThroughCursor, first.NextCursor, 1)
			if err != nil {
				t.Fatal("second replay page:", err)
			}
			if second.ThroughCursor != first.ThroughCursor || len(second.Changes) != 1 || second.Changes[0].FactKey != "a-1" || !second.Changes[0].Deleted || second.NextCursor != "" {
				t.Fatalf("second replay page=%+v", second)
			}
		})
	}
}
