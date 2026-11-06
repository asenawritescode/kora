//go:build integration
// +build integration

package kernel_test

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/schema"
)

const receiveBinYAML = `
name: bin.receive
namespace: warehouse
version: 1
input:
  record: Bin
require:
  - lock:
      record: Task
      name: $input.anchor
transaction:
  - as: bin
    ensure:
      record: Bin
      where:
        bin_key: $input.bin_key
      values:
        qty: "0"
  - update:
      record: Bin
      name: $steps.bin.name
      adjust:
        qty: { increase: $input.qty }
`

// TestEnsureStepCreatesMissingRecordOnceUnderConcurrency proves an ensure step
// creates the target when absent, reuses it afterwards, and that concurrent
// first writers serialized by a lock guard produce exactly one record.
func TestEnsureStepCreatesMissingRecordOnceUnderConcurrency(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	bin := &doctype.DocType{Name: "Bin", Fields: []doctype.Field{
		{Fieldname: "bin_key", Fieldtype: "Data", Reqd: true},
		{Fieldname: "qty", Fieldtype: "Float", Default: "0"},
	}}
	reg := doctype.NewRegistry()
	reg.LoadFull([]*doctype.DocType{s.Registry.Get("Task"), bin}, []*doctype.Role{{Name: "Administrator"}}, []*doctype.Permission{
		{Doctype: "Task", Role: "Administrator", Read: true, Write: true, Create: true},
		{Doctype: "Bin", Role: "Administrator", Read: true, Write: true, Create: true},
	})
	if err := schema.MigrateSiteFromRegistry(s.DB, s.Name, reg, s.Dialect); err != nil {
		t.Fatal(err)
	}
	s.Registry = reg
	k := newKernel(s)
	k.Commands = kernel.NewCommandRegistry()
	mustRegister(t, k, receiveBinYAML)

	mustComplete(t, exec(t, s, k, createOp(map[string]any{"title": "Anchor", "serial": "ANCHOR-1"})))
	var anchor string
	s.DB.QueryRow("SELECT name FROM `tabTask` WHERE serial='ANCHOR-1'").Scan(&anchor)
	receive := func(key string, qty int) contract.CommandResult {
		payload, _ := json.Marshal(map[string]any{"data": map[string]any{"bin_key": key, "qty": qty, "anchor": anchor}})
		return exec(t, s, k, kernel.Operation{Command: "warehouse.bin.receive", Payload: payload, Context: opCtx(s.Name, "Administrator")})
	}
	qty := func(key string) float64 {
		var v float64
		if err := s.DB.QueryRow("SELECT qty FROM `tabBin` WHERE bin_key=?", key).Scan(&v); err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		return v
	}

	mustComplete(t, receive("A", 5))
	if got := qty("A"); got != 5 || s.count(t, "SELECT COUNT(*) FROM `tabBin`") != 1 {
		t.Fatalf("first receipt qty=%v", got)
	}
	mustComplete(t, receive("A", 3))
	if got := qty("A"); got != 8 || s.count(t, "SELECT COUNT(*) FROM `tabBin` WHERE bin_key='A'") != 1 {
		t.Fatalf("second receipt qty=%v", got)
	}

	const writers = 6
	var wg sync.WaitGroup
	results := make([]contract.CommandResult, writers)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = receive("B", 2)
		}(i)
	}
	wg.Wait()
	for i, res := range results {
		if res.Status != contract.StatusCompleted {
			t.Fatalf("writer %d: %s %+v", i, res.Status, res.Error)
		}
	}
	if n := s.count(t, "SELECT COUNT(*) FROM `tabBin` WHERE bin_key='B'"); n != 1 {
		t.Fatalf("concurrent first receipts created %d rows, want 1", n)
	}
	if got, want := qty("B"), float64(2*writers); got != want {
		t.Fatalf("concurrent qty = %v, want %v", got, want)
	}
}
