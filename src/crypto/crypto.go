package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// Encrypt encrypts plaintext using AES-256-GCM with the provided hex-encoded key.
// Returns base64-encoded ciphertext with prepended nonce.
// If key is empty, returns plaintext unchanged (for backward compatibility).
func Encrypt(plaintext, hexKey string) (string, error) {
	if hexKey == "" || plaintext == "" {
		return plaintext, nil
	}

	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return "", fmt.Errorf("invalid encryption key: %w", err)
	}
	if len(key) != 32 {
		return "", fmt.Errorf("encryption key must be 32 bytes (64 hex chars), got %d bytes", len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return "enc:" + base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt decrypts base64-encoded ciphertext using AES-256-GCM.
// If the value doesn't have the "enc:" prefix, it's treated as plaintext (migration support).
func Decrypt(ciphertext, hexKey string) (string, error) {
	if hexKey == "" || ciphertext == "" {
		return ciphertext, nil
	}

	// Not encrypted yet (plaintext from before migration)
	if !strings.HasPrefix(ciphertext, "enc:") {
		return ciphertext, nil
	}

	encoded := strings.TrimPrefix(ciphertext, "enc:")

	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return "", fmt.Errorf("invalid encryption key: %w", err)
	}

	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("failed to decode ciphertext: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertextBytes := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertextBytes, nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt: %w", err)
	}

	return string(plaintext), nil
}
