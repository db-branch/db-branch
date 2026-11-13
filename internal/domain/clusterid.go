package domain

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
)

// ClusterIDStore keeps the cluster's anonymous identifier: 32 lowercase hex
// characters drawn at random, once, and identical on every node. It is sent
// to the licence service when the key is renewed, so that the service can
// notice a key used by more than one cluster. It is not derived from any
// secret, host or address, and not shown in the API or the console.
type ClusterIDStore interface {
	// ClusterID is "" before one is recorded.
	ClusterID(ctx context.Context) string
	// InitClusterID records id unless one is recorded already, and returns
	// the recorded one (so two racing callers end up with the same id).
	InitClusterID(ctx context.Context, id string) (string, error)
}

// ClusterIDBytes is the length of the random part; the id is twice as long in hex.
const ClusterIDBytes = 16

// ValidClusterID reports whether s is 32 lowercase hex characters.
func ValidClusterID(s string) bool {
	if len(s) != 2*ClusterIDBytes {
		return false
	}
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// NewClusterID reads ClusterIDBytes bytes from random (crypto/rand.Reader in
// production) and returns them as lowercase hex.
func NewClusterID(random io.Reader) (string, error) {
	b := make([]byte, ClusterIDBytes)
	if _, err := io.ReadFull(random, b); err != nil {
		return "", fmt.Errorf("drawing a cluster id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
