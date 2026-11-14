package dispatch

import (
	"crypto/rand"
	"encoding/hex"
)

// randHex is n random bytes as hex.
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
