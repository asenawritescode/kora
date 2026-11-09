package oidc

import (
	"errors"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
)

func TestPKCEGenerateAndVerify(t *testing.T) {
	v, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if v.Method != "S256" {
		t.Fatalf("method = %q, want S256", v.Method)
	}
	if err := VerifyPKCE(v.CodeVerifier, v.CodeChallenge); err != nil {
		t.Fatalf("verify valid pair: %v", err)
	}
}

func TestPKCECodeVerifierMismatch(t *testing.T) {
	v, _ := GeneratePKCE()
	if err := VerifyPKCE("wrong-verifier", v.CodeChallenge); !errors.Is(err, ErrPKCEMismatch) {
		t.Fatalf("want ErrPKCEMismatch, got %v", err)
	}
}

func TestStateSingleUseAndReplayRejected(t *testing.T) {
	s := NewStateStore(time.Minute)
	token, err := s.New("site-a", "nonce-1")
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	nonce, err := s.Consume(token, "site-a")
	if err != nil || nonce != "nonce-1" {
		t.Fatalf("first consume: nonce=%q err=%v", nonce, err)
	}
	if _, err := s.Consume(token, "site-a"); !errors.Is(err, ErrStateReplayed) {
		t.Fatalf("want ErrStateReplayed on second use, got %v", err)
	}
}

func TestStateSiteMismatchAndTamper(t *testing.T) {
	s := NewStateStore(time.Minute)
	token, _ := s.New("site-a", "nonce-1")
	if _, err := s.Consume(token, "site-b"); !errors.Is(err, ErrStateSiteMismatch) {
		t.Fatalf("want ErrStateSiteMismatch, got %v", err)
	}
	if _, err := s.Consume("tampered-token", "site-a"); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("want ErrStateInvalid, got %v", err)
	}
}

func TestStateExpiry(t *testing.T) {
	s := NewStateStore(time.Millisecond)
	token, _ := s.New("site-a", "nonce-1")
	time.Sleep(5 * time.Millisecond)
	if _, err := s.Consume(token, "site-a"); !errors.Is(err, ErrStateExpired) {
		t.Fatalf("want ErrStateExpired, got %v", err)
	}
}

func TestIssuerAudienceExpiryValidated(t *testing.T) {
	now := time.Now().UTC()
	base := IDTokenClaims{Issuer: "https://issuer", Audience: "client-id", Subject: "subj", ExpiresAt: now.Add(time.Hour)}
	opts := ValidateOptions{ExpectedIssuer: "https://issuer", ExpectedAudience: "client-id", Now: now}

	if _, err := ValidateIDToken(IDTokenClaims{Issuer: "https://evil", Audience: "client-id", Subject: "s", ExpiresAt: now.Add(time.Hour)}, opts); !errors.Is(err, ErrIssuerMismatch) {
		t.Fatalf("want ErrIssuerMismatch, got %v", err)
	}
	if _, err := ValidateIDToken(IDTokenClaims{Issuer: "https://issuer", Audience: "other", Subject: "s", ExpiresAt: now.Add(time.Hour)}, opts); !errors.Is(err, ErrAudienceMismatch) {
		t.Fatalf("want ErrAudienceMismatch, got %v", err)
	}
	if _, err := ValidateIDToken(IDTokenClaims{Issuer: "https://issuer", Audience: "client-id", Subject: "s", ExpiresAt: now.Add(-time.Minute)}, opts); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("want ErrTokenExpired, got %v", err)
	}
	if _, err := ValidateIDToken(base, opts); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
}

func TestNonceValidated(t *testing.T) {
	now := time.Now().UTC()
	opts := ValidateOptions{ExpectedIssuer: "i", ExpectedAudience: "c", ExpectedNonce: "n1", Now: now}
	c := IDTokenClaims{Issuer: "i", Audience: "c", Subject: "s", ExpiresAt: now.Add(time.Hour), Nonce: "n2"}
	if _, err := ValidateIDToken(c, opts); !errors.Is(err, ErrNonceMismatch) {
		t.Fatalf("want ErrNonceMismatch, got %v", err)
	}
}

