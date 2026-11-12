package modules

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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

// walkLess reports whether path a comes before path b in the order of
// filepath.WalkDir: paths are compared directory name by directory name, and
// a directory comes before its contents. (A plain string comparison is not
// that order: "a/b" is walked before "a.c".)
func walkLess(a, b string) bool {
	ca, cb := strings.Split(filepath.Clean(a), "/"), strings.Split(filepath.Clean(b), "/")
	for i := 0; i < len(ca) && i < len(cb); i++ {
		if ca[i] != cb[i] {
			return ca[i] < cb[i]
		}
	}
	return len(ca) < len(cb)
}

// isAncestor reports whether dir is a directory above p.
func isAncestor(dir, p string) bool {
	dir, p = filepath.Clean(dir), filepath.Clean(p)
	if dir == p {
		return false
	}
	if dir == "/" {
		return strings.HasPrefix(p, "/")
	}
	return strings.HasPrefix(p, dir+"/")
}

// entirelyBefore reports whether p and everything below it come before pos
// in the walk order.
func entirelyBefore(p, pos string) bool {
	return walkLess(p, pos) && !isAncestor(p, pos)
}

// resumeCheck tells a walk that continues at resume what to do with p: skip
// it (and its contents when it is a directory before resume), enter it
// without processing it (a directory above resume), or process it.
func resumeCheck(p, resume string, isDir bool) (skip, enterOnly bool) {
	if resume == "" || !walkLess(p, resume) {
		return false, false
	}
	if isDir && isAncestor(p, resume) {
		return false, true
	}
	return true, false
}

// WritableScanResult is the outcome of a writable-state scan. ResumeAt is
// the first entry not visited when the entry limit was reached.
type WritableScanResult struct {
	Findings     []FileFinding
	AllowedRoots []string
	Visited      int
	Truncated    bool
	ResumeAt     string
}

// ScanWritable walks base without following symbolic links and without
// leaving its file system. Directories matching an allowed path (a sequence
// of directory names at any depth) are recorded and skipped. Every other
// file or directory owned by one of uids, or writable by its group or by
// others, is a finding.
func ScanWritable(base string, allowed []string, uids map[int]bool, category string) (*WritableScanResult, error) {
	return ScanWritableFrom(base, allowed, uids, category, "", MaxScanEntries)
}

// ScanWritableFrom is ScanWritable continuing at resume (empty: from the
// beginning) and visiting at most limit entries. Entries before resume are
// not visited; directories above it are entered without being reported.
// When the limit is reached, Truncated is set and ResumeAt is the first
// entry not visited.
func ScanWritableFrom(base string, allowed []string, uids map[int]bool, category, resume string, limit int) (*WritableScanResult, error) {
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
		skip, enterOnly := resumeCheck(p, resume, d.IsDir())
		if skip {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !enterOnly {
			res.Visited++
			if res.Visited > limit {
				res.Truncated = true
				res.ResumeAt = p
				return errScanLimit
			}
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
		if enterOnly {
			// Visited by an earlier check of this pass.
			return nil
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
// category, all others get otherCategory. A root whose since is 0 is scanned
// for the first time: its files in legitimate locations are not reported;
// they are returned as skipped instead.
func ScanExecFiles(base string, roots []string, c *RuleContent, since func(root string) int64) (found, skipped []FileFinding, err error) {
	found, skipped, _, err = ScanExecFilesFrom(base, roots, c, func(root, _ string) int64 { return since(root) }, "", MaxScanEntries)
	return found, skipped, err
}

// ScanExecFilesFrom is ScanExecFiles with a since time for each file
// (since(root, path)), continuing at resume (empty: from the beginning) and
// visiting at most limit entries. The roots are walked in the walk order of
// their paths; roots outside base are not walked. A file whose since is 0 is
// scanned for the first time (see ScanExecFiles). When the limit is reached,
// resumeAt is the first entry not visited; it is empty when the walk reached
// the end.
func ScanExecFilesFrom(base string, roots []string, c *RuleContent, since func(root, path string) int64, resume string, limit int) (found, skipped []FileFinding, resumeAt string, err error) {
	exts := map[string]bool{}
	for _, e := range c.ExecFiles.Extensions {
		exts[e] = true
	}
	baseInfo, err := os.Lstat(base)
	if err != nil {
		return nil, nil, "", err
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
	ordered := make([]string, 0, len(roots))
	for _, r := range roots {
		if r == base || isAncestor(base, r) {
			ordered = append(ordered, r)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return walkLess(ordered[i], ordered[j]) })
	visited := 0
	for _, root := range ordered {
		if resume != "" && entirelyBefore(root, resume) {
			continue
		}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				if d != nil && d.IsDir() && p != root {
					return fs.SkipDir
				}
				return nil
			}
			skip, enterOnly := resumeCheck(p, resume, d.IsDir())
			if skip {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if !enterOnly {
				visited++
				if visited > limit {
					resumeAt = p
					return errScanLimit
				}
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
			if !exts[ext] {
				return nil
			}
			fileSince := since(root, p)
			if st.Ctim.Sec <= fileSince {
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
			if isLegit && fileSince == 0 {
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
	return found, skipped, resumeAt, nil
}
