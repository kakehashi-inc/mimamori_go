package modules

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// FileFinding is a file detected by a scan.
type FileFinding struct {
	Path     string
	Ctime    int64
	UID      int
	GID      int
	Mode     fs.FileMode
	Category string
	Type     string
}

// Fingerprint identifies a finding together with its state, so an unchanged
// file is reported once and a changed one again.
func (f FileFinding) Fingerprint() string {
	return fmt.Sprintf("%s|%s|%d|%d|%o", f.Type, f.Path, f.Ctime, f.UID, uint32(f.Mode))
}

// ToReport converts a finding into a detection report.
func (f FileFinding) ToReport(users map[int]string, groups map[int]string, now int64) DetectionReport {
	return DetectionReport{
		Type:      f.Type,
		Category:  f.Category,
		OSUser:    NameOrID(users, f.UID),
		Target:    f.Path,
		Count:     1,
		FirstAt:   now,
		LastAt:    now,
		FileCtime: f.Ctime,
		FileOwner: NameOrID(users, f.UID) + ":" + NameOrID(groups, f.GID),
		FileMode:  f.Mode.String(),
	}
}

// errScanLimit stops a walk that visited too many entries.
var errScanLimit = errors.New("scan entry limit reached")

func statT(info fs.FileInfo) *syscall.Stat_t {
	st, _ := info.Sys().(*syscall.Stat_t)
	return st
}

func relComponents(base, p string) []string {
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == "." {
		return nil
	}
	return SplitComponents(filepath.ToSlash(rel))
}

// WritableScanResult is the outcome of a writable-state scan.
type WritableScanResult struct {
	Findings     []FileFinding
	AllowedRoots []string
	Visited      int
	Truncated    bool
}

// ScanWritable walks base without following symbolic links and without
// leaving its file system. Directories matching an allowed path (a sequence
// of directory names at any depth) are recorded and skipped. Every other
// file or directory owned by one of uids, or writable by its group or by
// others, is a finding.
func ScanWritable(base string, allowed []string, uids map[int]bool, category string) (*WritableScanResult, error) {
	res := &WritableScanResult{}
	rootInfo, err := os.Lstat(base)
	if err != nil {
		return nil, err
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", base)
	}
	rootDev := statT(rootInfo).Dev
	seqs := make([][]string, 0, len(allowed))
	for _, a := range allowed {
		seqs = append(seqs, SplitComponents(a))
	}
	err = filepath.WalkDir(base, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if p == base {
				return werr
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		res.Visited++
		if res.Visited > MaxScanEntries {
			res.Truncated = true
			return errScanLimit
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		st := statT(info)
		if st == nil {
			return nil
		}
		if d.IsDir() && st.Dev != rootDev {
			return fs.SkipDir
		}
		if p != base && d.IsDir() {
			comps := relComponents(base, p)
			for _, seq := range seqs {
				if EndsWithSeq(comps, seq) {
					res.AllowedRoots = append(res.AllowedRoots, p)
					return fs.SkipDir
				}
			}
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil
		}
		perm := info.Mode().Perm()
		if uids[int(st.Uid)] || perm&0o022 != 0 {
			res.Findings = append(res.Findings, FileFinding{
				Path: p, Ctime: st.Ctim.Sec, UID: int(st.Uid), GID: int(st.Gid),
				Mode: info.Mode(), Category: category, Type: TypeWritable,
			})
		}
		return nil
	})
	if err != nil && !errors.Is(err, errScanLimit) {
		return res, err
	}
	return res, nil
}

// ScanExecFiles walks the allowed roots for files with one of the extensions
// whose status change time (not the forgeable modification time) is newer
// than since(root). Files inside a legitimate location get that location's
// category, all others get otherCategory. When skipLegit is set (first scan)
// files in legitimate locations are not reported; they are returned as
// skipped instead.
func ScanExecFiles(base string, roots []string, c *RuleContent, since func(root string) int64, skipLegit bool) (found, skipped []FileFinding, err error) {
	exts := map[string]bool{}
	for _, e := range c.ExecFiles.Extensions {
		exts[e] = true
	}
	baseInfo, err := os.Lstat(base)
	if err != nil {
		return nil, nil, err
	}
	baseDev := statT(baseInfo).Dev
	type legit struct {
		seq  []string
		exts map[string]bool
		cat  string
	}
	var legits []legit
	for _, l := range c.ExecFiles.Legit {
		m := map[string]bool{}
		for _, e := range l.Extensions {
			m[e] = true
		}
		legits = append(legits, legit{seq: SplitComponents(l.Path), exts: m, cat: l.Category})
	}
	visited := 0
	for _, root := range roots {
		rootSince := since(root)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				if d != nil && d.IsDir() && p != root {
					return fs.SkipDir
				}
				return nil
			}
			visited++
			if visited > MaxScanEntries {
				return errScanLimit
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			st := statT(info)
			if st == nil {
				return nil
			}
			if d.IsDir() {
				if st.Dev != baseDev {
					return fs.SkipDir
				}
				return nil
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(p))
			if !exts[ext] || st.Ctim.Sec <= rootSince {
				return nil
			}
			comps := relComponents(base, filepath.Dir(p))
			cat := c.ExecFiles.OtherCategory
			isLegit := false
			for _, l := range legits {
				if l.exts[ext] && MatchSeqAt(comps, l.seq) >= 0 {
					cat, isLegit = l.cat, true
					break
				}
			}
			f := FileFinding{
				Path: p, Ctime: st.Ctim.Sec, UID: int(st.Uid), GID: int(st.Gid),
				Mode: info.Mode(), Category: cat, Type: TypeExecFile,
			}
			if isLegit && skipLegit {
				skipped = append(skipped, f)
				return nil
			}
			found = append(found, f)
			return nil
		})
		if errors.Is(err, errScanLimit) {
			break
		}
	}
	return found, skipped, nil
}
