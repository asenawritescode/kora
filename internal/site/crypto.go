package site

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
)

// serverSecretKey derives a 32-byte AES key from the KORA_SECRET_KEY env var.
// Returns an error if KORA_SECRET_KEY is not set.
func serverSecretKey() ([]byte, error) {
	key := os.Getenv("KORA_SECRET_KEY")
	if key == "" {
		return nil, fmt.Errorf("KORA_SECRET_KEY environment variable is not set")
	}
	h := sha256.Sum256([]byte(key))
	return h[:], nil
}

// encryptPassword encrypts a plaintext password using AES-256-GCM with a random nonce.
// Returns the hex-encoded ciphertext (nonce + ciphertext).
func encryptPassword(plaintext string) (string, error) {
	key, err := serverSecretKey()
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("aes: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("gcm: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}

	// nonce + ciphertext
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return hex.EncodeToString(ciphertext), nil
}

// decryptPassword decrypts a hex-encoded ciphertext produced by encryptPassword.
// Returns the plaintext password.
func decryptPassword(cipherHex string) (string, error) {
	key, err := serverSecretKey()
	if err != nil {
		return "", err
	}

	data, err := hex.DecodeString(cipherHex)
	if err != nil {
		return "", fmt.Errorf("hex decode: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("aes: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("gcm: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}

	return string(plaintext), nil
}
