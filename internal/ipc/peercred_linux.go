//go:build linux

package ipc

import (
	"net"
	"os"
	"os/user"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// peerCred reads the credentials the kernel recorded when the peer called
// connect(2). They cannot be forged by the peer.
func peerCred(c *net.UnixConn) (PeerCred, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return PeerCred{}, err
	}
	var (
		cred *unix.Ucred
		cerr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return PeerCred{}, err
	}
	if cerr != nil {
		return PeerCred{}, cerr
	}
	return PeerCred{UID: cred.Uid, GID: cred.Gid, PID: cred.Pid}, nil
}

const (
	groupCacheTTL = 30 * time.Second
	groupCacheMax = 256
)

// groupAuthorizer allows root, the daemon's own user and the members of the
// socket group.
type groupAuthorizer struct {
	euid     uint32
	gid      uint32
	hasGroup bool

	// lookup returns the ids of every group uid belongs to. now is the clock
	// seam of the cache.
	lookup func(uid uint32) ([]string, error)
	now    func() time.Time

	mu    sync.Mutex
	cache map[uint32]groupVerdict
}

type groupVerdict struct {
	member bool
	at     time.Time
}

func newAuthorizer(gid uint32, hasGroup bool) func(PeerCred) bool {
	a := &groupAuthorizer{
		euid:     uint32(os.Geteuid()),
		gid:      gid,
		hasGroup: hasGroup,
		lookup:   lookupGroupIDs,
		now:      time.Now,
		cache:    map[uint32]groupVerdict{},
	}
	return a.authorize
}

func (a *groupAuthorizer) authorize(c PeerCred) bool {
	if c.UID == 0 || c.UID == a.euid {
		return true
	}
	if !a.hasGroup {
		return false
	}
	if c.GID == a.gid {
		return true
	}
	return a.supplementary(c.UID)
}

// supplementary reports whether uid lists the socket group among its groups
// in the user database. The answer is cached for groupCacheTTL: the lookup
// reads /etc/passwd and /etc/group, and every connection needs it.
//
// It reads the database, not the peer's live credentials: a user removed from
// the group is locked out within the TTL even if an old session still holds
// the group.
func (a *groupAuthorizer) supplementary(uid uint32) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.now()
	if v, ok := a.cache[uid]; ok && now.Sub(v.at) < groupCacheTTL {
		return v.member
	}
	member := false
	if ids, err := a.lookup(uid); err == nil {
		want := strconv.FormatUint(uint64(a.gid), 10)
		for _, id := range ids {
			if id == want {
				member = true
				break
			}
		}
	}
	if len(a.cache) >= groupCacheMax {
		clear(a.cache)
	}
	a.cache[uid] = groupVerdict{member: member, at: now}
	return member
}

// lookupGroupIDs resolves uid's groups through os/user, which parses
// /etc/passwd and /etc/group itself when built without cgo.
func lookupGroupIDs(uid uint32) ([]string, error) {
	u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return nil, err
	}
	return u.GroupIds()
}
