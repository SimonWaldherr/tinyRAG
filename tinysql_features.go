package main

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

// storageEncryptionKey loads a 32-byte key without persisting it in settings.
// Operators may supply hexadecimal or standard-base64 data in TINYRAG_STORAGE_KEY.
func storageEncryptionKey(enabled bool) ([]byte, error) {
	if !enabled {
		return nil, nil
	}
	raw := strings.TrimSpace(os.Getenv("TINYRAG_STORAGE_KEY"))
	if raw == "" {
		return nil, fmt.Errorf("storage encryption is enabled but TINYRAG_STORAGE_KEY is unset")
	}
	if key, err := hex.DecodeString(raw); err == nil && len(key) == tinysql.EncryptionKeySize {
		return key, nil
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) != tinysql.EncryptionKeySize {
		return nil, fmt.Errorf("TINYRAG_STORAGE_KEY must be %d bytes encoded as hex or base64", tinysql.EncryptionKeySize)
	}
	return key, nil
}

func configureTinySQLOptionalFeatures(rag *ragSystem, s appSettings, dbPath string) (func(), error) {
	configureTinySQLVectorCache(s)
	cleanup := func() {}
	if !s.TinySQLAuditEnabled {
		return cleanup, nil
	}
	path := strings.TrimSpace(s.TinySQLAuditPath)
	if path == "" {
		if strings.TrimSpace(dbPath) == "" {
			path = "tinyrag.audit.jsonl"
		} else {
			path = dbPath + ".audit.jsonl"
		}
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return cleanup, fmt.Errorf("create audit directory: %w", err)
		}
	}
	audit, err := tinysql.OpenAuditLog(path)
	if err != nil {
		return cleanup, fmt.Errorf("open tinySQL audit log: %w", err)
	}
	rag.db.AttachAuditLog(audit)
	return func() { _ = audit.Close() }, nil
}

func configureTinySQLVectorCache(s appSettings) {
	// v0.49.0 caches deterministic result IDs only; its key includes the table
	// version. Analytics retains query shape and timing, never raw vectors.
	cfg := tinysql.DefaultVectorCacheConfig()
	cfg.ResultCacheEntries = s.TinySQLVectorCacheEntries
	cfg.ResultCacheTTL = time.Duration(s.TinySQLVectorCacheTTLSeconds) * time.Second
	cfg.Analytics = s.TinySQLVectorAnalytics
	tinysql.ConfigureVectorCache(cfg)
}