func TestJITProvisioningPolicy(t *testing.T) {
	now := time.Now().UTC()
	claims := func(email string, groups []string) IDTokenClaims {
		return IDTokenClaims{Issuer: "i", Audience: "c", Subject: "s", ExpiresAt: now.Add(time.Hour), Email: email, EmailVerified: true, Groups: groups}
	}

	domainOpts := ValidateOptions{ExpectedIssuer: "i", ExpectedAudience: "c", Now: now, JITProvision: true, AllowedDomains: []string{"acme.com"}}
	if _, err := ValidateIDToken(claims("u@acme.com", nil), domainOpts); err != nil {
		t.Fatalf("allowed domain rejected: %v", err)
	}
	if _, err := ValidateIDToken(claims("u@evil.com", nil), domainOpts); !errors.Is(err, ErrJITDomainDenied) {
		t.Fatalf("want ErrJITDomainDenied, got %v", err)
	}

	groupOpts := ValidateOptions{ExpectedIssuer: "i", ExpectedAudience: "c", Now: now, JITProvision: true, AllowedGroups: []string{"staff"}}
	if _, err := ValidateIDToken(claims("u@x.com", []string{"staff"}), groupOpts); err != nil {
		t.Fatalf("allowed group rejected: %v", err)
	}
	if _, err := ValidateIDToken(claims("u@x.com", []string{"guest"}), groupOpts); !errors.Is(err, ErrJITGroupDenied) {
		t.Fatalf("want ErrJITGroupDenied, got %v", err)
	}
}

func TestValidateIDTokenMapsIdentityAssertion(t *testing.T) {
	now := time.Now().UTC()
	c := IDTokenClaims{Issuer: "https://issuer", Audience: "cid", Subject: "subj-1", Email: "u@acme.com", EmailVerified: true, Name: "U", ExpiresAt: now.Add(time.Hour)}
	a, err := ValidateIDToken(c, ValidateOptions{ExpectedIssuer: "https://issuer", ExpectedAudience: "cid", Now: now})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if a.Subject != "subj-1" || a.Issuer != "https://issuer" || a.Email != "u@acme.com" || !a.EmailVerified || a.FullName != "U" {
		t.Fatalf("unexpected assertion: %+v", a)
	}
}

func TestConfigValidation(t *testing.T) {
	valid := Config{ID: "entra", Issuer: "https://issuer", ClientID: "cid", SecretRef: "secret://auth/oidc/entra/secret"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for name, c := range map[string]Config{
		"missing issuer":    {ID: "x", ClientID: "c", SecretRef: "s"},
		"missing client":    {ID: "x", Issuer: "i", SecretRef: "s"},
		"missing secret":    {ID: "x", Issuer: "i", ClientID: "c"},
		"jit without allow": {ID: "x", Issuer: "i", ClientID: "c", SecretRef: "s", JITProvision: true},
	} {
		if err := c.Validate(); err == nil {
			t.Fatalf("%s: expected validation error", name)
		}
	}
}

func TestProfileReportsPlannedNotSupported(t *testing.T) {
	p := Config{ID: "x", Issuer: "i", ClientID: "c", SecretRef: "s"}.Profile()
	if p.Family != contract.ProviderFamilyOIDC {
		t.Fatalf("family = %q", p.Family)
	}
	if p.Status != contract.CapabilityPlanned {
		t.Fatalf("status = %q, want planned (not supported until AUTH-006)", p.Status)
	}
}

func TestSelectJWKSKeyRotatesFailClosed(t *testing.T) {
	keys := []JWKSKey{
		{KeyID: "kid-old", Issuer: "https://issuer", Active: false, Retired: true},
		{KeyID: "kid-new", Issuer: "https://issuer", Active: true},
	}

	key, err := SelectJWKSKey(keys, "kid-new", "https://issuer")
	if err != nil {
		t.Fatalf("SelectJWKSKey active: %v", err)
	}
	if key.KeyID != "kid-new" {
		t.Fatalf("selected key = %+v", key)
	}

	if _, err := SelectJWKSKey(keys, "kid-old", "https://issuer"); !errors.Is(err, ErrKeyRetired) {
		t.Fatalf("want ErrKeyRetired, got %v", err)
	}
	if _, err := SelectJWKSKey(keys, "missing", "https://issuer"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("want ErrKeyNotFound, got %v", err)
	}
	if _, err := SelectJWKSKey(keys, "kid-new", "https://other"); !errors.Is(err, ErrIssuerMismatch) {
		t.Fatalf("want ErrIssuerMismatch, got %v", err)
	}
}
