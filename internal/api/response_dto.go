package api

import (
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api/ai"
)

type aiCancelResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

type aiResumeResponse struct {
	RunID       string `json:"run_id"`
	Status      string `json:"status"`
	ResumeToken string `json:"resume_token"`
}

type aiGrantApprovalResponse struct {
	ApprovalID string `json:"approval_id"`
	State      string `json:"state"`
	GrantedBy  string `json:"granted_by"`
	RunID      string `json:"run_id"`
	RunStatus  string `json:"run_status"`
}

type aiRetentionCleanupResponse struct {
	Removed int64 `json:"removed"`
}

type extensionCreatedResponse struct {
	Name        string `json:"name"`
	Secret      string `json:"secret"`
	AccessToken string `json:"access_token"`
	Warning     string `json:"warning"`
}

type managedChannelClientResponse = contract.ManagedChannelClientV2

type managedSiteIdentityResponse = contract.ManagedSiteIdentityV2

type extensionRotatedSecretResponse struct {
	Secret  string `json:"secret"`
	Warning string `json:"warning"`
}

type extensionSummary struct {
	Name                string `json:"name"`
	DisplayName         string `json:"display_name"`
	Description         string `json:"description"`
	EndpointURL         string `json:"endpoint_url"`
	IsActive            bool   `json:"is_active"`
	Subscriptions       string `json:"subscriptions"`
	APIPermissions      string `json:"api_permissions"`
	SecretCount         int    `json:"secret_count"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	InstalledAt         string `json:"installed_at"`
	LastDeliveryAt      string `json:"last_delivery_at"`
	LastError           string `json:"last_error"`
}

type extensionDelivery struct {
	ID             string `json:"id"`
	EventID        string `json:"event_id"`
	EventType      string `json:"event_type"`
	EndpointURL    string `json:"endpoint_url"`
	Status         string `json:"status"`
	Attempt        int    `json:"attempt"`
	ResponseStatus int    `json:"response_status"`
	DurationMs     int    `json:"duration_ms"`
	ErrorMessage   string `json:"error_message"`
	CreatedAt      string `json:"created_at"`
}

type extensionListResponse struct {
	Extensions []extensionSummary `json:"extensions"`
}

type extensionDeliveriesResponse struct {
	Deliveries []extensionDelivery `json:"deliveries"`
}

type extensionReplayResponse struct {
	Status string `json:"status"`
}

type extensionGetResponse struct {
	Status string `json:"status"`
}

type extensionDeleteResponse struct {
	Status string `json:"status"`
}

type channelSessionIssueResponse = contract.ChannelSessionIssueResponseV2

type channelSessionRevokeResponse struct {
	Status string `json:"status"`
}

type channelToolsResponse struct {
	Version        string              `json:"version"`
	ConfigRevision uint64              `json:"config_revision"`
	Tools          []ai.ToolDescriptor `json:"tools"`
}

type channelToolResponse struct {
	Result string `json:"result"`
}

type configVersionSnapshotFile = contract.ConfigurationSnapshotFileV2

type configVersionSnapshotResponse = contract.ConfigurationSnapshotResponseV2

type deletedResponse struct {
	Message       string `json:"message"`
	UsersWithRole int    `json:"users_with_role,omitempty"`
}

type savedResponse struct {
	Message string `json:"message"`
}

type scriptStatusResponse struct {
	Status string `json:"status"`
}

type scriptValidateResponse struct {
	Valid    bool     `json:"valid"`
	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

type consoleLoginResponse struct {
	Token               string `json:"token"`
	Email               string `json:"email"`
	NeedsPasswordChange bool   `json:"needs_password_change"`
}

type consoleMessageResponse struct {
	Message string `json:"message"`
}

type consoleSiteCreateResponse struct {
	Hostname     string `json:"hostname"`
	DBName       string `json:"db_name,omitempty"`
	WorkspaceURL string `json:"workspace_url,omitempty"`
	Admin        string `json:"admin,omitempty"`
	AdminEmail   string `json:"admin_email,omitempty"`
	Status       string `json:"status"`
}

type consoleProvisioningStatusResponse struct {
	JobID            string    `json:"job_id"`
	Site             string    `json:"site"`
	State            string    `json:"state"`
	Attempt          int       `json:"attempt"`
	OperationID      string    `json:"operation_id,omitempty"`
	IdempotencyKey   string    `json:"idempotency_key,omitempty"`
	InputFingerprint string    `json:"input_fingerprint,omitempty"`
	OutputID         string    `json:"output_id,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
	WorkspaceURL     string    `json:"workspace_url,omitempty"`
}

type consoleProvisioningListResponse struct {
	Jobs []consoleProvisioningStatusResponse `json:"jobs"`
}

type consoleSiteUpdateResponse struct {
	Hostname      string   `json:"hostname"`
	Domains       []string `json:"domains"`
	FileStorage   string   `json:"file_storage"`
	StorageBucket string   `json:"storage_bucket,omitempty"`
}

type consoleSiteDeleteResponse struct {
	Hostname string `json:"hostname"`
	Deleted  bool   `json:"deleted"`
}
