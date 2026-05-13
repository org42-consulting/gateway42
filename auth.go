package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	gocrypto "golang.org/x/crypto/pbkdf2"
)

// hashPassword creates a Werkzeug-compatible pbkdf2:sha256 hash.
func hashPassword(password string) string {
	saltBytes := make([]byte, 16)
	rand.Read(saltBytes)
	salt := hex.EncodeToString(saltBytes)
	iterations := 260000
	dk := gocrypto.Key([]byte(password), []byte(salt), iterations, 32, sha256.New)
	hash := hex.EncodeToString(dk)
	return fmt.Sprintf("pbkdf2:sha256:%d$%s$%s", iterations, salt, hash)
}

// verifyPassword checks a Werkzeug pbkdf2:sha256 hash.
func verifyPassword(hashStr, password string) bool {
	parts := strings.SplitN(hashStr, "$", 3)
	if len(parts) != 3 {
		return false
	}
	methodParts := strings.Split(parts[0], ":")
	if len(methodParts) < 3 || methodParts[0] != "pbkdf2" || methodParts[1] != "sha256" {
		return false
	}
	iterations, err := strconv.Atoi(methodParts[2])
	if err != nil || iterations <= 0 {
		return false
	}
	dk := gocrypto.Key([]byte(password), []byte(parts[1]), iterations, 32, sha256.New)
	computed := hex.EncodeToString(dk)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(parts[2])) == 1
}

// generateAPIKey returns a URL-safe random API key (~27 chars).
func generateAPIKey() string {
	b := make([]byte, 20)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func validateName(name string) bool {
	n := strings.TrimSpace(name)
	return len(n) >= 1 && len(n) <= 100
}

// validatePassword checks password complexity using unicode categories instead of regexes.
func validatePassword(password string) (bool, string) {
	if len(password) < 8 {
		return false, "Password must be at least 8 characters"
	}
	if strings.IndexFunc(password, unicode.IsUpper) < 0 {
		return false, "Password must contain at least one uppercase letter"
	}
	if strings.IndexFunc(password, unicode.IsLower) < 0 {
		return false, "Password must contain at least one lowercase letter"
	}
	if strings.IndexFunc(password, unicode.IsDigit) < 0 {
		return false, "Password must contain at least one digit"
	}
	return true, ""
}

func truncateInput(text string) string {
	if len(text) <= cfg.MaxMsgLen {
		return text
	}
	return text[:cfg.MaxMsgLen]
}
