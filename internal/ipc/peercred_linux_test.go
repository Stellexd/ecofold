//go:build linux

package ipc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func newTestAuthorizer(gid uint32, hasGroup bool, groups map[uint32][]string) (*groupAuthorizer, *int, *time.Time) {
	lookups := 0
	now := time.Unix(1000, 0)
	a := &groupAuthorizer{
		euid:     1000,
		gid:      gid,
		hasGroup: hasGroup,
		lookup: func(uid uint32) ([]string, error) {
			lookups++
			ids, ok := groups[uid]
			if !ok {
				return nil, errors.New("unknown user")
			}
			return ids, nil
		},
		now:   func() time.Time { return now },
		cache: map[uint32]groupVerdict{},
	}
	return a, &lookups, &now
}

func TestGroupAuthorizer(t *testing.T) {
	const ecofold = 970
	groups := map[uint32][]string{
		1001: {"1001", "970"}, // supplementary member
		1002: {"1002", "10"},  // not a member
		1004: {"1004", "9700", "97"},
	}
	a, _, _ := newTestAuthorizer(ecofold, true, groups)

	cases := []struct {
		name string
		cred PeerCred
		want bool
	}{
		{"root", PeerCred{UID: 0, GID: 0}, true},
		{"usuario del daemon", PeerCred{UID: 1000, GID: 1000}, true},
		{"grupo primario", PeerCred{UID: 1003, GID: ecofold}, true},
		{"grupo suplementario", PeerCred{UID: 1001, GID: 1001}, true},
		{"otro grupo", PeerCred{UID: 1002, GID: 1002}, false},
		{"gid parecido", PeerCred{UID: 1004, GID: 1004}, false},
		{"uid sin entrada en passwd", PeerCred{UID: 4242, GID: 4242}, false},
		{"nobody", PeerCred{UID: 65534, GID: 65534}, false},
	}
	for _, tc := range cases {
		if got := a.authorize(tc.cred); got != tc.want {
			t.Errorf("%s: authorize = %v, se esperaba %v", tc.name, got, tc.want)
		}
	}

	// Without a group only root and the daemon's user get in, and nobody is
	// looked up.
	closed, lookups, _ := newTestAuthorizer(0, false, groups)
	for _, tc := range cases {
		want := tc.cred.UID == 0 || tc.cred.UID == 1000
		if got := closed.authorize(tc.cred); got != want {
			t.Errorf("sin grupo, %s: authorize = %v, se esperaba %v", tc.name, got, want)
		}
	}
	if *lookups != 0 {
		t.Fatalf("%d consultas de grupo sin grupo configurado", *lookups)
	}
}

// TestGroupAuthorizerCache checks a verdict is reused for 30 s and then
// refreshed, so a removed user is locked out within the TTL.
func TestGroupAuthorizerCache(t *testing.T) {
	groups := map[uint32][]string{1001: {"970"}}
	a, lookups, now := newTestAuthorizer(970, true, groups)
	cred := PeerCred{UID: 1001, GID: 1001}

	for i := 0; i < 10; i++ {
		if !a.authorize(cred) {
			t.Fatal("miembro rechazado")
		}
	}
	if *lookups != 1 {
		t.Fatalf("%d consultas, se esperaba 1 (caché)", *lookups)
	}

	groups[1001] = []string{"1001"}
	*now = now.Add(groupCacheTTL - time.Second)
	if !a.authorize(cred) {
		t.Fatal("la caché expiró antes de tiempo")
	}
	*now = now.Add(2 * time.Second)
	if a.authorize(cred) {
		t.Fatal("un usuario expulsado del grupo sigue autorizado tras el TTL")
	}
	if *lookups != 2 {
		t.Fatalf("%d consultas, se esperaban 2", *lookups)
	}

	// The cache never grows without bound.
	for uid := uint32(2000); uid < 2000+3*groupCacheMax; uid++ {
		a.authorize(PeerCred{UID: uid, GID: uid})
	}
	if len(a.cache) > groupCacheMax {
		t.Fatalf("la caché tiene %d entradas, el máximo es %d", len(a.cache), groupCacheMax)
	}
}

