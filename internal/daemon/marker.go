package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// markerName is the file that records that the daemon, not the user, paused
// Folding@home. FAH's own state cannot tell the two apart (CH-10).
const markerName = "paused-by-daemon"

// marker persists pause ownership across daemon restarts. Only the file's
// presence matters; its content is never read.
type marker struct {
	dir string
}

func (m marker) path() string {
	return filepath.Join(m.dir, markerName)
}

// present reports whether the marker exists.
func (m marker) present() (bool, error) {
	_, err := os.Lstat(m.path())
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("daemon: pause marker: %w", err)
	}
}

// set creates the marker. It writes a temporary file and renames it into
// place, so an existing symlink at the marker path is replaced, never
// followed.
func (m marker) set() error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return fmt.Errorf("daemon: pause marker: %w", err)
	}
	tmp, err := os.CreateTemp(m.dir, markerName+".tmp*") // created with mode 0600
	if err != nil {
		return fmt.Errorf("daemon: pause marker: %w", err)
	}
	name := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("daemon: pause marker: %w", err)
	}
	if err := os.Rename(name, m.path()); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("daemon: pause marker: %w", err)
	}
	return nil
}

// clear removes the marker. A missing marker is not an error.
func (m marker) clear() error {
	if err := os.Remove(m.path()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("daemon: pause marker: %w", err)
	}
	return nil
}
