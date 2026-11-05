package auth

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

const (
	passwordLoginMaxPasswordBytes = 1024
	invalidPasswordLoginMessage   = "Email or password is incorrect."
)

// dummyPasswordHash keeps unknown-email and passwordless-account attempts on
// the same bcrypt cost as real comparisons.
var dummyPasswordHash = sync.OnceValue(func() []byte {
	hash, err := bcrypt.GenerateFromPassword([]byte("kora-password-login-timing-guard"), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return hash
})

// PasswordSignInDisabledContextKey is set by tenant sign-in policy (for example
// single-sign-on-only workspaces) to refuse local password sign-in.
const PasswordSignInDisabledContextKey = "auth_password_sign_in_disabled"

func passwordSignInAllowed(c *gin.Context) bool { return !c.GetBool(PasswordSignInDisabledContextKey) }

type passwordLoginAccount struct {
	user          User
	passwordHash  string
	emailVerified bool
}

func registerPasswordLoginRoute(auth *gin.RouterGroup, db *sql.DB, mailer MagicLinkMailer, publicAppBaseURL string) {
	auth.POST("/login", CSRFMiddleware(), func(c *gin.Context) {
		var request struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := c.ShouldBindJSON(&request); err != nil || strings.TrimSpace(request.Email) == "" || request.Password == "" || len(request.Password) > passwordLoginMaxPasswordBytes {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "auth.invalid_request", "message": "Email and password are required."}})
			return
		}
		site := c.GetString("site_name")
		targetDB := db
		if value, ok := c.Get("site_db"); ok {
			if siteDB, valid := value.(*sql.DB); valid && siteDB != nil {
				targetDB = siteDB
			}
		}
		if targetDB == nil || site == "" {
			c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "site.not_found", "message": "Site not found."}})
			return
		}
		emailAddr := strings.TrimSpace(request.Email)
		emailKey := strings.ToLower(emailAddr)
		if !enforceAuthRateLimit(c, "password-login:ip:"+site+"\x00"+c.ClientIP(), authRateLimitMax, authRateLimitWindow) ||
			!enforceAuthRateLimit(c, "password-login:email:"+site+"\x00"+emailKey, authRateLimitMax, authRateLimitWindow) {
			return
		}
		if !passwordSignInAllowed(c) {
			c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "auth.password_sign_in_disabled", "message": "This workspace uses single sign-on. Sign in with your organization account."}})
			return
		}
		if localSignInDisabled(c, targetDB) {
			return
		}

		account, err := lookupPasswordLoginAccount(c.Request.Context(), targetDB, kdb.Resolve(c.GetString("site_db_type")), site, emailAddr)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			slog.Error("password sign-in lookup failed", "site", site, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "auth.sign_in_failed", "message": "Sign-in is temporarily unavailable."}})
			return
		}
		if !passwordMatches(account, request.Password) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "auth.invalid_credentials", "message": invalidPasswordLoginMessage}})
			return
		}
		if !account.emailVerified {
			if mailer != nil && strings.TrimSpace(publicAppBaseURL) != "" {
				if _, err := issueOneTimeLinkWithPublicAppBaseURL(c, targetDB, site, &account.user, mailer, publicAppBaseURL); err != nil {
					slog.Error("issuing verification link failed", "site", site, "error", err)
				}
			}
			c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "auth.email_verification_required", "message": "Confirm your email address with a sign-in link before using your password."}})
			return
		}

		sid, err := sessionManagerForRequest(c, targetDB).CreateSession(site, &account.user)
		if err != nil {
			slog.Error("creating password sign-in session failed", "site", site, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "auth.sign_in_failed", "message": "Sign-in is temporarily unavailable."}})
			return
		}
		SetSecureCookie(c, "kora_sid", sid, int(SessionLifetime.Seconds()), "/", true)
		slog.Info("password sign-in", "site", site, "user", account.user.Name)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"name":      account.user.Name,
			"email":     account.user.Email,
			"full_name": account.user.FullName,
			"roles":     account.user.Roles,
		}})
	})
}

func lookupPasswordLoginAccount(parent context.Context, db *sql.DB, dialect kdb.QueryDialect, site, emailAddr string) (*passwordLoginAccount, error) {
	ctx, cancel := context.WithTimeout(parent, dbTimeout)
	defer cancel()
	var account passwordLoginAccount
	var roles string
	var verifiedAt sql.NullTime
	err := db.QueryRowContext(ctx, kdb.Rebind(dialect,
		`SELECT name, email, full_name, enabled, COALESCE(roles, ''), password_hash, email_verified_at FROM _kora_user WHERE site = ? AND LOWER(email) = LOWER(?)`),
		site, emailAddr,
	).Scan(&account.user.Name, &account.user.Email, &account.user.FullName, &account.user.Enabled, &roles, &account.passwordHash, &verifiedAt)
	if err != nil {
		return nil, err
	}
	account.user.Roles = splitRolesStr(roles)
	account.emailVerified = verifiedAt.Valid
	return &account, nil
}

// passwordMatches always performs one bcrypt comparison so missing users,
// passwordless accounts and disabled accounts cost the same as a wrong password.
func passwordMatches(account *passwordLoginAccount, password string) bool {
	hash := dummyPasswordHash()
	usable := account != nil && account.user.Enabled && strings.HasPrefix(account.passwordHash, "$2")
	if usable {
		hash = []byte(account.passwordHash)
	}
	return bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil && usable
}