// TestLookupGroupIDsCurrentUser checks the real lookup against this process.
func TestLookupGroupIDsCurrentUser(t *testing.T) {
	ids, err := lookupGroupIDs(uint32(os.Getuid()))
	if err != nil {
		t.Skipf("el usuario actual no está en la base de usuarios: %v", err)
	}
	want := strconv.Itoa(os.Getgid())
	for _, id := range ids {
		if id == want {
			return
		}
	}
	t.Fatalf("GroupIds = %v, falta el grupo primario %s", ids, want)
}

// TestSocketGroup shares the socket with a group the test user belongs to:
// socket 0660 and directory 0750, both owned by that group.
func TestSocketGroup(t *testing.T) {
	g, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Skipf("el grupo primario no tiene nombre: %v", err)
	}
	dir := filepath.Join(shortDir(t), "run")
	path := filepath.Join(dir, "ecofold.sock")
	s, err := Listen(Config{Path: path, Group: g.Name, Handler: &echoHandler{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Shutdown(context.Background())

	for p, want := range map[string]os.FileMode{path: 0o660, dir: 0o750} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s: modo %s, se esperaba %o", p, fi.Mode(), want)
		}
		if gid := fi.Sys().(*syscall.Stat_t).Gid; gid != uint32(os.Getgid()) {
			t.Errorf("%s: gid %d, se esperaba %d", p, gid, os.Getgid())
		}
	}
}

// TestMissingSocketGroupFailsClosed checks an unknown group does not stop the
// server: the socket stays owner-only and a warning says so.
func TestMissingSocketGroupFailsClosed(t *testing.T) {
	logs := &logBuffer{}
	path := filepath.Join(shortDir(t), "run", "ecofold.sock")
	s, err := Listen(Config{Path: path, Group: "ecofold-no-such-group", Handler: &echoHandler{},
		Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Shutdown(context.Background())

	if m := mustMode(t, path); m.Perm() != 0o600 {
		t.Fatalf("modo del socket = %s, se esperaba 0600", m)
	}
	if got := logs.String(); !strings.Contains(got, "socket group not found") {
		t.Fatalf("falta el aviso:\n%s", got)
	}
	if s.authorize(PeerCred{UID: 4242, GID: 4242}) {
		t.Fatal("un uid ajeno quedó autorizado sin grupo")
	}
}

// TestSetgidDirKeepsItsGroup mirrors the systemd unit: the directory already
// belongs to the socket group and is setgid, so the socket inherits the
// group. Listen must keep the bit and open the socket to the group.
func TestSetgidDirKeepsItsGroup(t *testing.T) {
	// A supplementary group, so inheritance and not the process's own gid
	// explains the result.
	gids, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	var group *user.Group
	for _, gid := range gids {
		if gid == os.Getgid() {
			continue
		}
		if g, err := user.LookupGroupId(strconv.Itoa(gid)); err == nil {
			group = g
			break
		}
	}
	if group == nil {
		t.Skip("el usuario no tiene un grupo suplementario con nombre")
	}
	gid, _ := strconv.Atoi(group.Gid)

	dir := filepath.Join(shortDir(t), "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, -1, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "ecofold.sock")
	s, err := Listen(Config{Path: path, Group: group.Name, Handler: &echoHandler{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Shutdown(context.Background())

	if m := mustMode(t, dir); m&os.ModeSetgid == 0 || m.Perm() != 0o750 {
		t.Fatalf("modo del directorio = %s, se esperaba 0750 con setgid", m)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Sys().(*syscall.Stat_t).Gid; got != uint32(gid) || fi.Mode().Perm() != 0o660 {
		t.Fatalf("socket: gid %d modo %s, se esperaba gid %d modo 0660", got, fi.Mode(), gid)
	}
}
