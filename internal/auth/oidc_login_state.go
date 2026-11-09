package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/internal/auth/oidc"
	kdb "github.com/asenawritescode/kora/internal/db"
)

// oidcLoginState is the server-side half of an OIDC authorization request.
// Only hashes of the state and of the browser binding cookie are stored.
type oidcLoginState struct {
	Site         string
	ProviderID   string
	Nonce        string
	CodeVerifier string
	ReturnTo     string
	BindingHash  string
	ExpiresAt    time.Time
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// SQLLoginStates keeps pending OIDC logins in _kora_oidc_login so any Engine
// replica can complete a login another replica started.
type SQLLoginStates struct {
	DB      *sql.DB
	Dialect kdb.QueryDialect
}

func (s SQLLoginStates) query(statement string) string {
	if s.Dialect == nil {
		return statement
	}
	return kdb.Rebind(s.Dialect, statement)
}

// Save stores a pending login and drops expired ones.
func (s SQLLoginStates) Save(ctx context.Context, stateHash string, st oidcLoginState) error {
	if _, err := s.DB.ExecContext(ctx, s.query(`DELETE FROM _kora_oidc_login WHERE expires_at < ?`), time.Now().UTC()); err != nil {
		return fmt.Errorf("purge expired sign-in requests: %w", err)
	}
	_, err := s.DB.ExecContext(ctx, s.query(`INSERT INTO _kora_oidc_login (state_hash, site, provider_id, nonce, code_verifier, return_to, binding_hash, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		stateHash, st.Site, st.ProviderID, st.Nonce, st.CodeVerifier, st.ReturnTo, st.BindingHash, st.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("save sign-in request: %w", err)
	}
	return nil
}

// Consume returns and deletes a pending login. Exactly one caller can consume
// a state; later attempts get oidc.ErrStateReplayed.
func (s SQLLoginStates) Consume(ctx context.Context, site, stateHash string, now time.Time) (oidcLoginState, error) {
	var st oidcLoginState
	var expires any
	err := s.DB.QueryRowContext(ctx, s.query(`SELECT site, provider_id, nonce, code_verifier, return_to, binding_hash, expires_at FROM _kora_oidc_login WHERE state_hash = ?`), stateHash).
		Scan(&st.Site, &st.ProviderID, &st.Nonce, &st.CodeVerifier, &st.ReturnTo, &st.BindingHash, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return oidcLoginState{}, oidc.ErrStateInvalid
	}
	if err != nil {
		return oidcLoginState{}, fmt.Errorf("read sign-in request: %w", err)
	}
	result, err := s.DB.ExecContext(ctx, s.query(`DELETE FROM _kora_oidc_login WHERE state_hash = ?`), stateHash)
	if err != nil {
		return oidcLoginState{}, fmt.Errorf("consume sign-in request: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return oidcLoginState{}, oidc.ErrStateReplayed
	}
	if st.Site != site {
		return oidcLoginState{}, oidc.ErrStateSiteMismatch
	}
	if st.ExpiresAt, err = parseDBTimeValue(expires); err != nil {
		return oidcLoginState{}, fmt.Errorf("read sign-in request expiry: %w", err)
	}
	if !now.Before(st.ExpiresAt) {
		return oidcLoginState{}, oidc.ErrStateExpired
	}
	return st, nil
}
