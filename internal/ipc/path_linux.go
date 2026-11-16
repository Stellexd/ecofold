//go:build linux

package ipc

// DefaultGroup is the group whose members may use the socket.
const DefaultGroup = "ecofold"

const (
	// maxPathLen is sizeof(sockaddr_un.sun_path) on Linux.
	maxPathLen = 108
	// dirMode lets the socket group reach the socket and nobody else.
	dirMode = 0o750
	// groupSupported: the socket can be shared with a group.
	groupSupported = true
)

// DefaultPath is the daemon's socket. /run/ecofold is the systemd
// RuntimeDirectory of the unit.
func DefaultPath() (string, error) {
	return "/run/ecofold/ecofold.sock", nil
}
