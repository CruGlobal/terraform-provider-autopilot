package client

import (
	"crypto/sha256"
	"encoding/hex"
)

// fingerprintPrefix is what AutoPilot puts before a secret when it hashes it
// for a fingerprint.
const fingerprintPrefix = "autopilot-fingerprint:"

// Fingerprint is how AutoPilot reports a sender's secret without showing it:
// the first 16 hex characters of the SHA-256 of "autopilot-fingerprint:"
// followed by the secret. The provider works out the same from its
// configuration, so it can tell that a secret changed although AutoPilot
// never sends one back and Terraform never stores one.
func Fingerprint(secret string) string {
	sum := sha256.Sum256([]byte(fingerprintPrefix + secret))
	return hex.EncodeToString(sum[:])[:16]
}
