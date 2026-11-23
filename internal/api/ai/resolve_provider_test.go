package ai

import (
	"database/sql/driver"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/asenawritescode/kora/internal/envcompat"
	"github.com/asenawritescode/kora/internal/secret"
)

// capturedBytes is a sqlmock argument matcher that records the []byte argument it matches, so a test can read back
// the ciphertext secret.Store wrote.
type capturedBytes struct{ into *[]byte }

func (c capturedBytes) Match(v driver.Value) bool {
	b, ok := v.([]byte)
	if ok {
		*c.into = append([]byte(nil), b...)
	}
	return ok
}

// encryptSiteKey stores value as the site-a secret for providerKey through secret.Store, against a sqlmock, and
// returns the ciphertext that was written.
func encryptSiteKey(t *testing.T, providerKey, value string) []byte {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	var ciphertext []byte
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS _kora_secret")).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM _kora_secret")).WithArgs("site-a", providerKey).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_secret")).
		WithArgs("site-a", providerKey, capturedBytes{into: &ciphertext}, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := secret.NewStore(db).Set("site-a", providerKey, value); err != nil {
		t.Fatalf("storing site key: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	return ciphertext
}

// expectSiteSecretLookups registers the Get for each direct provider in order. The provider named by keyed returns
// the ciphertext; every other provider has no row.
func expectSiteSecretLookups(mock sqlmock.Sqlmock, keyed string, ciphertext []byte) {
	for _, p := range directProviders {
		rows := sqlmock.NewRows([]string{"encrypted_value"})
		if p.key == keyed {
			rows.AddRow(ciphertext)
		}
		mock.ExpectQuery(regexp.QuoteMeta("SELECT encrypted_value FROM _kora_secret")).
			WithArgs("site-a", p.key).WillReturnRows(rows)
		if p.key == keyed {
			return
		}
	}
}

func TestResolveProviderTable(t *testing.T) {
	const (
		siteKey     = "sk-site-secret"
		cloudToken  = "engine-cloud-token"
		identityURL = "https://cloud.test/internal/identity/handoff/redeem"
	)
	tests := []struct {
		name        string
		gatewayURL  string
		identityURL string
		token       string
		// siteKey stores a Secrets key for openai on site-a.
		siteKey bool
		// wantSiteRead is true when resolution reads the site's Secrets.
		wantSiteRead bool
		wantProvider string
		wantKey      string
		wantBaseURL  string
		wantModel    string
	}{
		{
			name:         "explicit gateway env wins over identity URL",
			gatewayURL:   "http://gateway.test/internal/engine/ai/",
			identityURL:  identityURL,
			token:        cloudToken,
			wantProvider: "kora_cloud", wantKey: cloudToken,
			wantBaseURL: "http://gateway.test/internal/engine/ai", wantModel: "cloud-managed",
		},
		{
			name:         "gateway derived from identity URL with path",
			identityURL:  identityURL,
			token:        cloudToken,
			wantProvider: "kora_cloud", wantKey: cloudToken,
			wantBaseURL: "https://cloud.test/internal/engine/ai", wantModel: "cloud-managed",
		},
		{
			name:         "gateway derived from identity URL keeps the port",
			identityURL:  "http://127.0.0.1:8081/internal/identity/handoff/redeem",
			token:        cloudToken,
			wantProvider: "kora_cloud", wantKey: cloudToken,
			wantBaseURL: "http://127.0.0.1:8081/internal/engine/ai", wantModel: "cloud-managed",
		},
		{
			name:         "neither set with a site key uses the site key",
			siteKey:      true,
			wantSiteRead: true,
			wantProvider: "openai_api_key", wantKey: "sk-site-secret",
			wantBaseURL: "https://api.openai.com/v1", wantModel: "gpt-4o",
		},
		{
			name:         "neither set and no site key resolves nothing",
			wantSiteRead: true,
		},
		{
			name:        "identity URL set without token resolves nothing and reads no site key",
			identityURL: identityURL,
			siteKey:     true,
		},
		{
			name:        "identity URL invalid resolves nothing and reads no site key",
			identityURL: "cloud.test/no-scheme",
			token:       cloudToken,
			siteKey:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KORA_CLOUD_AI_GATEWAY_URL", tt.gatewayURL)
			t.Setenv("KORA_ENGINE_CLOUD_IDENTITY_URL", tt.identityURL)
			t.Setenv(envcompat.IntegrationTokenEnv, tt.token)
			t.Setenv("KORA_CLOUD_AI_GATEWAY_MODEL", "")

			var ciphertext []byte
			if tt.siteKey {
				ciphertext = encryptSiteKey(t, "openai_api_key", "sk-site-secret")
			}
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			// Registering the lookups makes a skipped read visible: the expectations stay unmet.
			if tt.siteKey || tt.wantSiteRead {
				keyed := ""
				if tt.siteKey {
					keyed = "openai_api_key"
				}
				expectSiteSecretLookups(mock, keyed, ciphertext)
			}

			providerKey, apiKey, baseURL, model := resolveProvider(db, "site-a", "")
			if providerKey != tt.wantProvider || apiKey != tt.wantKey || baseURL != tt.wantBaseURL || model != tt.wantModel {
				t.Fatalf("resolveProvider = %q %q %q %q, want %q %q %q %q",
					providerKey, apiKey, baseURL, model, tt.wantProvider, tt.wantKey, tt.wantBaseURL, tt.wantModel)
			}
			if tt.siteKey || tt.wantSiteRead {
				readSite := mock.ExpectationsWereMet() == nil
				if readSite != tt.wantSiteRead {
					t.Fatalf("site Secrets read = %v, want %v", readSite, tt.wantSiteRead)
				}
			}
		})
	}
}
