package secret

import (
	"bytes"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/db"
)

func TestStoreRebindsPostgresStatementsAndUsesBytea(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	store := NewStore(database, kdb.Resolve("postgres"))

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS _kora_secret .*encrypted_value BYTEA NOT NULL`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM _kora_secret WHERE site = \$1 AND key_name = \$2`).
		WithArgs("site-a", "provider-key").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`INSERT INTO _kora_secret .*VALUES \(\$1, \$2, \$3, \$4, \$5\)`).
		WithArgs("site-a", "provider-key", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.Set("site-a", "provider-key", "secret-value"); err != nil {
		t.Fatalf("set PostgreSQL secret: %v", err)
	}

	encrypted, err := encrypt([]byte("secret-value"), deriveKey([]byte("site-a")))
	if err != nil {
		t.Fatal("encrypt fixture:", err)
	}
	mock.ExpectQuery(`SELECT encrypted_value FROM _kora_secret WHERE site = \$1 AND key_name = \$2`).
		WithArgs("site-a", "provider-key").
		WillReturnRows(sqlmock.NewRows([]string{"encrypted_value"}).AddRow(encrypted))
	if got, err := store.Get("site-a", "provider-key"); err != nil || got != "secret-value" {
		t.Fatalf("get PostgreSQL secret = %q, %v", got, err)
	}

	mock.ExpectQuery(`SELECT key_name FROM _kora_secret WHERE site = \$1 ORDER BY key_name`).
		WithArgs("site-a").
		WillReturnRows(sqlmock.NewRows([]string{"key_name"}).AddRow("provider-key"))
	if keys, err := store.List("site-a"); err != nil || len(keys) != 1 || keys[0] != "provider-key" {
		t.Fatalf("list PostgreSQL secrets = %v, %v", keys, err)
	}

	mock.ExpectExec(`DELETE FROM _kora_secret WHERE site = \$1 AND key_name = \$2`).
		WithArgs("site-a", "provider-key").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.Delete("site-a", "provider-key"); err != nil {
		t.Fatalf("delete PostgreSQL secret: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("PostgreSQL secret SQL expectations: %v", err)
	}
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	key := deriveKey([]byte("test-site-password"))

	tests := []struct {
		name      string
		plaintext string
	}{
		{
			name:      "short value",
			plaintext: "sk-hello-world",
		},
		{
			name:      "json value",
			plaintext: `{"api_key":"sk-test","endpoint":"https://api.example.com"}`,
		},
		{
			name:      "unicode value",
			plaintext: "kora-测试-тест",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encrypted, err := encrypt([]byte(tt.plaintext), key)
			if err != nil {
				t.Fatalf("encrypt() error = %v", err)
			}

			// Encrypted output should not be the same as plaintext.
			if bytes.Equal(encrypted, []byte(tt.plaintext)) {
				t.Error("encrypted output should differ from plaintext")
			}

			// Encrypted output should include nonce (starts with 12 bytes of nonce).
			if len(encrypted) <= 12 {
				t.Error("encrypted output too short, missing nonce")
			}

			decrypted, err := decrypt(encrypted, key)
			if err != nil {
				t.Fatalf("decrypt() error = %v", err)
			}

			if string(decrypted) != tt.plaintext {
				t.Errorf("decrypt() = %q, want %q", string(decrypted), tt.plaintext)
			}
		})
	}
}

func TestEncryptDecrypt_WrongKey(t *testing.T) {
	plaintext := []byte("secret-value")
	key1 := deriveKey([]byte("password1"))
	key2 := deriveKey([]byte("password2"))

	encrypted, err := encrypt(plaintext, key1)
	if err != nil {
		t.Fatalf("encrypt() error = %v", err)
	}

	_, err = decrypt(encrypted, key2)
	if err == nil {
		t.Error("decrypt with wrong key should error")
	}
}

func TestEncryptDecrypt_EmptyPlaintext(t *testing.T) {
	key := deriveKey([]byte("test-site-password"))

	tests := []struct {
		name      string
		plaintext []byte
	}{
		{"empty slice", []byte{}},
		{"nil slice", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encrypted, err := encrypt(tt.plaintext, key)
			if err != nil {
				t.Fatalf("encrypt() error = %v", err)
			}

			decrypted, err := decrypt(encrypted, key)
			if err != nil {
				t.Fatalf("decrypt() error = %v", err)
			}

			if len(decrypted) != 0 {
				t.Errorf("decrypted length = %d, want 0", len(decrypted))
			}
		})
	}
}

func TestDeriveKey_Deterministic(t *testing.T) {
	password := []byte("my-db-password")

	key1 := deriveKey(password)
	key2 := deriveKey(password)

	if !bytes.Equal(key1, key2) {
		t.Error("deriveKey should be deterministic for the same password")
	}

	if len(key1) != 32 {
		t.Errorf("deriveKey output length = %d, want 32", len(key1))
	}
}

func TestDeriveKey_DifferentPasswords(t *testing.T) {
	key1 := deriveKey([]byte("password-a"))
	key2 := deriveKey([]byte("password-b"))

	if bytes.Equal(key1, key2) {
		t.Error("deriveKey should produce different keys for different passwords")
	}
}

func TestDecrypt_CiphertextTooShort(t *testing.T) {
	key := deriveKey([]byte("test"))

	_, err := decrypt([]byte{1, 2, 3}, key)
	if err == nil {
		t.Error("decrypt with too-short ciphertext should error")
	}
}
