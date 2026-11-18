package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// markerName is the file that records that the daemon, not the user, paused
// Folding@home. FAH's own state cannot tell the two apart (CH-10).
const markerName = "paused-by-daemon"

// maxMarkerBytes bounds how much of the marker is read. The real content is
// one short word.
const maxMarkerBytes = 64

// pauseOrigin is why the daemon paused FAH, stored inside the marker so a
// restart can tell a manual pause from an automatic one.
type pauseOrigin string

const (
	// originAuto is a pause the evaluator asked for (load, battery,
	// thermal). A restart resumes it after the startup hold.
	originAuto pauseOrigin = "auto"
	// originManual is a ForcePause. A restart keeps it until ForceResume.
	originManual pauseOrigin = "manual"
)

// marker persists pause ownership and its origin across daemon restarts.
type marker struct {
	dir string
}

func (m marker) path() string {
	return filepath.Join(m.dir, markerName)
}

// present reports whether the marker exists.
func (m marker) present() (bool, error) {
	_, ok, err := m.read()
	return ok, err
}

// read reports whether the marker exists and the origin it records. Any
// entry at the path counts as present (R3-2). Only a regular file is read,
// without following symlinks; anything else, and any content other than
// "manual" (an empty marker from an older build, garbage), reads as
// originAuto, which the startup hold still guards.
func (m marker) read() (pauseOrigin, bool, error) {
	fi, err := os.Lstat(m.path())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("daemon: pause marker: %w", err)
	case !fi.Mode().IsRegular():
		return originAuto, true, nil
	}
	f, err := os.OpenFile(m.path(), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		// Replaced between Lstat and Open: still an entry at the path.
		return originAuto, true, nil
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxMarkerBytes))
	if err != nil {
		return originAuto, true, nil
	}
	if pauseOrigin(bytes.TrimSpace(raw)) == originManual {
		return originManual, true, nil
	}
	return originAuto, true, nil
}

// set writes the marker with origin. It writes a temporary file and renames
// it into place, so an existing symlink at the marker path is replaced, never
// followed.
func (m marker) set(origin pauseOrigin) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return fmt.Errorf("daemon: pause marker: %w", err)
	}
	tmp, err := os.CreateTemp(m.dir, markerName+".tmp*") // created with mode 0600
	if err != nil {
		return fmt.Errorf("daemon: pause marker: %w", err)
	}
	name := tmp.Name()
	_, werr := tmp.WriteString(string(origin) + "\n")
	if err := errors.Join(werr, tmp.Close()); err != nil {
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
