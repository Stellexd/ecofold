package fah

import (
	"crypto/sha1" //nolint:gosec // RFC 6455 mandates SHA-1 for the accept key.
	"encoding/base64"
)

// acceptFor computes the Sec-WebSocket-Accept value for a client key.
func acceptFor(key string) string {
	sum := sha1.Sum([]byte(key + websocketGUID)) //nolint:gosec // RFC 6455.
	return base64.StdEncoding.EncodeToString(sum[:])
}
