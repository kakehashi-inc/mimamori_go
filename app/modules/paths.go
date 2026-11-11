package modules

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// dataDirOverride is set from the global --data-dir flag (see SetDataDir).
var dataDirOverride string

// SetDataDir overrides the data directory for this process. An empty value
// keeps the default (data/ next to the executable).
func SetDataDir(dir string) {
	dataDirOverride = dir
}

// ExeDir returns the directory of the running executable. Configuration
// files are placed here.
//
// It is derived from the path the program was started with (os.Args[0]),
// following symbolic links, without reading /proc and without the PATH
// environment variable; the program is therefore started with a path (an
// absolute path, or for example ./mimamori). CheckExePath reports when that
// is not the case.
func ExeDir() string {
	return filepath.Dir(exePath())
}

func exePath() string {
	p := os.Args[0]
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return p
}

// CheckExePath fails when the program was started by name only (found
// through PATH), because its directory cannot be determined then.
func CheckExePath() error {
	if !strings.Contains(os.Args[0], "/") {
		return fmt.Errorf("start %s with its path (for example /opt/mimamori/%s or ./%s): its directory holds the configuration and data", AppName, AppName, AppName)
	}
	if _, err := os.Stat(exePath()); err != nil {
		return fmt.Errorf("cannot locate the executable %s: %w", os.Args[0], err)
	}
	return nil
}

// DataDir returns the data directory: the --data-dir flag, otherwise data/
// next to the executable. It may be a symbolic link to another place.
func DataDir() string {
	if dataDirOverride != "" {
		return dataDirOverride
	}
	return filepath.Join(ExeDir(), DataDirName)
}

// EnsureDataDir creates dir (mode 0700) when it does not exist. An existing
// directory, or the directory a symbolic link points to, is used as it is:
// its owner and mode are neither changed nor checked, and access is decided
// by the actual permissions when files are opened.
func EnsureDataDir(dir string) error {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(dir, 0o700)
	} else if err != nil {
		return err
	}
	return nil
}

// TargetDBPath returns the per-target database path for an agent row.
func TargetDBPath(dataDir, file string) string {
	return filepath.Join(dataDir, TargetDBDirName, file)
}
