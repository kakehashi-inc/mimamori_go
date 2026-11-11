package modules

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// dataDirOverride is set from the global --data-dir flag (see SetDataDir).
var dataDirOverride string

// SetDataDir overrides the data directory for this process. An empty value
// keeps the role default.
func SetDataDir(dir string) {
	dataDirOverride = dir
}

// DataDir returns the data directory: the --data-dir flag or the default.
func DataDir(defaultDir string) string {
	if dataDirOverride != "" {
		return dataDirOverride
	}
	return defaultDir
}

// EnsureSecureDir creates dir (and parents) with mode 0700 and checks that
// it is owned by the user running the program and not accessible by others.
// The owner of an existing directory is never changed: a mismatch is
// reported instead.
func EnsureSecureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	return CheckSecurePath(dir)
}

// CheckSecurePath verifies that path is owned by the user running the program
// and that group and others have no access.
func CheckSecurePath(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot read ownership", path)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s: owner uid %d, want %d", path, st.Uid, os.Geteuid())
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s: mode %04o allows access by group or others", path, fi.Mode().Perm())
	}
	return nil
}

// TargetDBPath returns the per-target database path for an agent row.
func TargetDBPath(dataDir, file string) string {
	return filepath.Join(dataDir, TargetDBDirName, file)
}
