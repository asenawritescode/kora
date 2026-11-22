//go:build integration

package kernel_test

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
)

// TestRecordCreateEventCarriesStoredChildNames creates a parent with three child lines through the kernel
// command path. The emitted change event must carry each line with the name the line row was stored under,
// so fact consumers can key their line rows by it.
func TestRecordCreateEventCarriesStoredChildNames(t *testing.T) {
	cases := []struct{ driver, env string }{
		{"mysql", "KORA_KERNEL_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_KERNEL_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range cases {
		t.Run(tc.driver, func(t *testing.T) {
			line := &doctype.DocType{Name: "Ledger Line", IsChildTable: true, Fields: []doctype.Field{
				{Fieldname: "product", Fieldtype: "Data"},
				{Fieldname: "quantity", Fieldtype: "Float"},
			}}
			sale := &doctype.DocType{
				Name: "Ledger Sale", Module: "Kernel Test", NamingSeries: "LSALE-.#####",
				Fields: []doctype.Field{{Fieldname: "items", Fieldtype: "Table", Options: "Ledger Line"}},
			}
			perms := []*doctype.Permission{
				{Doctype: "Ledger Sale", Role: "Administrator", Read: true, Create: true},
				{Doctype: "Ledger Line", Role: "Administrator", Read: true, Create: true},
			}
			s := newDisposableFixture(t, tc.driver, tc.env, []*doctype.DocType{sale, line}, perms)
			defer s.DB.Close()
			k := newKernel(s)
			k.Commands = kernel.NewCommandRegistry()

			items := []map[string]any{{"product": "Tea", "quantity": 2}, {"product": "Bread", "quantity": 1}, {"product": "Milk", "quantity": 3}}
			payload, _ := json.Marshal(map[string]any{"doctype": "Ledger Sale", "data": map[string]any{"items": items}})
			mustComplete(t, exec(t, s, k, kernel.Operation{Command: "record.create", Payload: payload, Context: opCtx(s.Name, "Administrator")}))

			var raw []byte
			query := db.Rebind(s.Dialect, "SELECT payload FROM _kora_outbox WHERE aggregate_type = ?")
			if err := s.DB.QueryRow(query, "Ledger Sale").Scan(&raw); err != nil {
				t.Fatalf("read outbox payload: %v", err)
			}
			// The stored payload is the envelope: the record event sits under its own "data" key.
			var event struct {
				Data struct {
					Data struct {
						Items []map[string]any `json:"items"`
					} `json:"data"`
					RecordID string `json:"record_id"`
				} `json:"data"`
			}
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatalf("decode outbox payload: %v", err)
			}
			if len(event.Data.Data.Items) != len(items) {
				t.Fatalf("event carries %d lines, want %d", len(event.Data.Data.Items), len(items))
			}

			childTable := s.Dialect.QuoteIdent(s.Registry.Get("Ledger Sale").RawChildTableName("items"))
			stored := map[string]bool{}
			rows, err := s.DB.Query(db.Rebind(s.Dialect, "SELECT name FROM "+childTable+" WHERE parent = ?"), event.Data.RecordID)
			if err != nil {
				t.Fatalf("read stored lines: %v", err)
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var name string
				if err := rows.Scan(&name); err != nil {
					t.Fatalf("scan stored line: %v", err)
				}
				stored[name] = true
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("iterate stored lines: %v", err)
			}
			if len(stored) != len(items) {
				t.Fatalf("stored lines = %d, want %d", len(stored), len(items))
			}
			for i, got := range event.Data.Data.Items {
				// Decode into a map: encoding/json matches keys case-insensitively, so a struct tag would accept "Name".
				name, _ := got["name"].(string)
				if name == "" {
					t.Fatalf("event line %d has no name key: %v", i, got)
				}
				if !stored[name] {
					t.Fatalf("event line %d name %q is not a stored line", i, name)
				}
			}
		})
	}
}
