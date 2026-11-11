package modules

import (
	"errors"
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

// exeDir returns the directory of the running executable, derived from the
// path the program was started with (os.Args[0], following symbolic links,
// without reading /proc and without the PATH environment variable). ok is
// false when it cannot be determined (for example when the program was
// started by name only).
func exeDir() (dir string, ok bool) {
	p := os.Args[0]
	if !strings.Contains(p, "/") {
		return "", false
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	if fi, err := os.Stat(p); err != nil || fi.IsDir() {
		return "", false
	}
	return filepath.Dir(p), true
}

// DataDir returns the data directory: the --data-dir flag, otherwise data/
// next to the executable, or data/ in the current directory when the
// executable's directory cannot be determined.
func DataDir() string {
	if dataDirOverride != "" {
		return dataDirOverride
	}
	if dir, ok := exeDir(); ok {
		return filepath.Join(dir, DataDirName)
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, DataDirName)
	}
	return DataDirName
}

// EnsureDataDir creates dir (mode 0700) when it does not exist. An existing
// one is used as it is: nothing about it is changed or checked, and access
// is decided by the actual permissions when files are opened.
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
