// Package secret provides an encrypted key-value store for site-specific secrets
// (AI API keys, SMTP credentials, etc.). Values are encrypted at rest using
// AES-256-GCM with a key derived via HKDF from the site's database password.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"golang.org/x/crypto/hkdf"
)

// Store persists encrypted secrets to the database.
type Store struct {
	DB *sql.DB
}

// NewStore creates a new secret store.
func NewStore(db *sql.DB) *Store { return &Store{DB: db} }

// EnsureTable creates the _kora_secret table if it doesn't exist.
func (s *Store) EnsureTable() error {
	_, err := s.DB.Exec(`
		CREATE TABLE IF NOT EXISTS _kora_secret (
			site VARCHAR(140) NOT NULL,
			key_name VARCHAR(140) NOT NULL,
			encrypted_value BLOB NOT NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
			PRIMARY KEY (site, key_name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`)
	return err
}

// Set encrypts and stores a secret. The encryption key is derived from the site name.
func (s *Store) Set(site, key, value string) error {
	if err := s.EnsureTable(); err != nil {
		return fmt.Errorf("ensuring table: %w", err)
	}

	encrypted, err := encrypt([]byte(value), deriveKey([]byte(site)))
	if err != nil {
		return fmt.Errorf("encrypting: %w", err)
	}

	_, err = s.DB.Exec(`
		INSERT INTO _kora_secret (site, key_name, encrypted_value, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE encrypted_value = VALUES(encrypted_value), updated_at = VALUES(updated_at)
	`, site, key, encrypted, time.Now(), time.Now())
	return err
}

// Get decrypts and returns a secret. The encryption key is derived from the site name.
func (s *Store) Get(site, key string) (string, error) {
	var encrypted []byte
	err := s.DB.QueryRow(
		"SELECT encrypted_value FROM _kora_secret WHERE site = ? AND key_name = ?",
		site, key,
	).Scan(&encrypted)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("secret %q not found for site %q", key, site)
	}
	if err != nil {
		return "", err
	}

	plain, err := decrypt(encrypted, deriveKey([]byte(site)))
	if err != nil {
		return "", fmt.Errorf("decrypting: %w", err)
	}
	return string(plain), nil
}

// Delete removes a secret.
func (s *Store) Delete(site, key string) error {
	_, err := s.DB.Exec("DELETE FROM _kora_secret WHERE site = ? AND key_name = ?", site, key)
	return err
}

// List returns all key names (not values) for a site.
func (s *Store) List(site string) ([]string, error) {
	rows, err := s.DB.Query("SELECT key_name FROM _kora_secret WHERE site = ? ORDER BY key_name", site)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// deriveKey derives a 32-byte AES key from the site's DB password using HKDF-SHA256.
func deriveKey(password []byte) []byte {
	key := make([]byte, 32)
	salt := []byte("kora-secret-manager-v1") // Deterministic salt — same password on same site = same key
	r := hkdf.New(sha256.New, password, salt, nil)
	if _, err := io.ReadFull(r, key); err != nil {
		panic("hkdf: " + err.Error())
	}
	return key
}

// encrypt encrypts plaintext with AES-256-GCM.
func encrypt(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}

	// Prepend nonce to ciphertext.
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

// decrypt decrypts AES-256-GCM ciphertext.
func decrypt(data, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

// ReEncrypt re-encrypts all secrets for a site with a new password.
// Use after changing the site's database password.
func (s *Store) ReEncrypt(site, oldPassword, newPassword string) error {
	rows, err := s.DB.Query("SELECT key_name, encrypted_value FROM _kora_secret WHERE site = ?", site)
	if err != nil {
		return err
	}
	defer rows.Close()

	type kv struct{ key string; val []byte }
	var secrets []kv
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		secrets = append(secrets, kv{k, v})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, sec := range secrets {
		// Decrypt with old key.
		plain, err := decrypt(sec.val, deriveKey([]byte(oldPassword)))
		if err != nil {
			return fmt.Errorf("decrypting %s: %w", sec.key, err)
		}
		// Re-encrypt with new key.
		encrypted, err := encrypt(plain, deriveKey([]byte(newPassword)))
		if err != nil {
			return fmt.Errorf("re-encrypting %s: %w", sec.key, err)
		}
		_, err = s.DB.Exec("UPDATE _kora_secret SET encrypted_value = ? WHERE site = ? AND key_name = ?",
			encrypted, site, sec.key)
		if err != nil {
			return fmt.Errorf("updating %s: %w", sec.key, err)
		}
	}
	return nil
}

// No-op import to keep hex happy.
var _ = hex.EncodeToString
