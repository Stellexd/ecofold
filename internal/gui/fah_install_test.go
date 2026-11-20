package gui

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeSystem answers lookPath and fileExists from fixed sets.
type fakeSystem struct {
	bins  map[string]bool
	files map[string]bool
}

func (f fakeSystem) lookPath(bin string) (string, error) {
	if f.bins[bin] {
		return "/fake/bin/" + bin, nil
	}
	return "", errors.New("not found")
}

func (f fakeSystem) fileExists(p string) bool { return f.files[p] }

func set(items ...string) map[string]bool {
	m := map[string]bool{}
	for _, it := range items {
		m[it] = true
	}
	return m
}

func TestDetectFAHInstall(t *testing.T) {
	const docker = "docker run -d --name foldingathome -p 127.0.0.1:7396:7396 linuxserver/foldingathome:latest"
	cases := []struct {
		name       string
		goos       string
		sys        fakeSystem
		installed  bool
		cmd, label string
	}{
		{"macos sin nada", "darwin", fakeSystem{}, false, "", ""},
		{"macos con brew en el PATH", "darwin", fakeSystem{bins: set("brew")}, false,
			"brew install --cask folding-at-home", "Install via Homebrew"},
		{"macos con brew fuera del PATH", "darwin", fakeSystem{files: set("/opt/homebrew/bin/brew")}, false,
			"brew install --cask folding-at-home", "Install via Homebrew"},
		{"macos con la .app", "darwin", fakeSystem{bins: set("brew"), files: set("/Applications/Folding@home.app")}, true, "", ""},
		{"macos con el instalador v8", "darwin", fakeSystem{files: set("/Applications/Folding@home", "/usr/local/bin/fah-client")}, true, "", ""},
		{"macos con fah-client en el PATH", "darwin", fakeSystem{bins: set("fah-client")}, true, "", ""},
		{"linux con yay", "linux", fakeSystem{bins: set("yay", "paru", "docker")}, false,
			"yay -S foldingathome", "Install via AUR (yay)"},
		{"linux con paru", "linux", fakeSystem{bins: set("paru", "docker")}, false,
			"paru -S foldingathome", "Install via AUR (paru)"},
		{"linux con docker", "linux", fakeSystem{bins: set("docker")}, false, docker, "Run via Docker"},
		{"linux sin nada", "linux", fakeSystem{}, false, "", ""},
		{"linux instalado en /usr/bin", "linux", fakeSystem{bins: set("yay"), files: set("/usr/bin/fah-client")}, true, "", ""},
		{"linux instalado en /usr/local/bin", "linux", fakeSystem{files: set("/usr/local/bin/fah-client")}, true, "", ""},
		// The macOS paths mean nothing on Linux.
		{"linux con rutas de macos", "linux", fakeSystem{files: set("/Applications/Folding@home.app")}, false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := DetectFAHInstall(tc.goos, tc.sys.lookPath, tc.sys.fileExists)
			if g.Installed != tc.installed || g.InstallCommand != tc.cmd || g.CommandLabel != tc.label {
				t.Fatalf("guía = %+v", g)
			}
			if g.DownloadURL != "https://foldingathome.org/start-folding/" || g.Title == "" || g.Description == "" {
				t.Fatalf("guía incompleta: %+v", g)
			}
			if tc.installed != strings.Contains(g.Description, "127.0.0.1:7396") {
				t.Fatalf("descripción = %q", g.Description)
			}
		})
	}
}

func TestDetectFAHInstallHere(t *testing.T) {
	// Only checks that it runs on the real system without executing anything.
	if g := DetectFAHInstallHere(); g.DownloadURL != FAHDownloadURL {
		t.Fatalf("guía = %+v", g)
	}
}

func TestNeedsFAHSetup(t *testing.T) {
	var v View
	if v.NeedsFAHSetup() {
		t.Fatal("sin daemon no hay nada que configurar")
	}
	v.Connected = true
	if v.NeedsFAHSetup() {
		t.Fatal("antes de que el daemon consulte FAH no se sabe si responde")
	}
	v.Status.FAH.ObservedAt = time.Now()
	if !v.NeedsFAHSetup() {
		t.Fatal("FAH consultado y sin respuesta: hay que ofrecer la instalación")
	}
	v.Status.FAH.Reachable = true
	if v.NeedsFAHSetup() {
		t.Fatal("FAH responde")
	}
	v.Status.FAH.Reachable, v.Connected = false, false
	if v.NeedsFAHSetup() {
		t.Fatal("daemon desconectado")
	}
}
