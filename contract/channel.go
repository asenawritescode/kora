package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// These versioned shapes cover the Engine channel routes consumed by Cloud.
// The token fields are one-time secrets and must never be persisted or logged.

//contract:wire:v2
type ManagedChannelClientV2 struct {
	Name        string `json:"name"`
	AccessToken string `json:"access_token"`
	Warning     string `json:"warning"`
}

// RevokeManagedChannelClientV2 disables the current Cloud-managed channel
// credential only when it still matches the credential Cloud intends to
// revoke. The fingerprint is SHA-256 over the one-time access token; the token
// itself is never sent back to Engine or logged by this command.
//
//contract:wire:v2
type RevokeManagedChannelClientV2 struct {
	//contract:minLength=64
	//contract:maxLength=64
	//contract:pattern=^[a-f0-9]{64}$
	CredentialFingerprint string `json:"credential_fingerprint"`
}

//contract:wire:v2
type RevokeManagedChannelClientResultV2 struct {
	Revoked bool `json:"revoked"`
}

func ValidateRevokeManagedChannelClientV2(value RevokeManagedChannelClientV2) error {
	if len(value.CredentialFingerprint) != 64 || value.CredentialFingerprint != strings.ToLower(value.CredentialFingerprint) {
		return fmt.Errorf("managed channel credential fingerprint must be a SHA-256 hex digest")
	}
	decoded, err := hex.DecodeString(value.CredentialFingerprint)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("managed channel credential fingerprint must be a SHA-256 hex digest")
	}
	return nil
}

//contract:wire:v2
type ManagedSiteIdentityV2 struct {
	SiteID         string `json:"site_id"`
	Status         string `json:"status"`
	ConfigRevision uint64 `json:"config_revision"`
	Healthy        bool   `json:"healthy"`
}

// ChannelSessionIssueRequestV2 is the managed channel's session request.
//
//contract:wire:v2
type ChannelSessionIssueRequestV2 struct {
	ClientName      string `json:"client_name"`
	ConversationKey string `json:"conversation_key"`
	Provider        string `json:"provider"`
	SenderAddress   string `json:"sender_address"`
	// SubjectUser, when set, names the enabled tenant user the session runs as. The session then uses that user's
	// roles and ignores Permissions.
	SubjectUser    string            `json:"subject_user,omitempty"`
	Permissions    []PermissionGrant `json:"permissions"`
	TrustedUntil   string            `json:"trusted_until"`
	SensitiveUntil string            `json:"sensitive_until,omitempty"`
}

// ChannelChatRequestV2 is one turn from a managed channel. It needs a channel session issued with a subject user.
//
//contract:wire:v2
type ChannelChatRequestV2 struct {
	Message         string `json:"message"`
	ConversationKey string `json:"conversation_key"`
}

// ChannelChatResponseV2 is the reply to one channel turn. AwaitingConfirmation is true when the conversation holds a
// preview the person has not yet confirmed.
//
//contract:wire:v2
type ChannelChatResponseV2 struct {
	Result               string             `json:"result"`
	Choices              *ChoiceQuestion    `json:"choices,omitempty"`
	AwaitingConfirmation bool               `json:"awaiting_confirmation"`
	Usage                ChannelChatUsageV2 `json:"usage"`
}

// ChannelChatUsageV2 is the model token usage of one channel turn, summed over its provider calls.
//
//contract:wire:v2
type ChannelChatUsageV2 struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
}

// ChannelSessionIssueResponseV2 carries a newly issued, short-lived bearer
// credential. Consumers must treat AccessToken as a secret.
//
//contract:wire:v2
type ChannelSessionIssueResponseV2 struct {
	SessionID       string     `json:"session_id"`
	AccessToken     string     `json:"access_token"`
	TrustedUntil    time.Time  `json:"trusted_until"`
	SensitiveUntil  *time.Time `json:"sensitive_until,omitempty"`
	ConversationKey string     `json:"conversation_key"`
}
