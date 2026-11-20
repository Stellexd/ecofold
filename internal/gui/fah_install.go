package gui

import (
	"os"
	"os/exec"
	"runtime"
)

// FAHDownloadURL is the official Folding@home download page.
const FAHDownloadURL = "https://foldingathome.org/start-folding/"

// FAHInstallGuide tells the user how to get Folding@home v8 running when the
// daemon cannot reach it. EcoFold never installs or starts it: the guide only
// hands over the official link and a command to copy.
type FAHInstallGuide struct {
	// Installed is true when a Folding@home client was found on disk.
	Installed   bool
	Title       string
	Description string
	DownloadURL string
	// InstallCommand is a command for the user to copy and run, empty when
	// Folding@home is installed or no known package manager was found.
	InstallCommand string
	// CommandLabel names InstallCommand in a few words.
	CommandLabel string
}

// fahPaths are the places a Folding@home v8 client is installed, besides the
// PATH. On macOS the v8 installer puts the client in /usr/local/bin and a
// folder of links in /Applications.
var fahPaths = map[string][]string{
	"darwin": {"/Applications/Folding@home.app", "/Applications/Folding@home", "/usr/local/bin/fah-client"},
	"linux":  {"/usr/bin/fah-client", "/usr/local/bin/fah-client"},
}

// brewPaths are Homebrew's binaries on Apple Silicon and Intel Macs. An app
// opened from Finder does not get the shell's PATH, so they are checked too.
var brewPaths = []string{"/opt/homebrew/bin/brew", "/usr/local/bin/brew"}

// DetectFAHInstall builds the guide for goos. lookPath and fileExists are
// the only contact with the system, so the caller decides when it runs:
// nothing is executed, only looked up.
func DetectFAHInstall(goos string, lookPath func(string) (string, error), fileExists func(string) bool) FAHInstallGuide {
	has := func(bin string, paths ...string) bool {
		if _, err := lookPath(bin); err == nil {
			return true
		}
		for _, p := range paths {
			if fileExists(p) {
				return true
			}
		}
		return false
	}

	g := FAHInstallGuide{DownloadURL: FAHDownloadURL}
	g.Installed = has("fah-client", fahPaths[goos]...)
	if g.Installed {
		g.Title = "Folding@home Is Not Running"
		g.Description = "Folding@home is installed, but its service does not answer on 127.0.0.1:7396. " +
			"Open Folding@home or start its service; this window notices by itself."
		return g
	}

	g.Title = "Folding@home Not Installed"
	g.Description = "EcoFold drives Folding@home v8, the engine that does the science, and it was not " +
		"found on this system. Install it from the official site or with the command below."
	switch goos {
	case "darwin":
		if has("brew", brewPaths...) {
			g.InstallCommand, g.CommandLabel = "brew install --cask folding-at-home", "Install via Homebrew"
		}
	case "linux":
		switch {
		case has("yay"):
			g.InstallCommand, g.CommandLabel = "yay -S foldingathome", "Install via AUR (yay)"
		case has("paru"):
			g.InstallCommand, g.CommandLabel = "paru -S foldingathome", "Install via AUR (paru)"
		case has("docker"):
			// Published on loopback only: the API controls folding and must
			// not be reachable from the network.
			g.InstallCommand = "docker run -d --name foldingathome -p 127.0.0.1:7396:7396 linuxserver/foldingathome:latest"
			g.CommandLabel = "Run via Docker"
		}
	}
	if g.InstallCommand == "" {
		g.Description = "EcoFold drives Folding@home v8, the engine that does the science, and it was not " +
			"found on this system. Download it from the official site."
	}
	return g
}

// DetectFAHInstallHere is DetectFAHInstall for this system.
func DetectFAHInstallHere() FAHInstallGuide {
	return DetectFAHInstall(runtime.GOOS, exec.LookPath, func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	})
}

// NeedsFAHSetup reports whether the UI should offer the Folding@home setup:
// the daemon is up and asked Folding@home, which did not answer.
func (v View) NeedsFAHSetup() bool {
	return v.Connected && v.fahUnreachable()
}
