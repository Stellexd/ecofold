//go:build darwin

package ipc

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultGroup is empty: on macOS the daemon runs as the user and only that
// user may connect.
const DefaultGroup = ""

const (
	// maxPathLen is sizeof(sockaddr_un.sun_path) on macOS.
	maxPathLen = 104
	dirMode    = 0o700
	// groupSupported: the socket is never shared with a group on macOS.
	groupSupported = false
)

// DefaultPath is per user, next to the pause marker. There is no fallback to
// a shared temporary directory.
func DefaultPath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("ipc: no user cache directory for the socket: %w", err)
	}
	return filepath.Join(dir, "ecofold", "ecofold.sock"), nil
}
