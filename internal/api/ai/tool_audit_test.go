package ai

import (
	"context"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
)

// TestIsEmptyFindReplyRecognisesWriterOutput checks the zero-row reply recogniser against what the writers return,
// and against replies that have rows.
func TestIsEmptyFindReplyRecognisesWriterOutput(t *testing.T) {
	bill := &doctype.DocType{Name: "Supplier Bill"}
	noneAsked := noRowsText(context.Background(), nil, bill, policy.RowScope{}, titleResolution{})
	closest := []orm.TitleMatch{{Title: "Mr. Ouma Son"}}
	for _, tc := range []struct {
		name  string
		reply string
		want  bool
	}{
		{"nothing asked", noneAsked, true},
		{"link matches no record", noTargetMatchText("Supplier", "zzz", nil), true},
		{"link matches no record, closest titles", noTargetMatchText("Supplier", "zzz", closest), true},
		{"rows found", "Found 2 Supplier Bill, showing 2:\n1. SB-0001", false},
		{"rows found after a note", "Matched supplier \"Ouma\" → 3 suppliers.\nFound 2 Supplier Bill, showing 2:", false},
		{"tool error", "Error: could not search Supplier Bill.", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := findReply(tc.reply).empty(); got != tc.want {
				t.Fatalf("findReply(%q).empty() = %v, want %v", tc.reply, got, tc.want)
			}
		})
	}
}

// TestToolAuditStatusAndResult checks the status of a tool_call audit row and the capped result it stores.
func TestToolAuditStatusAndResult(t *testing.T) {
	for _, tc := range []struct {
		name, tool, result, want string
	}{
		{"find with rows is completed", toolFindRecords, "Found 2 Supplier Bill, showing 2:", "completed"},
		{"find with no rows is empty", toolFindRecords, "No Supplier Bill found.", "empty"},
		{"find error is failed", toolFindRecords, "Error: could not search Supplier Bill.", "failed"},
		{"other tool with a zero-row reply is completed", toolGetRecord, "No Supplier Bill found.", "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolAuditStatus(tc.tool, tc.result); got != tc.want {
				t.Fatalf("toolAuditStatus(%q, %q) = %q, want %q", tc.tool, tc.result, got, tc.want)
			}
		})
	}

	long := strings.Repeat("a", 2000) + strings.Repeat("b", 2000) + strings.Repeat("c", 2000)
	got := toolAuditResult(long)
	if len(got) >= len(long) || !strings.HasPrefix(got, strings.Repeat("a", 2000)) || !strings.HasSuffix(got, strings.Repeat("c", 2000)) {
		t.Fatalf("toolAuditResult kept %d of %d characters, want head and tail capped", len(got), len(long))
	}
	if short := "Found 1 Supplier Bill"; toolAuditResult(short) != short {
		t.Fatalf("toolAuditResult(short) = %q, want it unchanged", toolAuditResult(short))
	}
}
