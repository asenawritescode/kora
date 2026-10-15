package contract

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	maxRequestTextBytes = 8 * 1024
	maxRequestAssignees = 50
	maxInformationBytes = 64 * 1024
)

// ValidateSubmitRequest checks transport-independent wire constraints. It does
// not establish tenant membership, policy eligibility, or subject visibility;
// those checks belong to the request service at commit time.
func ValidateSubmitRequest(cmd SubmitRequest) error {
	if !utf8.ValidString(cmd.Title) || strings.TrimSpace(cmd.Title) == "" || utf8.RuneCountInString(cmd.Title) > 160 {
		return fmt.Errorf("title must contain 1 to 160 Unicode characters")
	}
	if err := validateTextBytes("message", cmd.Message); err != nil {
		return err
	}
	if cmd.Assignment != nil {
		if len(cmd.Assignment.Assignees) == 0 || len(cmd.Assignment.Assignees) > maxRequestAssignees {
			return fmt.Errorf("assignment must contain 1 to %d assignees", maxRequestAssignees)
		}
		if cmd.Assignment.Mode != AssignmentAny && cmd.Assignment.Mode != AssignmentAll && cmd.Assignment.Mode != AssignmentSequential {
			return fmt.Errorf("invalid assignment mode")
		}
		seen := make(map[Assignee]struct{}, len(cmd.Assignment.Assignees))
		for _, assignee := range cmd.Assignment.Assignees {
			if err := ValidateAssignee(assignee); err != nil {
				return fmt.Errorf("assignment contains an invalid assignee: %w", err)
			}
			if _, exists := seen[assignee]; exists {
				return fmt.Errorf("assignment contains a duplicate assignee")
			}
			seen[assignee] = struct{}{}
		}
	}
	return nil
}

// ValidateRespondRequest checks the request body bounds before authorization
// and transactional response checks are performed by the request service.
func ValidateRespondRequest(cmd RespondRequest) error {
	if err := validateTextBytes("comment", cmd.Comment); err != nil {
		return err
	}
	if cmd.Information != nil {
		if strings.TrimSpace(cmd.Information.SchemaID) == "" || cmd.Information.SchemaVersion == 0 {
			return fmt.Errorf("information response requires a schema ID and version")
		}
		if len(cmd.Information.Values) == 0 || len(cmd.Information.Values) > maxInformationBytes || !json.Valid(cmd.Information.Values) {
			return fmt.Errorf("information response values must be valid JSON no larger than %d bytes", maxInformationBytes)
		}
	}
	return nil
}

func ValidateReassignRequest(cmd ReassignRequest) error {
	if err := validateTextBytes("reason", cmd.Reason); err != nil {
		return err
	}
	return ValidateAssignee(cmd.To)
}

func ValidateAssignee(assignee Assignee) error {
	if assignee.Kind != AssigneeUser && assignee.Kind != AssigneeRole && assignee.Kind != AssigneeGroup {
		return fmt.Errorf("assignee kind is invalid")
	}
	if err := validateOpaqueID(assignee.ID); err != nil {
		return fmt.Errorf("assignee ID: %w", err)
	}
	return nil
}

func ValidateCancelRequest(cmd CancelRequest) error { return validateTextBytes("reason", cmd.Reason) }

func validateTextBytes(name, value string) error {
	if !utf8.ValidString(value) || len(value) > maxRequestTextBytes {
		return fmt.Errorf("%s must be valid UTF-8 no larger than %d bytes", name, maxRequestTextBytes)
	}
	return nil
}
