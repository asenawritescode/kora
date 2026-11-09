package contract

import (
	"encoding/json"
	"testing"
	"time"
)

func TestExportedContractRoundTrips(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		in   any
		out  any
	}{
		{
			name: "actor_context",
			in: ActorContext{
				PrincipalID:     "usr-1",
				PrincipalType:   PrincipalHuman,
				Site:            "acme.example.com",
				Roles:           []string{"Accounts User"},
				AuthenticatedAt: now,
				AuthSessionID:   "sess-1",
				DelegationID:    "del-1",
			},
			out: &ActorContext{},
		},
		{
			name: "command_envelope",
			in: CommandEnvelope{
				Type:           "document.submit",
				Version:        CurrentVersion,
				ID:             NewCommandID(),
				Site:           "acme.example.com",
				Actor:          ActorContext{PrincipalID: "usr-1", PrincipalType: PrincipalHuman, Site: "acme.example.com"},
				CorrelationID:  "req-123",
				IdempotencyKey: "client-op-456",
				Deadline:       now,
				Data:           MustEncodeData(map[string]any{"doctype": "Sales Invoice"}),
			},
			out: &CommandEnvelope{},
		},
		{
			name: "event_envelope",
			in: EventEnvelope{
				ID:            NewEventID(),
				Type:          "kora.document.sales_invoice.submitted",
				Version:       CurrentVersion,
				Source:        "kora.kernel",
				Site:          "acme.example.com",
				AggregateType: "Sales Invoice",
				AggregateID:   "SINV-0001",
				OccurredAt:    now,
				CorrelationID: "req-123",
				CausationID:   "cmd-456",
				Data:          MustEncodeData(map[string]any{"status": "submitted"}),
			},
			out: &EventEnvelope{},
		},
		{
			name: "command_result",
			in: CommandResult{
				OperationID:   "op-123",
				CorrelationID: "req-123",
				Status:        StatusCompleted,
				Data:          MustEncodeData(map[string]any{"ok": true}),
				Error:         &Error{Type: CodeInternal, Message: "boom"},
				Replayed:      true,
			},
			out: &CommandResult{},
		},
		{
			name: "task_receipt",
			in: TaskReceipt{
				OperationID:   "op-123",
				CorrelationID: "req-123",
				Status:        StatusAccepted,
				AcceptedAt:    now,
			},
			out: &TaskReceipt{},
		},
		{
			name: "delivery",
			in: Delivery{
				ID:      "del-1",
				Type:    "kora.document.sales_invoice.submitted",
				Site:    "acme.example.com",
				Data:    MustEncodeData(map[string]any{"status": "submitted"}),
				Attempt: 1,
			},
			out: &Delivery{},
		},
		{
			name: "offline_operation",
			in: OfflineOperation{
				ID:            "op-1",
				DeviceID:      "dev-1",
				BranchID:      "branch-1",
				Site:          "acme.example.com",
				EntityType:    "Sales Invoice",
				EntityID:      "SINV-0001",
				OperationType: "update",
				BaseVersion:   3,
				OccurredAt:    now,
				SchemaVersion: "2026.08",
				Status:        OfflineOperationQueued,
				Payload:       json.RawMessage(`{"status":"draft"}`),
				Metadata:      map[string]string{"source": "pos"},
			},
			out: &OfflineOperation{},
		},
		{
			name: "offline_conflict",
			in: OfflineConflict{
				ID:             "conf-1",
				OperationID:    "op-1",
				DeviceID:       "dev-1",
				BranchID:       "branch-1",
				Site:           "acme.example.com",
				EntityType:     "Sales Invoice",
				EntityID:       "SINV-0001",
				ReasonCode:     CodeConflict,
				ResolutionMode: "retry",
				ServerVersion:  4,
				RecordedAt:     now,
				Snapshot:       json.RawMessage(`{"status":"submitted"}`),
			},
			out: &OfflineConflict{},
		},
		{
			name: "offline_tombstone",
			in: OfflineTombstone{
				ID:            "ts-1",
				DeviceID:      "dev-1",
				BranchID:      "branch-1",
				Site:          "acme.example.com",
				EntityType:    "Sales Invoice",
				EntityID:      "SINV-0001",
				DeletedAt:     now,
				RetainUntil:   now.Add(time.Hour),
				SchemaVersion: "2026.08",
			},
			out: &OfflineTombstone{},
		},
		{
			name: "sync_cursor",
			in: SyncCursor{
				Token:    "token-1",
				BranchID: "branch-1",
				DeviceID: "dev-1",
				Version:  3,
				At:       now,
			},
			out: &SyncCursor{},
		},
		{
			name: "offline_sync_batch",
			in: OfflineSyncBatch{
				SchemaVersion: "2026.08",
				Gate:          OfflineSchemaGateAccepted,
				Operations: []OfflineOperation{{
					ID:            "op-1",
					DeviceID:      "dev-1",
					BranchID:      "branch-1",
					Site:          "acme.example.com",
					EntityType:    "Sales Invoice",
					OperationType: "update",
					BaseVersion:   3,
					OccurredAt:    now,
					SchemaVersion: "2026.08",
					Status:        OfflineOperationQueued,
				}},
				Conflicts:  []OfflineConflict{{ID: "conf-1", ResolutionMode: "retry"}},
				Cursor:     SyncCursor{Token: "token-1", BranchID: "branch-1", DeviceID: "dev-1", Version: 3, At: now},
				NextCursor: SyncCursor{Token: "token-2", BranchID: "branch-1", DeviceID: "dev-1", Version: 4, At: now.Add(time.Minute)},
			},
			out: &OfflineSyncBatch{},
		},
		{
			name: "tool_descriptor",
			in: ToolDescriptor{
				ID:                      "tool-1",
				Source:                  "registry",
				Name:                    "document.create",
				Description:             "Create a document",
				InputSchema:             map[string]any{"type": "object"},
				SafetyLevel:             ToolSafetyStandard,
				RequiresConfirmation:    true,
				RequiresRecentAuth:      false,
				ChannelAllowlist:        []string{"chat"},
				ArgumentContractVersion: ToolArgumentVersion("1"),
				Operation:               "create",
				Doctype:                 "Sales Invoice",
				DoctypeLabel:            "Sales Invoice",
				TitleField:              "name",
				SearchFields:            []string{"name"},
				SortField:               "modified",
				SortOrder:               "desc",
				FieldHints: []FieldHint{{
					Name:      "name",
					Fieldtype: "Data",
					Writable:  true,
				}},
				SystemFields: []SystemFieldHint{{
					Name:      "name",
					Fieldtype: "Data",
					Writable:  false,
				}},
			},
			out: &ToolDescriptor{},
		},
		{
			name: "tool_catalog",
			in: ToolCatalog{
				Version: "1",
				Tools: []ToolDescriptor{{
					ID:        "tool-1",
					Source:    "registry",
					Name:      "document.create",
					Operation: "create",
				}},
			},
			out: &ToolCatalog{},
		},
		{
			name: "usage_event",
			in: UsageEvent{
				ID:           "usage-1",
				Site:         "acme.example.com",
				Organization: "org-1",
				UserID:       "usr-1",
				Model:        "gpt-5",
				Provider:     "openai",
				RunID:        "run-1",
				StepID:       "step-1",
				Channel:      "chat",
				Attempt:      2,
				Status:       "completed",
				Tokens:       map[UsageClass]int64{UsageClassInput: 10, UsageClassOutput: 3},
				LatencyMs:    123,
				OccurredAt:   now,
				RetryOf:      "usage-0",
				Attribution:  map[string]string{"project": "core"},
			},
			out: &UsageEvent{},
		},
		{
			name: "approval",
			in: Approval{
				ID:                "appr-1",
				Site:              "acme.example.com",
				OperationID:       "op-1",
				Actor:             ActorContext{PrincipalID: "usr-1", PrincipalType: PrincipalHuman, Site: "acme.example.com"},
				ToolName:          "document.create",
				State:             ApprovalGranted,
				TargetFingerprint: "fingerprint-1",
				ArgumentHash:      "hash-1",
				RecordVersion:     2,
				RequestedAt:       now,
				ExpiresAt:         now.Add(time.Hour),
				GrantedAt:         now.Add(5 * time.Minute),
				GrantedBy:         "usr-2",
				AuthSessionID:     "sess-1",
			},
			out: &Approval{},
		},
		{
			name: "cursor",
			in: Cursor{
				Token:   "token-1",
				Version: 4,
				Nonce:   "nonce-1",
				At:      now,
			},
			out: &Cursor{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := json.Unmarshal(b, tc.out); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got, want := string(b), string(mustJSON(t, tc.out)); got != want {
				t.Fatalf("round-trip JSON mismatch:\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	return b
}
