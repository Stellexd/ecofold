//go:build darwin

package ipc

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// peerCred reads the credentials the kernel recorded for the peer of a
// connected socket. They cannot be forged by the peer.
func peerCred(c *net.UnixConn) (PeerCred, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return PeerCred{}, err
	}
	var (
		cred PeerCred
		cerr error
	)
	if err := raw.Control(func(fd uintptr) {
		var x *unix.Xucred
		x, cerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if cerr != nil {
			return
		}
		cred.UID = x.Uid
		if x.Ngroups > 0 {
			cred.GID = x.Groups[0]
		}
		// The pid is only logged, so a failure here is not an error.
		if pid, err := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID); err == nil {
			cred.PID = int32(pid)
		}
	}); err != nil {
		return PeerCred{}, err
	}
	if cerr != nil {
		return PeerCred{}, cerr
	}
	return cred, nil
}

// newAuthorizer allows only the daemon's own user: the macOS daemon is
// unprivileged and per user.
func newAuthorizer(uint32, bool) func(PeerCred) bool {
	euid := uint32(os.Geteuid())
	return func(c PeerCred) bool { return c.UID == euid }
}
