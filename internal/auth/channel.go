package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	kdb "github.com/asenawritescode/kora/internal/db"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/oklog/ulid/v2"
)

type ChannelSession struct {
	ID              string
	Site            string
	ClientName      string
	ConversationKey string
	Provider        string
	SenderAddress   string
	// SubjectUser is the enabled tenant user the session acts as; empty for delegated sessions.
	SubjectUser    string
	TokenHash      string
	Permissions    []doctype.Permission
	PermissionsRaw string
	TrustedUntil   time.Time
	SensitiveUntil sql.NullTime
	RevokedAt      sql.NullTime
	RevokedReason  string
	CreatedAt      time.Time
	LastUsedAt     sql.NullTime
}

type ChannelSessionCreateParams struct {
	Site            string
	ClientName      string
	ConversationKey string
	Provider        string
	SenderAddress   string
	// SubjectUser, when set, must name an enabled user of the site; the session then runs as that user.
	SubjectUser    string
	Permissions    []doctype.Permission
	TrustedUntil   time.Time
	SensitiveUntil *time.Time
}

// ErrSubjectUnavailable means a channel session's subject user is missing or disabled on the site.
var ErrSubjectUnavailable = errors.New("channel subject user is unavailable")

// LoadChannelSubject returns the enabled user a channel session acts as, with its roles from the user record.
func LoadChannelSubject(ctx context.Context, db *sql.DB, dialect kdb.QueryDialect, site, name string) (*User, error) {
	var user User
	var roles string
	err := db.QueryRowContext(ctx, kdb.Rebind(dialect, `SELECT name, enabled, COALESCE(roles, '') FROM _kora_user WHERE site = ? AND name = ?`), site, name).Scan(&user.Name, &user.Enabled, &roles)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSubjectUnavailable
	}
	if err != nil {
		return nil, err
	}
	if !user.Enabled {
		return nil, ErrSubjectUnavailable
	}
	user.Roles = splitRolesStr(roles)
	return &user, nil
}

// ChannelSubjectByEmail returns the name of the enabled site user whose email is email, compared without case.
// A missing or disabled user is ErrSubjectUnavailable.
func ChannelSubjectByEmail(ctx context.Context, db *sql.DB, dialect kdb.QueryDialect, site, email string) (string, error) {
	var name string
	var enabled bool
	err := db.QueryRowContext(ctx, kdb.Rebind(dialect, `SELECT name, enabled FROM _kora_user WHERE site = ? AND LOWER(email) = ?`),
		site, strings.ToLower(strings.TrimSpace(email))).Scan(&name, &enabled)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !enabled) {
		return "", ErrSubjectUnavailable
	}
	if err != nil {
		return "", err
	}
	return name, nil
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
		SubjectUser:     params.SubjectUser,
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
		 trusted_until, sensitive_until, revoked_at, revoked_reason, created_at, last_used_at, subject_user)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		session.ID, session.Site, session.ClientName, session.ConversationKey, session.Provider, session.SenderAddress,
		session.TokenHash, session.PermissionsRaw, session.TrustedUntil, nullableTime(session.SensitiveUntil),
		nil, "", session.CreatedAt, nil, session.SubjectUser,
	)
	if err != nil {
		return "", nil, err
	}
	return token, session, nil
}

// RevokeChannelSession marks a channel session revoked, recording why. It is a
// no-op for a session that is already revoked.
func RevokeChannelSession(ctx context.Context, db *sql.DB, dialect kdb.QueryDialect, id string, reason string) error {
	_, err := db.ExecContext(ctx, kdb.Rebind(dialect, `UPDATE _kora_channel_session
		SET revoked_at = ?, revoked_reason = ?
		WHERE id = ? AND revoked_at IS NULL`), time.Now().UTC(), reason, id)
	return err
}

// AuthenticateChannelSession resolves a channel token to its live session and
// refreshes last_used_at. It returns an error for unknown, revoked or expired tokens.
func AuthenticateChannelSession(ctx context.Context, db *sql.DB, dialect kdb.QueryDialect, token string) (*ChannelSession, error) {
	hash := channelTokenHash(token)
	row := db.QueryRowContext(ctx, kdb.Rebind(dialect, `SELECT id, site, client_name, conversation_key, provider, sender_address, token_hash, api_permissions,
		trusted_until, sensitive_until, revoked_at, revoked_reason, created_at, last_used_at, subject_user
		FROM _kora_channel_session
		WHERE token_hash = ?`), hash)
	var session ChannelSession
	var permsRaw string
	if err := row.Scan(
		&session.ID, &session.Site, &session.ClientName, &session.ConversationKey, &session.Provider, &session.SenderAddress,
		&session.TokenHash, &permsRaw, &session.TrustedUntil, &session.SensitiveUntil, &session.RevokedAt,
		&session.RevokedReason, &session.CreatedAt, &session.LastUsedAt, &session.SubjectUser,
	); err != nil {
		return nil, err
	}
	if session.RevokedAt.Valid || time.Now().UTC().After(session.TrustedUntil) {
		return nil, sql.ErrNoRows
	}
	session.PermissionsRaw = permsRaw
	session.Permissions = parseExtensionPermissions(permsRaw)
	_, _ = db.ExecContext(ctx, kdb.Rebind(dialect, `UPDATE _kora_channel_session SET last_used_at = ? WHERE id = ?`), time.Now().UTC(), session.ID)
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
