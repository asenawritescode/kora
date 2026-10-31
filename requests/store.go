// Package requests owns durable human requests and assignments. These records
// are persistence models, not public wire DTOs; transports project them through
// current authorization before returning any response.
package requests

import (
	"context"
	"database/sql"
	"time"

	"github.com/asenawritescode/kora/contract"
)

type RequestRecord struct {
	Tenant              contract.TenantID
	ID                  contract.RequestID
	Revision            contract.Revision
	Kind                contract.RequestKind
	AssignmentMode      contract.AssignmentMode
	Status              contract.RequestStatus
	Requester           contract.PrincipalID
	Subject             *contract.SubjectRef
	SubjectDigest       string
	Title               string
	Message             string
	Source              *contract.RequestSource
	DueAt               *time.Time
	Outcome             *contract.RequestOutcome
	ResolvedAt          *time.Time
	Continuation        *contract.ContinuationView
	CreatedAt           time.Time
	UpdatedAt           time.Time
	PolicyID            string
	PolicyRevision      contract.Revision
	InformationSchema   *contract.SchemaRef
	SupersedesRequestID *contract.RequestID
	Assignments         []AssignmentRecord
}

type AssignmentRecord struct {
	ID                 contract.AssignmentID
	RequestID          contract.RequestID
	Principal          contract.PrincipalID
	OriginalTarget     contract.Assignee
	Stage              uint16
	Status             contract.AssignmentStatus
	AllowedResponses   []contract.ResponseKind
	MembershipRevision contract.Revision
}

type DecisionRecord struct {
	Tenant       contract.TenantID
	ID           contract.DecisionID
	RequestID    contract.RequestID
	AssignmentID contract.AssignmentID
	Actor        contract.PrincipalID
	OnBehalfOf   contract.PrincipalID
	Response     contract.ResponseKind
	Comment      string
	Information  *contract.InformationResponse
	At           time.Time
}

type WriteIdentity struct {
	Tenant         contract.TenantID
	Actor          contract.PrincipalID
	Operation      string
	IdempotencyKey contract.IdempotencyKey
	CommandDigest  string
}

type CreateCommit struct {
	Identity                   WriteIdentity
	Request                    RequestRecord
	ExpectedPolicyRevision     contract.Revision
	ExpectedMembershipRevision contract.Revision
	ExpectedSubject            *contract.SubjectRef
	Assignments                []AssignmentRecord
	History                    contract.RequestHistoryEntry
	Events                     []contract.EventEnvelope
}

type DecisionCommit struct {
	Request                    RequestRecord
	ExpectedRevision           contract.Revision
	Decision                   DecisionRecord
	Receipt                    contract.ResponseReceipt
	Identity                   WriteIdentity
	ExpectedSubject            *contract.SubjectRef
	ExpectedPolicyRevision     contract.Revision
	ExpectedMembershipRevision contract.Revision
	Events                     []contract.EventEnvelope
}

type ReassignCommit struct {
	Identity                   WriteIdentity
	RequestID                  contract.RequestID
	ExpectedRevision           contract.Revision
	ExpectedPolicyRevision     contract.Revision
	ExpectedMembershipRevision contract.Revision
	AssignmentID               contract.AssignmentID
	Replacement                AssignmentRecord
	Reason                     string
	Events                     []contract.EventEnvelope
}

type CancelCommit struct {
	Identity                   WriteIdentity
	RequestID                  contract.RequestID
	ExpectedRevision           contract.Revision
	ExpectedPolicyRevision     contract.Revision
	ExpectedMembershipRevision contract.Revision
	Reason                     string
	Events                     []contract.EventEnvelope
}

type VisibilityScope struct {
	Tenant       contract.TenantID
	Actor        contract.PrincipalID
	CanManageAll bool // derived by policy; never decoded from HTTP
}

type DecisionStore interface {
	Get(ctx context.Context, tenant contract.TenantID, id contract.RequestID) (RequestRecord, error)
	CommitDecision(ctx context.Context, change DecisionCommit) (contract.ResponseReceipt, error)
}

// DecisionReceiptLookup resolves a response retry before current assignment
// eligibility is evaluated. Implementations scope receipts by the complete
// authenticated write identity and reject reuse with a different digest.
type DecisionReceiptLookup interface {
	LookupResponseReceipt(context.Context, WriteIdentity) (contract.ResponseReceipt, bool, error)
}

type Store interface {
	DecisionStore
	Create(ctx context.Context, change CreateCommit) (RequestRecord, error)
	ListVisible(ctx context.Context, scope VisibilityScope, filter contract.RequestFilter) (contract.Page[RequestRecord], error)
	Reassign(ctx context.Context, change ReassignCommit) (RequestRecord, error)
	Cancel(ctx context.Context, change CancelCommit) (RequestRecord, error)
	History(ctx context.Context, scope VisibilityScope, id contract.RequestID, page contract.PageRequest) (contract.Page[contract.RequestHistoryEntry], error)
	Response(ctx context.Context, scope VisibilityScope, request contract.RequestID, decision contract.DecisionID) (DecisionRecord, error)
	MarkRead(ctx context.Context, scope VisibilityScope, id contract.RequestID) error
	ExpireDue(ctx context.Context, tenant contract.TenantID, limit uint16) (uint16, error)
	MarkStale(ctx context.Context, tenant contract.TenantID, id contract.RequestID, expected contract.Revision, observed contract.SubjectRef) error
}

// TransactionCreateStore records an authorized request in a transaction owned
// by the caller. Implementations must not commit or roll back the transaction.
// Workflow human steps use this boundary to register their durable wait and
// request as one local unit of work.
type TransactionCreateStore interface {
	CreateInTx(context.Context, *sql.Tx, CreateCommit) (RequestRecord, error)
}
