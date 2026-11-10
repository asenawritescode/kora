package kernel

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/policy"
)

func TestLookupReceiptByIntentRejectsIncompleteIntent(t *testing.T) {
	execution, err := policy.NewVerifiedExecution("stable-tenant", contract.ActorContext{PrincipalID: "alice", PrincipalType: contract.PrincipalHuman, Site: "tenant-route"})
	if err != nil {
		t.Fatal(err)
	}
	full := ReceiptIntent{Execution: execution, ActorUser: "alice", Command: "sales.invoice.issue", IdempotencyKey: "k"}
	tests := []struct {
		name   string
		mutate func(*ReceiptIntent)
	}{
		{"zero value", func(i *ReceiptIntent) { *i = ReceiptIntent{} }},
		{"no execution", func(i *ReceiptIntent) { i.Execution = policy.Execution{} }},
		{"no actor user", func(i *ReceiptIntent) { i.ActorUser = "" }},
		{"no command", func(i *ReceiptIntent) { i.Command = "" }},
		{"no idempotency key", func(i *ReceiptIntent) { i.IdempotencyKey = "" }},
	}
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	k := New(db.Resolve("mysql"), nil)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			intent := full
			tc.mutate(&intent)
			_, found, cerr := k.LookupReceiptByIntent(context.Background(), database, intent)
			if found || cerr == nil || cerr.Type != contract.CodeValidationFailed {
				t.Fatalf("found=%v err=%v, want validation failure without a query", found, cerr)
			}
		})
	}
}

func TestExecuteBulkWithoutRowsRunsNothing(t *testing.T) {
	tests := []struct {
		name string
		opts BulkOptions
	}{
		{"zero options", BulkOptions{}},
		{"dry run", BulkOptions{DryRun: true}},
		{"upsert key fields", BulkOptions{KeyFields: []string{"code"}}},
	}
	k := New(db.Resolve("mysql"), nil)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := k.ExecuteBulk(context.Background(), nil, nil, Operation{Command: CommandRecordCreate}, tc.opts)
			if len(got) != 0 {
				t.Fatalf("results = %d, want 0", len(got))
			}
		})
	}
}
