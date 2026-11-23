package api

import (
	"crypto/sha256"
	"encoding/hex"
)

// StepHash names a setup step's script: its sha256, 12 hex digits. A base
// that ran a step under another hash runs it again.
func StepHash(script string) string {
	sum := sha256.Sum256([]byte(script))
	return hex.EncodeToString(sum[:])[:12]
}
