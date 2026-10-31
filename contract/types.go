package contract

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

type TenantID string
type PrincipalID string
type RequestID string
type AssignmentID string
type DecisionID string
type InstanceID string
type DefinitionID string
type StepID string
type OperationID string
type DeploymentID string
type DBServerID string
type IdempotencyKey string
type Revision string

// PageCursor is an opaque cursor for new paginated APIs. Cursor remains the
// existing structured stream/sync cursor for wire compatibility.
type PageCursor string

// Page is a bounded keyset-paginated result. It is a Go helper; concrete HTTP
// responses are declared and generated as named wire types when endpoints are
// added, so the generic type is deliberately not a schema root.
type Page[T any] struct {
	Items      []T        `json:"items"`
	NextCursor PageCursor `json:"next_cursor,omitempty"`
}

//contract:enum:v2
type OperationState string

const (
	OperationRequested        OperationState = "requested"
	OperationRunning          OperationState = "running"
	OperationSucceeded        OperationState = "succeeded"
	OperationFailed           OperationState = "failed"
	OperationRecoveryRequired OperationState = "recovery_required"
)

// SubjectRef binds an operation or request to a versioned business subject.
//
//contract:wire:v2
type SubjectRef struct {
	Kind     string   `json:"kind"`
	ID       string   `json:"id"`
	Revision Revision `json:"revision"`
}

//contract:wire:v2
type SchemaRef struct {
	ID      string `json:"id"`
	Version uint32 `json:"version"`
}

// PageRequest is a bounded, stable-order page query.
//
//contract:wire:v2
type PageRequest struct {
	Cursor PageCursor `json:"cursor,omitempty"`
	//contract:min=1
	//contract:max=100
	Limit uint16 `json:"limit"`
}

// CommandMeta carries retry identity and optimistic concurrency data.
//
//contract:wire:v2
type CommandMeta struct {
	IdempotencyKey   IdempotencyKey `json:"idempotency_key"`
	ExpectedRevision Revision       `json:"expected_revision,omitempty"`
}

func ParseTenantID(raw string) (TenantID, error) {
	if err := validateOpaqueID(raw); err != nil {
		return "", err
	}
	return TenantID(raw), nil
}

func ParsePrincipalID(raw string) (PrincipalID, error) {
	if err := validateOpaqueID(raw); err != nil {
		return "", err
	}
	return PrincipalID(raw), nil
}
func ParseRequestID(raw string) (RequestID, error) {
	if err := validateOpaqueID(raw); err != nil {
		return "", err
	}
	return RequestID(raw), nil
}
func ParseAssignmentID(raw string) (AssignmentID, error) {
	if err := validateOpaqueID(raw); err != nil {
		return "", err
	}
	return AssignmentID(raw), nil
}
func ParseDecisionID(raw string) (DecisionID, error) {
	if err := validateOpaqueID(raw); err != nil {
		return "", err
	}
	return DecisionID(raw), nil
}
func ParseInstanceID(raw string) (InstanceID, error) {
	if err := validateOpaqueID(raw); err != nil {
		return "", err
	}
	return InstanceID(raw), nil
}
func ParseDefinitionID(raw string) (DefinitionID, error) {
	if err := validateOpaqueID(raw); err != nil {
		return "", err
	}
	return DefinitionID(raw), nil
}
func ParseStepID(raw string) (StepID, error) {
	if err := validateOpaqueID(raw); err != nil {
		return "", err
	}
	return StepID(raw), nil
}
func ParseOperationID(raw string) (OperationID, error) {
	if err := validateOpaqueID(raw); err != nil {
		return "", err
	}
	return OperationID(raw), nil
}
func ParseDeploymentID(raw string) (DeploymentID, error) {
	if err := validateOpaqueID(raw); err != nil {
		return "", err
	}
	return DeploymentID(raw), nil
}
func ParseDBServerID(raw string) (DBServerID, error) {
	if err := validateOpaqueID(raw); err != nil {
		return "", err
	}
	return DBServerID(raw), nil
}
func ParseIdempotencyKey(raw string) (IdempotencyKey, error) {
	if err := validateOpaqueID(raw); err != nil {
		return "", err
	}
	return IdempotencyKey(raw), nil
}

func ParseRevision(raw string) (Revision, error) {
	if raw == "" || (len(raw) > 1 && raw[0] == '0') {
		return "", fmt.Errorf("revision must be canonical unsigned decimal")
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("revision must be canonical unsigned decimal")
		}
	}
	if _, err := strconv.ParseUint(raw, 10, 64); err != nil {
		return "", fmt.Errorf("revision is outside uint64 range: %w", err)
	}
	return Revision(raw), nil
}

func ValidatePageRequest(page PageRequest) error {
	if page.Limit == 0 || page.Limit > 100 {
		return fmt.Errorf("page limit must be between 1 and 100")
	}
	if len(page.Cursor) > 512 {
		return fmt.Errorf("page cursor exceeds 512 bytes")
	}
	return nil
}

func validateOpaqueID(raw string) error {
	if raw == "" || len(raw) > 160 || strings.TrimSpace(raw) != raw {
		return fmt.Errorf("identifier must be 1 to 160 bytes without surrounding whitespace")
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return fmt.Errorf("identifier contains a control character")
		}
	}
	return nil
}
