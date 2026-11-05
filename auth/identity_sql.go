package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	kdb "github.com/asenawritescode/kora/db"
)

// SQLIdentityLinks persists external sign-in identities in _kora_user_identity.
// A link is keyed by (site, issuer, subject); email is stored only as the
// address that was verified when the link was made and never resolves a user.
type SQLIdentityLinks struct {
	DB      *sql.DB
	Dialect kdb.QueryDialect
}

func (s SQLIdentityLinks) query(statement string) string {
	if s.Dialect == nil {
		return statement
	}
	return kdb.Rebind(s.Dialect, statement)
}

// Resolve returns the user linked to (issuer, subject) on site, or
// ErrLinkNotFound.
func (s SQLIdentityLinks) Resolve(ctx context.Context, site, issuer, subject string) (string, error) {
	var user string
	err := s.DB.QueryRowContext(ctx, s.query(`SELECT user_name FROM _kora_user_identity WHERE site = ? AND issuer = ? AND subject = ?`), site, issuer, subject).Scan(&user)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrLinkNotFound
	}
	if err != nil {
		return "", fmt.Errorf("resolve identity link: %w", err)
	}
	return user, nil
}

// SubjectFor returns the subject already linked for user and issuer on site,
// or ErrLinkNotFound.
func (s SQLIdentityLinks) SubjectFor(ctx context.Context, site, user, issuer string) (string, error) {
	var subject string
	err := s.DB.QueryRowContext(ctx, s.query(`SELECT subject FROM _kora_user_identity WHERE site = ? AND user_name = ? AND issuer = ?`), site, user, issuer).Scan(&subject)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrLinkNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read identity link: %w", err)
	}
	return subject, nil
}

// Link binds (issuer, subject) to user. Re-linking the same pair to the same
// user is a no-op. It fails with ErrIdentityCollision when the pair belongs to
// another user or the user already has a different subject for this issuer.
func (s SQLIdentityLinks) Link(ctx context.Context, site, user, issuer, subject, email string, linkedAt time.Time) error {
	if site == "" || user == "" || issuer == "" || subject == "" {
		return errors.New("identity link requires site, user, issuer and subject")
	}
	if owner, err := s.Resolve(ctx, site, issuer, subject); err == nil {
		if owner == user {
			return nil
		}
		return ErrIdentityCollision
	} else if !errors.Is(err, ErrLinkNotFound) {
		return err
	}
	if existing, err := s.SubjectFor(ctx, site, user, issuer); err == nil && existing != subject {
		return ErrIdentityCollision
	} else if err != nil && !errors.Is(err, ErrLinkNotFound) {
		return err
	}
	_, err := s.DB.ExecContext(ctx, s.query(`INSERT INTO _kora_user_identity (site, user_name, issuer, subject, email, linked_at) VALUES (?, ?, ?, ?, ?, ?)`), site, user, issuer, subject, email, linkedAt.UTC())
	if err == nil {
		return nil
	}
	// A concurrent link may have won the primary key; its owner decides.
	if owner, resolveErr := s.Resolve(ctx, site, issuer, subject); resolveErr == nil {
		if owner == user {
			return nil
		}
		return ErrIdentityCollision
	}
	return fmt.Errorf("insert identity link: %w", err)
}
