package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTypedIdentifiersPreserveExistingTenantNamesAndRejectUnsafeValues(t *testing.T) {
	for _, value := range []string{"existing-tenant.example", "site_42", "a/b"} {
		got, err := ParseTenantID(value)
		if err != nil || string(got) != value {
			t.Errorf("ParseTenantID(%q) = %q, %v", value, got, err)
		}
	}
	for _, value := range []string{"", " leading", "trailing ", "bad\nvalue", strings.Repeat("x", 161)} {
		if _, err := ParseTenantID(value); err == nil {
			t.Errorf("ParseTenantID(%q) unexpectedly succeeded", value)
		}
	}
}

func TestParseRevisionRequiresCanonicalUint64Decimal(t *testing.T) {
	for _, value := range []string{"0", "1", "18446744073709551615"} {
		if got, err := ParseRevision(value); err != nil || string(got) != value {
			t.Errorf("ParseRevision(%q) = %q, %v", value, got, err)
		}
	}
	for _, value := range []string{"", "00", "01", "+1", "-1", "1.0", "18446744073709551616"} {
		if _, err := ParseRevision(value); err == nil {
			t.Errorf("ParseRevision(%q) unexpectedly succeeded", value)
		}
	}
}

func TestValidatePageRequestBoundsLimitAndCursor(t *testing.T) {
	for _, limit := range []uint16{1, 25, 100} {
		if err := ValidatePageRequest(PageRequest{Limit: limit}); err != nil {
			t.Errorf("limit %d rejected: %v", limit, err)
		}
	}
	for _, limit := range []uint16{0, 101} {
		if err := ValidatePageRequest(PageRequest{Limit: limit}); err == nil {
			t.Errorf("limit %d unexpectedly accepted", limit)
		}
	}
	if err := ValidatePageRequest(PageRequest{Limit: 1, Cursor: PageCursor(strings.Repeat("x", 513))}); err == nil {
		t.Fatal("oversized cursor unexpectedly accepted")
	}
}

func TestRequestCommandValidationBoundsAndDuplicateParticipants(t *testing.T) {
	if err := ValidateSubmitRequest(SubmitRequest{Kind: RequestReview, Title: "Review", Message: "note"}); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	duplicate := SubmitRequest{Kind: RequestReview, Title: "Review", Assignment: &AssignmentSpec{Mode: AssignmentAll, Assignees: []Assignee{{Kind: AssigneeUser, ID: "u1"}, {Kind: AssigneeUser, ID: "u1"}}}}
	for name, cmd := range map[string]SubmitRequest{
		"blank title":        {Kind: RequestReview, Title: " \t", Message: "ok"},
		"duplicate assignee": duplicate,
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateSubmitRequest(cmd); err == nil {
				t.Fatal("invalid command accepted")
			}
		})
	}
	longTitle := make([]rune, 161)
	for i := range longTitle {
		longTitle[i] = 'x'
	}
	if err := ValidateSubmitRequest(SubmitRequest{Kind: RequestReview, Title: string(longTitle)}); err == nil {
		t.Fatal("title over 160 Unicode characters accepted")
	}
	if err := ValidateSubmitRequest(SubmitRequest{Kind: RequestReview, Title: "ok", Message: strings.Repeat("é", 4097)}); err == nil {
		t.Fatal("message over byte limit accepted")
	}
	tooMany := make([]Assignee, maxRequestAssignees+1)
	for i := range tooMany {
		tooMany[i] = Assignee{Kind: AssigneeUser, ID: string(rune('a' + i))}
	}
	if err := ValidateSubmitRequest(SubmitRequest{Kind: RequestReview, Title: "ok", Assignment: &AssignmentSpec{Mode: AssignmentAny, Assignees: tooMany}}); err == nil {
		t.Fatal("assignment over item limit accepted")
	}
}

func TestResponseAndReassignmentCommandValidation(t *testing.T) {
	valid := RespondRequest{Information: &InformationResponse{SchemaID: "answer", SchemaVersion: 1, Values: json.RawMessage(`{"answer":true}`)}}
	if err := ValidateRespondRequest(valid); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	oversized := strings.Repeat("x", maxInformationBytes)
	if err := ValidateRespondRequest(RespondRequest{Information: &InformationResponse{SchemaID: "answer", SchemaVersion: 1, Values: json.RawMessage(`"` + oversized + `"`)}}); err == nil {
		t.Fatal("oversized information response accepted")
	}
	if err := ValidateRespondRequest(RespondRequest{Comment: strings.Repeat("x", maxRequestTextBytes+1)}); err == nil {
		t.Fatal("oversized comment accepted")
	}
	if err := ValidateReassignRequest(ReassignRequest{To: Assignee{Kind: AssigneeUser, ID: "u2"}, Reason: "membership changed"}); err != nil {
		t.Fatalf("valid reassignment rejected: %v", err)
	}
	if err := ValidateCancelRequest(CancelRequest{Reason: strings.Repeat("x", maxRequestTextBytes+1)}); err == nil {
		t.Fatal("oversized cancellation reason accepted")
	}
}
