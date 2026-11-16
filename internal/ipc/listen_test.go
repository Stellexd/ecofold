package ipc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func quietConfig(path string) Config {
	return Config{Path: path, Handler: &echoHandler{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func mustMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	return fi.Mode()
}

// TestListenCreatesProtectedDirAndSocket checks the modes right after Listen,
// before any Accept: nothing is ever reachable with looser permissions.
func TestListenCreatesProtectedDirAndSocket(t *testing.T) {
	dir := filepath.Join(shortDir(t), "run")
	path := filepath.Join(dir, "ecofold.sock")

	// A permissive umask must not leak into the socket or the directory.
	old := umaskForTest(0)
	s, err := Listen(quietConfig(path))
	umaskForTest(old)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Shutdown(context.Background())

	if m := mustMode(t, path); m&os.ModeSocket == 0 || m.Perm() != 0o600 {
		t.Fatalf("modo del socket = %s, se esperaba un socket 0600", m)
	}
	if m := mustMode(t, dir); !m.IsDir() || m.Perm() != dirMode {
		t.Fatalf("modo del directorio = %s, se esperaba %o", m, dirMode)
	}
	if got := umaskForTest(old); got != old {
		t.Fatalf("Listen dejó el umask en %o, se esperaba %o", got, old)
	}
}

// TestListenTightensExistingDir checks a directory that already exists with
// loose permissions is fixed.
func TestListenTightensExistingDir(t *testing.T) {
	dir := filepath.Join(shortDir(t), "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	s, err := Listen(quietConfig(filepath.Join(dir, "ecofold.sock")))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Shutdown(context.Background())
	if m := mustMode(t, dir); m.Perm() != dirMode {
		t.Fatalf("modo del directorio = %s, se esperaba %o", m, dirMode)
	}
}

// TestListenRejectsBadDirs checks the directory must be a real directory with
// an existing parent.
func TestListenRejectsBadDirs(t *testing.T) {
	base := shortDir(t)
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for name, dir := range map[string]string{
		"symlink":         link,
		"archivo regular": file,
		"padre ausente":   filepath.Join(base, "missing", "run"),
	} {
		if s, err := Listen(quietConfig(filepath.Join(dir, "ecofold.sock"))); err == nil {
			_ = s.Shutdown(context.Background())
			t.Errorf("%s: Listen aceptó el directorio %s", name, dir)
		}
	}
	if entries, _ := os.ReadDir(real); len(entries) != 0 {
		t.Fatalf("se creó un socket a través del symlink: %v", entries)
	}
}

// TestListenRejectsBadPaths checks the path validation.
func TestListenRejectsBadPaths(t *testing.T) {
	dir := shortDir(t)
	long := filepath.Join(dir, strings.Repeat("s", maxPathLen))
	if _, err := Listen(quietConfig(long)); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("ruta larga: err = %v", err)
	}
	// One byte under the limit is the longest path that fits.
	fits := filepath.Join(dir, strings.Repeat("s", maxPathLen-2-len(dir)))
	if len(fits) != maxPathLen-1 {
		t.Fatalf("ruta de prueba de %d bytes", len(fits))
	}
	s, err := Listen(quietConfig(fits))
	if err != nil {
		t.Fatalf("ruta de %d bytes: %v", len(fits), err)
	}
	_ = s.Shutdown(context.Background())

	if _, err := Listen(quietConfig("ecofold.sock")); err == nil {
		t.Fatal("Listen aceptó una ruta relativa")
	}
	if _, err := Listen(Config{Path: filepath.Join(dir, "x.sock")}); err == nil {
		t.Fatal("Listen aceptó un Handler nil")
	}
}

// TestStaleSocketIsReplaced leaves a dead socket behind, as a SIGKILL would,
// and checks the next Listen takes the path over.
func TestStaleSocketIsReplaced(t *testing.T) {
	path := filepath.Join(shortDir(t), "ecofold.sock")
	old, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	old.SetUnlinkOnClose(false)
	_ = old.Close()
	if mustMode(t, path)&os.ModeSocket == 0 {
		t.Fatal("no quedó un socket viejo")
	}

	ts := startServerAt(t, path, &echoHandler{}, nil)
	rc := dialRaw(t, ts.Path())
	rc.send([]byte(`{"v":1,"id":"x","cmd":"status"}` + "\n"))
	if resp := rc.recv(); !resp.OK {
		t.Fatalf("respuesta = %+v", resp)
	}
}

// TestSymlinkAtSocketPathIsRefused checks Listen neither follows nor removes
// a symlink, whatever it points to.
func TestSymlinkAtSocketPathIsRefused(t *testing.T) {
	dir := shortDir(t)
	target := filepath.Join(dir, "precious")
	if err := os.WriteFile(target, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadSock := filepath.Join(dir, "dead.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: deadSock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	_ = l.Close()

	for name, dest := range map[string]string{
		"a un archivo":      target,
		"a un socket viejo": deadSock,
		"colgante":          filepath.Join(dir, "nowhere"),
	} {
		path := filepath.Join(dir, "ecofold.sock")
		if err := os.Symlink(dest, path); err != nil {
			t.Fatal(err)
		}
		if s, err := Listen(quietConfig(path)); err == nil {
			_ = s.Shutdown(context.Background())
			t.Fatalf("symlink %s: Listen lo aceptó", name)
		}
		if mustMode(t, path)&os.ModeSymlink == 0 {
			t.Fatalf("symlink %s: Listen lo reemplazó", name)
		}
		_ = os.Remove(path)
	}

	if got, err := os.ReadFile(target); err != nil || string(got) != "do not touch" {
		t.Fatalf("el destino del symlink cambió: %q, %v", got, err)
	}
	if mustMode(t, deadSock)&os.ModeSocket == 0 {
		t.Fatal("el socket destino del symlink se borró")
	}
}

// TestNonSocketAtSocketPathIsRefused checks a regular file or a directory at
// the path is an error and is left in place.
func TestNonSocketAtSocketPathIsRefused(t *testing.T) {
	dir := shortDir(t)

	file := filepath.Join(dir, "file.sock")
	if err := os.WriteFile(file, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(quietConfig(file)); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("archivo regular: err = %v", err)
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "data" {
		t.Fatalf("el archivo regular cambió: %q, %v", got, err)
	}

	sub := filepath.Join(dir, "dir.sock")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(quietConfig(sub)); err == nil {
		t.Fatal("directorio: Listen lo aceptó")
	}
	if !mustMode(t, sub).IsDir() {
		t.Fatal("el directorio se borró")
	}
}

// TestSecondDaemonIsRefused checks a live socket is never stolen.
func TestSecondDaemonIsRefused(t *testing.T) {
	ts := startServer(t, &echoHandler{}, nil)

	_, err := Listen(quietConfig(ts.Path()))
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("err = %v, se esperaba ErrAlreadyRunning", err)
	}

	rc := dialRaw(t, ts.Path())
	rc.send([]byte(`{"v":1,"id":"x","cmd":"status"}` + "\n"))
	if resp := rc.recv(); !resp.OK {
		t.Fatalf("el primer daemon dejó de responder: %+v", resp)
	}
}
