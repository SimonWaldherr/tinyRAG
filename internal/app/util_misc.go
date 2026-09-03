package app

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

func vecJSON(v []float64) string {
	b, err := json.Marshal(v)
	if err != nil {
		// This should never happen with float64 slices, but handle it anyway
		log.Printf("Warning: failed to marshal vector: %v", err)
		return "[]"
	}
	return string(b)
}

// escapeSQ escapes single quotes for safe SQL insertion.
func escapeSQ(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// storageModeLabel returns a short string label for a tinySQL storage mode.
func storageModeLabel(mode tinysql.StorageMode) string {
	switch mode {
	case tinysql.ModeMemory:
		return "memory"
	case tinysql.ModeWAL:
		return "wal"
	case tinysql.ModeDisk:
		return "disk"
	case tinysql.ModeIndex:
		return "index"
	case tinysql.ModeHybrid:
		return "hybrid"
	default:
		return "legacy"
	}
}

// newRequestID generates a short random request identifier.
func newRequestID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err == nil {
		return fmt.Sprintf("req-%x", b)
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}
