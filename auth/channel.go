package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	kdb "github.com/asenawritescode/kora/db"
	"time"

	"github.com/asenawritescode/kora/doctype"
	"github.com/oklog/ulid/v2"
)

type ChannelSession struct {
	ID              string
	Site            string
	ClientName      string
	ConversationKey string
	Provider        string
	SenderAddress   string
	TokenHash       string
	Permissions     []doctype.Permission
	PermissionsRaw  string
	TrustedUntil    time.Time
	SensitiveUntil  sql.NullTime
	RevokedAt       sql.NullTime
	RevokedReason   string
	CreatedAt       time.Time
	LastUsedAt      sql.NullTime
}

type ChannelSessionCreateParams struct {
	Site            string
	ClientName      string
	ConversationKey string
	Provider        string
	SenderAddress   string
	Permissions     []doctype.Permission
	TrustedUntil    time.Time
	SensitiveUntil  *time.Time
}

func CreateChannelSession(db *sql.DB, dialect kdb.QueryDialect, params ChannelSessionCreateParams) (string, *ChannelSession, error) {
	token, hash, err := generateChannelToken()
	if err != nil {
		return "", nil, err
	}
	rawPerms, err := json.Marshal(params.Permissions)
	if err != nil {
		return "", nil, err
	}
	now := time.Now().UTC()
	session := &ChannelSession{
		ID:              newChannelID("chs"),
		Site:            params.Site,
		ClientName:      params.ClientName,
		ConversationKey: params.ConversationKey,
		Provider:        params.Provider,
		SenderAddress:   params.SenderAddress,
		TokenHash:       hash,
		Permissions:     params.Permissions,
		PermissionsRaw:  string(rawPerms),
		TrustedUntil:    params.TrustedUntil.UTC(),
		CreatedAt:       now,
	}
	if params.SensitiveUntil != nil {
		session.SensitiveUntil = sql.NullTime{Time: params.SensitiveUntil.UTC(), Valid: true}
	}
	_, err = db.Exec(kdb.Rebind(dialect, `INSERT INTO _kora_channel_session
		(id, site, client_name, conversation_key, provider, sender_address, token_hash, api_permissions,
		 trusted_until, sensitive_until, revoked_at, revoked_reason, created_at, last_used_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		session.ID, session.Site, session.ClientName, session.ConversationKey, session.Provider, session.SenderAddress,
		session.TokenHash, session.PermissionsRaw, session.TrustedUntil, nullableTime(session.SensitiveUntil),
		nil, "", session.CreatedAt, nil,
	)
	if err != nil {
		return "", nil, err
	}
	return token, session, nil
}

func RevokeChannelSession(db *sql.DB, dialect kdb.QueryDialect, id string, reason string) error {
	_, err := db.Exec(kdb.Rebind(dialect, `UPDATE _kora_channel_session
		SET revoked_at = ?, revoked_reason = ?
		WHERE id = ? AND revoked_at IS NULL`), time.Now().UTC(), reason, id)
	return err
}

func AuthenticateChannelSession(db *sql.DB, dialect kdb.QueryDialect, token string) (*ChannelSession, error) {
	hash := channelTokenHash(token)
	row := db.QueryRow(kdb.Rebind(dialect, `SELECT id, site, client_name, conversation_key, provider, sender_address, token_hash, api_permissions,
		trusted_until, sensitive_until, revoked_at, revoked_reason, created_at, last_used_at
		FROM _kora_channel_session
		WHERE token_hash = ?`), hash)
	var session ChannelSession
	var permsRaw string
	if err := row.Scan(
		&session.ID, &session.Site, &session.ClientName, &session.ConversationKey, &session.Provider, &session.SenderAddress,
		&session.TokenHash, &permsRaw, &session.TrustedUntil, &session.SensitiveUntil, &session.RevokedAt,
		&session.RevokedReason, &session.CreatedAt, &session.LastUsedAt,
	); err != nil {
		return nil, err
	}
	if session.RevokedAt.Valid || time.Now().UTC().After(session.TrustedUntil) {
		return nil, sql.ErrNoRows
	}
	session.PermissionsRaw = permsRaw
	session.Permissions = parseExtensionPermissions(permsRaw)
	_, _ = db.Exec(kdb.Rebind(dialect, `UPDATE _kora_channel_session SET last_used_at = ? WHERE id = ?`), time.Now().UTC(), session.ID)
	return &session, nil
}

func generateChannelToken() (string, string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	token := hex.EncodeToString(buf)
	return token, channelTokenHash(token), nil
}

func channelTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newChannelID(prefix string) string {
	entropy := ulid.Monotonic(rand.Reader, 0)
	return fmt.Sprintf("%s_%s", prefix, ulid.MustNew(ulid.Timestamp(time.Now().UTC()), entropy).String())
}

func NewAuditID() string {
	return newChannelID("cha")
}

func nullableTime(value sql.NullTime) any {
	if !value.Valid {
		return nil
	}
	return value.Time
}
