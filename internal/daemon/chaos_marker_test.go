package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestChaosMarkerSymlinkIsReplacedNotFollowed verifies that a symlink planted
// at the marker path never lets set or clear write or delete its target.
func TestChaosMarkerSymlinkIsReplacedNotFollowed(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := marker{dir: dir}
	if err := os.Symlink(target, m.path()); err != nil {
		t.Fatal(err)
	}

	if err := m.set(); err != nil {
		t.Fatalf("set: %v", err)
	}
	if fi, err := os.Lstat(m.path()); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("el marcador sigue siendo un symlink (err=%v)", err)
	}
	if err := os.Symlink(target, m.path()+".2"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(m.path()+".2", m.path()); err != nil {
		t.Fatal(err)
	}
	if err := m.clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "keep" {
		t.Fatalf("el destino del symlink cambió: %q, %v", got, err)
	}
}

// TestChaosMarkerRuntimeDirIsAFile verifies a runtime dir that is a regular
// file degrades to "not ours" with errors, never a panic or a false claim.
func TestChaosMarkerRuntimeDirIsAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := marker{dir: file}
	if ok, err := m.present(); ok || err == nil {
		t.Fatalf("present = %v, %v; se esperaba false con error", ok, err)
	}
	if err := m.set(); err == nil {
		t.Fatal("set debía fallar")
	}

	h := newHarness(t, false)
	h.d.marker = m
	h.start(t)
	if h.d.pausedByUs {
		t.Fatal("se reclamó una pausa sin poder leer el marcador")
	}
	h.ticks(1, snapLowBattery)
	if !h.d.pausedByUs || h.log.count("fah.Pause") != 1 {
		t.Fatal("la pausa debe funcionar aunque el marcador no se pueda escribir")
	}
	if !strings.Contains(h.logs.String(), "could not write the pause marker") {
		t.Fatal("falta el aviso de marcador no escribible")
	}
	h.ticks(1, snapIdle)
	if h.d.pausedByUs || h.log.count("fah.Resume") != 1 {
		t.Fatal("la pausa propia en memoria debe reanudarse aunque el marcador falle")
	}
}

// TestChaosMarkerIsADirectory verifies a directory at the marker path is
// reported (it counts as present) and that set fails loudly instead of
// silently keeping stale ownership.
func TestChaosMarkerIsADirectory(t *testing.T) {
	dir := t.TempDir()
	m := marker{dir: dir}
	if err := os.MkdirAll(filepath.Join(m.path(), "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if ok, _ := m.present(); !ok {
		t.Fatal("un directorio en la ruta del marcador debe contar como presente")
	}
	if err := m.set(); err == nil {
		t.Fatal("set sobre un directorio no vacío debía fallar")
	}
	if err := m.clear(); err == nil {
		t.Fatal("clear sobre un directorio no vacío debía fallar")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("quedó un temporal huérfano: %s", e.Name())
		}
	}
}

// TestChaosMarkerReadOnlyDir verifies a read-only runtime dir keeps the
// daemon working in memory and reports the failure.
func TestChaosMarkerReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignora los permisos del directorio")
	}
	h := newHarness(t, false)
	if err := os.Chmod(h.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.dir, 0o700) })
	h.start(t)

	h.ticks(1, snapLowBattery)
	if !h.d.pausedByUs {
		t.Fatal("la pausa confirmada debe quedar en memoria")
	}
	if ok, _ := h.d.marker.present(); ok {
		t.Fatal("no debía poder crearse el marcador")
	}
	if !strings.Contains(h.logs.String(), "could not write the pause marker") {
		t.Fatal("falta el aviso de marcador no escribible")
	}
	var pe *os.PathError
	if err := h.d.marker.set(); !errors.As(err, &pe) {
		t.Fatalf("err = %v, se esperaba *os.PathError", err)
	}
}
