package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// API keys for external (non-browser) clients of /api/ask and /api/search
// (see handlers.go's requireAPIKey) — a separate credential from the
// LDAP-session cookie the bundled browser UI uses, so another system can
// call R3's JSON API without going through an interactive AD login.
//
// Only a SHA-256 hash of each key is ever persisted in settings.json; the
// plaintext is generated once, returned to the caller of
// handleCreateAPIKey, and never stored or shown again — the same
// "show-once" pattern as a GitHub personal access token or an AWS secret
// key, chosen because unlike a password, an API key is a bearer
// credential presented on every request, so a leaked settings.json
// shouldn't hand over every live key.
// ─────────────────────────────────────────────────────────────────────────────

// apiKeyRecord is one issued key. Prefix is kept in the clear (it's not
// sensitive on its own) purely so an admin can tell keys apart in a list
// without ever seeing the full plaintext again.
type apiKeyRecord struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	Hash       string `json:"hash"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
	Enabled    bool   `json:"enabled"`
}

// hashAPIKey computes the SHA-256 hash stored in settings.json in place of
// the plaintext key — see the show-once rationale above.
func hashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// generateAPIKey creates a new random key and its record. The returned
// plaintext must be handed to the caller immediately — nothing else
// retains it.
func generateAPIKey(name string) (plaintext string, rec apiKeyRecord, err error) {
	buf := make([]byte, 24)
	if _, err = rand.Read(buf); err != nil {
		return "", apiKeyRecord{}, fmt.Errorf("generate api key: %w", err)
	}
	plaintext = "r3_" + hex.EncodeToString(buf)
	rec = apiKeyRecord{
		ID:        hex.EncodeToString(buf[:8]),
		Name:      name,
		Prefix:    plaintext[:11], // "r3_" + 8 hex chars — enough to tell keys apart, not enough to brute-force
		Hash:      hashAPIKey(plaintext),
		CreatedAt: time.Now().Unix(),
		Enabled:   true,
	}
	return plaintext, rec, nil
}

// findAPIKey returns the matching enabled record for a presented
// plaintext key. Hash comparison is constant-time so response timing
// can't be used to fish for a valid key.
func findAPIKey(keys []apiKeyRecord, presented string) (apiKeyRecord, bool) {
	if presented == "" {
		return apiKeyRecord{}, false
	}
	h := []byte(hashAPIKey(presented))
	for _, k := range keys {
		if !k.Enabled {
			continue
		}
		if subtle.ConstantTimeCompare(h, []byte(k.Hash)) == 1 {
			return k, true
		}
	}
	return apiKeyRecord{}, false
}
