package modules

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"mimamori/app/models"
)

// scanTwin is one agent runtime of TestScanContinuationRandomized with the
// detections it reported, by key.
type scanTwin struct {
	rt     *AgentRuntime
	seen   map[string]int
	checks int
}

func (s *scanTwin) check(t *testing.T, c *RuleContent, res *ResolvedRule) {
	t.Helper()
	s.checks++
	if s.checks > 5000 {
		t.Fatal("too many checks")
	}
	for _, d := range s.rt.scanFiles(c, res, time.Now()) {
		s.seen[fmt.Sprintf("%s|%s|%d|%s", d.Type, d.Target, d.FileCtime, d.FileMode)]++
	}
}

// cover runs checks until a writable pass started after the second after
// completed, and then a whole executed-file pass from its beginning.
func (s *scanTwin) cover(t *testing.T, c *RuleContent, res *ResolvedRule, after int64) {
	t.Helper()
	for {
		var lastFull int64
		_, _ = fmt.Sscan(models.GetLocalState(s.rt.DB.Read(), stateLastFullScan), &lastFull)
		if models.GetLocalState(s.rt.DB.Read(), stateWritableResume) == "" && lastFull > after {
			break
		}
		s.check(t, c, res)
	}
	started := false
	for {
		pre := models.GetLocalState(s.rt.DB.Read(), stateExecResume)
		s.check(t, c, res)
		if pre == "" {
			started = true
		}
		if started && models.GetLocalState(s.rt.DB.Read(), stateExecResume) == "" {
			return
		}
	}
}

// sleepToNextSecond waits until the wall clock is in a later second than
// the given one.
func sleepToNextSecond(sec int64) {
	for time.Now().Unix() <= sec {
		time.Sleep(time.Until(time.Unix(sec+1, 0)) + time.Millisecond)
	}
}

// A capped scan continued over many checks reports exactly what an
// uncapped scan of the same tree reports, each detection once, while files
// change, appear and disappear between (and in the middle of) the split
// passes. The tree, the cap and the changes are random (the seed is logged).
func TestScanContinuationRandomized(t *testing.T) {
	for round := 0; round < 3; round++ {
		seed := time.Now().UnixNano() + int64(round)
		t.Run(fmt.Sprint(seed), func(t *testing.T) { scanContinuationRandom(t, seed) })
	}
}

func scanContinuationRandom(t *testing.T, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	base := t.TempDir()
	var dirs, storages, files []string
	var addDir func(dir string, depth int)
	addDir = func(dir string, depth int) {
		dirs = append(dirs, dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := rng.Intn(4); i > 0; i-- {
			p := filepath.Join(dir, fmt.Sprintf("f%d.txt", rng.Intn(1000)))
			mode := os.FileMode(0o644)
			if rng.Intn(5) == 0 {
				mode = 0o666
			}
			mkFile(t, p, mode)
			files = append(files, p)
		}
		if rng.Intn(3) == 0 || (depth == 1 && len(storages) == 0) {
			st := filepath.Join(dir, "storage")
			storages = append(storages, st)
			for i := 1 + rng.Intn(4); i > 0; i-- {
				p := filepath.Join(st, fmt.Sprintf("s%d.php", rng.Intn(1000)))
				mkFile(t, p, 0o644)
				files = append(files, p)
			}
		}
		if depth < 3 {
			n := rng.Intn(4)
			if depth == 0 {
				n = 3 + rng.Intn(3)
			}
			for i := n; i > 0; i-- {
				addDir(filepath.Join(dir, fmt.Sprintf("d%d", rng.Intn(1000))), depth+1)
			}
		}
	}
	addDir(base, 0)
	c := &RuleContent{Writable: WritableSpec{Base: base, AllowedPaths: []string{"storage"}, Category: CategoryAlert}, ScanIntervalSec: 1,
		ExecFiles: ExecFilesSpec{Extensions: []string{".php"}, OtherCategory: CategoryAlert}}
	res := &ResolvedRule{Content: c}
	refRT, done1 := openAgentDB(t)
	defer done1()
	capRT, done2 := openAgentDB(t)
	defer done2()
	limit := 2 + rng.Intn(8)
	capRT.scanLimit = limit
	ref := &scanTwin{rt: refRT, seen: map[string]int{}}
	capped := &scanTwin{rt: capRT, seen: map[string]int{}}
	t.Logf("seed %d: %d directories, %d storage directories, %d files, limit %d", seed, len(dirs), len(storages), len(files), limit)

	// Both see the initial tree completely.
	start := time.Now().Unix() - 1
	ref.check(t, c, res)
	capped.cover(t, c, res, start-1)

	// Changes between the checks of the capped runtime, in the middle of
	// its split passes. Every file changes at most once, so both runtimes
	// see every state of every file.
	changed := map[string]bool{}
	pick := func() string {
		for i := 0; i < 20; i++ {
			if p := files[rng.Intn(len(files))]; !changed[p] {
				changed[p] = true
				return p
			}
		}
		return ""
	}
	lastChange := int64(0)
	for step := 0; step < 40; step++ {
		if rng.Intn(3) == 0 {
			for n := 1 + rng.Intn(3); n > 0; n-- {
				switch rng.Intn(5) {
				case 0, 1: // the status of a file changes
					if p := pick(); p != "" {
						mode := os.FileMode(0o640)
						if rng.Intn(2) == 0 {
							mode = 0o666
						}
						if err := os.Chmod(p, mode); err != nil {
							t.Fatal(err)
						}
					}
				case 2: // a new file in an allowed directory
					if len(storages) > 0 {
						p := filepath.Join(storages[rng.Intn(len(storages))], fmt.Sprintf("n%d.php", step*10+n))
						mkFile(t, p, 0o644)
						changed[p] = true
					}
				case 3: // a new allowed directory
					st := filepath.Join(dirs[rng.Intn(len(dirs))], fmt.Sprintf("new%d", step*10+n), "storage")
					p := filepath.Join(st, "x.php")
					mkFile(t, p, 0o644)
					changed[p] = true
				case 4: // a file disappears
					if p := pick(); p != "" {
						if err := os.Remove(p); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			lastChange = time.Now().Unix()
			ref.check(t, c, res)
		}
		capped.check(t, c, res)
	}

	// Both see the final tree completely.
	sleepToNextSecond(lastChange)
	ref.check(t, c, res)
	capped.cover(t, c, res, lastChange)

	var keys []string
	for k := range ref.seen {
		keys = append(keys, k)
	}
	for k := range capped.seen {
		if _, ok := ref.seen[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if ref.seen[k] != 1 || capped.seen[k] != 1 {
			t.Errorf("%s: uncapped %d, capped %d", k, ref.seen[k], capped.seen[k])
		}
	}
	if len(ref.seen) == 0 {
		t.Fatal("nothing was reported")
	}
	t.Logf("%d detections, capped checks %d", len(keys), capped.checks)
}
