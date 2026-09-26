// Package ids generates the random identifiers used for services,
// deployments and log lines.
package ids

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns a random 16-char hex id.
func New() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
