package natsprovider

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultEventStreamMaxBytes    int64 = 1 << 30
	MaximumEventStreamMaxBytes    int64 = 1 << 36
	DefaultEventStreamMaxMessages       = 1_000_000
	MaximumEventStreamMaxMessages       = 100_000_000
	DirectoryStreamMaxBytes       int64 = 64 << 20
	DirectoryStreamMaxMessages          = 100_000
)

// Config controls the self-hosted NATS provider connection and bootstrap
// contract.
type Config struct {
	Name              string
	ServerURLs        []string
	Username          string
	Password          string
	Token             string
	StreamName        string
	SubjectPrefix     string
	ConsumerName      string
	ConsumerSubject   string
	AckWait           time.Duration
	DeadLetterSubject string
	MaxDeliver        int
	StreamMaxBytes    int64
	StreamMaxMessages int
}

// DeploymentManifest captures the operator-facing bootstrap/status fields from
// RFC §9.7.
type DeploymentManifest struct {
	DeploymentID          string   `json:"deployment_id"`
	OperatorID            string   `json:"operator_id"`
	NATSServerVersion     string   `json:"nats_server_version"`
	ClusterID             string   `json:"cluster_id"`
	AccountID             string   `json:"account_id"`
	CredentialRef         string   `json:"credential_ref"`
	TLSPolicy             string   `json:"tls_policy"`
	StreamConfigHash      string   `json:"stream_config_hash"`
	KVConfigHash          string   `json:"kv_config_hash"`
	ObjectStoreConfigHash string   `json:"object_store_config_hash"`
	BackupPolicy          string   `json:"backup_policy"`
	RPO                   string   `json:"rpo"`
	RTO                   string   `json:"rto"`
	LastValidatedAt       string   `json:"last_validated_at"`
	Resources             []string `json:"resources"`
}

// FromEnv builds a provider config from the standard operator environment.
func FromEnv() Config {
	urls := strings.FieldsFunc(os.Getenv("KORA_NATS_URLS"), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	})
	return Config{
		Name:              envOrDefault("KORA_NATS_NAME", "kora"),
		ServerURLs:        filterEmpty(urls),
		Username:          os.Getenv("KORA_NATS_USER"),
		Password:          os.Getenv("KORA_NATS_PASSWORD"),
		Token:             os.Getenv("KORA_NATS_TOKEN"),
		StreamName:        envOrDefault("KORA_NATS_STREAM", "KORA_EVENTS"),
		SubjectPrefix:     envOrDefault("KORA_NATS_SUBJECT_PREFIX", "kora"),
		ConsumerName:      envOrDefault("KORA_NATS_CONSUMER", "kora-worker"),
		DeadLetterSubject: envOrDefault("KORA_NATS_DEAD_LETTER", "kora.deadletter"),
		MaxDeliver:        5,
		StreamMaxBytes:    envInt64("KORA_NATS_STREAM_MAX_BYTES", DefaultEventStreamMaxBytes),
		StreamMaxMessages: envInt("KORA_NATS_STREAM_MAX_MESSAGES", DefaultEventStreamMaxMessages),
	}
}

// Validate checks the provider configuration without touching the network.
func (c Config) Validate() error {
	c = c.withDefaults()
	if len(c.ServerURLs) == 0 {
		return fmt.Errorf("natsprovider: at least one server url is required")
	}
	if c.StreamName == "" {
		return fmt.Errorf("natsprovider: stream name is required")
	}
	if c.SubjectPrefix == "" {
		return fmt.Errorf("natsprovider: subject prefix is required")
	}
	if c.MaxDeliver <= 0 {
		return fmt.Errorf("natsprovider: max deliver must be positive")
	}
	if c.StreamMaxBytes < 1 || c.StreamMaxBytes > MaximumEventStreamMaxBytes {
		return fmt.Errorf("natsprovider: stream max bytes must be between 1 and %d", MaximumEventStreamMaxBytes)
	}
	if c.StreamMaxMessages < 1 || c.StreamMaxMessages > MaximumEventStreamMaxMessages {
		return fmt.Errorf("natsprovider: stream max messages must be between 1 and %d", MaximumEventStreamMaxMessages)
	}
	for _, raw := range c.ServerURLs {
		if _, err := url.ParseRequestURI(raw); err != nil {
			return fmt.Errorf("natsprovider: invalid server url %q: %w", raw, err)
		}
	}
	if err := validateSubjectToken(c.SubjectPrefix); err != nil {
		return fmt.Errorf("natsprovider: invalid subject prefix: %w", err)
	}
	return nil
}

func (c Config) withDefaults() Config {
	if c.StreamMaxBytes == 0 {
		c.StreamMaxBytes = DefaultEventStreamMaxBytes
	}
	if c.StreamMaxMessages == 0 {
		c.StreamMaxMessages = DefaultEventStreamMaxMessages
	}
	return c
}

// SortedServerURLs returns a canonical copy for hashing/comparison.
func (c Config) SortedServerURLs() []string {
	out := append([]string(nil), c.ServerURLs...)
	sort.Strings(out)
	return out
}

func validateSubjectToken(token string) error {
	if token == "" {
		return fmt.Errorf("empty token")
	}
	if strings.ContainsAny(token, "*> \t\r\n./") {
		return fmt.Errorf("token %q contains disallowed characters", token)
	}
	return nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value == 0 {
		return -1
	}
	return value
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value == 0 {
		return -1
	}
	return value
}

func filterEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// DeploymentManifest returns the RFC-recorded bootstrap manifest fields.
func (c Config) DeploymentManifest() DeploymentManifest {
	return DeploymentManifest{
		DeploymentID:          envOrDefault("KORA_NATS_DEPLOYMENT_ID", "local"),
		OperatorID:            envOrDefault("KORA_NATS_OPERATOR_ID", "local-operator"),
		NATSServerVersion:     envOrDefault("KORA_NATS_SERVER_VERSION", "unknown"),
		ClusterID:             envOrDefault("KORA_NATS_CLUSTER_ID", "unknown"),
		AccountID:             envOrDefault("KORA_NATS_ACCOUNT_ID", "unknown"),
		CredentialRef:         envOrDefault("KORA_NATS_CREDENTIAL_REF", "env"),
		TLSPolicy:             envOrDefault("KORA_NATS_TLS_POLICY", "required"),
		StreamConfigHash:      envOrDefault("KORA_NATS_STREAM_HASH", ""),
		KVConfigHash:          envOrDefault("KORA_NATS_KV_HASH", ""),
		ObjectStoreConfigHash: envOrDefault("KORA_NATS_OBJECT_STORE_HASH", ""),
		BackupPolicy:          envOrDefault("KORA_NATS_BACKUP_POLICY", "operator-managed"),
		RPO:                   envOrDefault("KORA_NATS_RPO", "unknown"),
		RTO:                   envOrDefault("KORA_NATS_RTO", "unknown"),
		LastValidatedAt:       envOrDefault("KORA_NATS_LAST_VALIDATED_AT", ""),
		Resources:             []string{c.StreamName, c.ConsumerName, c.DeadLetterSubject},
	}
}
