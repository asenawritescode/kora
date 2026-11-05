package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/print"
	"github.com/asenawritescode/kora/query"
)

// Fixtures bypass package file-type validation (#285) and feed the snapshot merge directly.
func cmdRaw(ns, name string, version int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"name":%q,"namespace":%q,"version":%d,"input":{"record":"Item"},"transaction":[{"create":{"record":"Item","values":{"title":"x"}}}]}`, name, ns, version))
}

func queryRaw(t *testing.T, owner, name, fields string) json.RawMessage {
	t.Helper()
	raws, err := query.Encode([]*query.Definition{{Name: name, ModuleOwnerID: owner, Capability: "c", Operation: name, Source: "Item", Scope: "tenant", Permission: "read", Fields: []string{fields}}})
	if err != nil {
		t.Fatal(err)
	}
	return raws[0]
}

func printRaw(name, text string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"name":%q,"for":{"operation":"a~b~c"},"paper":"thermal_58","text":%q}`, name, text))
}

func configSnapshot(t *testing.T, commands, queries, formats []json.RawMessage) *doctype.ConfigSnapshot {
	return &doctype.ConfigSnapshot{
		DocTypes: []*doctype.DocType{{Name: "Item", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}}},
		Commands: commands, Queries: queries, PrintFormats: formats,
	}
}

func rawKeys(t *testing.T, s *doctype.ConfigSnapshot) (cmds, qs, pfs []string) {
	t.Helper()
	reg := doctype.NewRegistry()
	for _, dt := range s.DocTypes {
		reg.Register(dt)
	}
	for _, raw := range s.Commands {
		c, err := kernel.ParseCommandResourceJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, c.Namespace+"."+c.Name+fmt.Sprintf("@%d", c.Version))
	}
	for _, raw := range s.Queries {
		q, err := query.Parse(raw, reg)
		if err != nil {
			t.Fatal(err)
		}
		qs = append(qs, q.ModuleOwnerID+"/"+q.Name+"@"+q.Fields[0])
	}
	for _, raw := range s.PrintFormats {
		f, err := print.Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		pfs = append(pfs, f.Name+"@"+f.Text)
	}
	return
}

func TestMergeManagementPackageSnapshotMergesCommandsQueriesAndPrintFormats(t *testing.T) {
	tenant := func() *doctype.ConfigSnapshot {
		return configSnapshot(t, []json.RawMessage{cmdRaw("own", "keep", 1), cmdRaw("shop", "sell", 1)},
			[]json.RawMessage{queryRaw(t, "own.core", "keep", "name"), queryRaw(t, "shop.core", "items", "name")},
			[]json.RawMessage{printRaw("keep", "k"), printRaw("receipt", "old")})
	}
	pkg := configSnapshot(t, []json.RawMessage{cmdRaw("shop", "sell", 2), cmdRaw("shop", "refund", 1)},
		[]json.RawMessage{queryRaw(t, "shop.core", "items", "title"), queryRaw(t, "shop.core", "stock", "name")},
		[]json.RawMessage{printRaw("receipt", "new"), printRaw("invoice", "i")})
	tests := []struct {
		name         string
		current      *doctype.ConfigSnapshot
		runs         int
		cmds, qs, pf string
	}{
		{"new package adds all kinds", configSnapshot(t, nil, nil, nil), 1,
			"shop.sell@2,shop.refund@1", "shop.core/items@title,shop.core/stock@name", "receipt@new,invoice@i"},
		{"reinstall replaces without duplicating and keeps tenant items", tenant(), 2,
			"own.keep@1,shop.sell@2,shop.refund@1", "own.core/keep@name,shop.core/items@title,shop.core/stock@name", "keep@k,receipt@new,invoice@i"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cur := tc.current
			for i := 0; i < tc.runs; i++ {
				var err error
				if cur, err = mergeManagementPackageSnapshot(cur, pkg); err != nil {
					t.Fatal(err)
				}
			}
			c, q, p := rawKeys(t, cur)
			if got := strings.Join(c, ",") + "|" + strings.Join(q, ",") + "|" + strings.Join(p, ","); got != tc.cmds+"|"+tc.qs+"|"+tc.pf {
				t.Fatalf("got %s", got)
			}
		})
	}
}

func TestMergeManagementPackageSnapshotRejectsUndecodableConfig(t *testing.T) {
	bad := json.RawMessage(`{"bogus":`)
	for name, in := range map[string]*doctype.ConfigSnapshot{
		"command":      configSnapshot(t, []json.RawMessage{bad}, nil, nil),
		"query":        configSnapshot(t, nil, []json.RawMessage{bad}, nil),
		"print format": configSnapshot(t, nil, nil, []json.RawMessage{bad}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := mergeManagementPackageSnapshot(configSnapshot(t, nil, nil, nil), in); err == nil {
				t.Fatal("undecodable entry accepted")
			}
		})
	}
}
