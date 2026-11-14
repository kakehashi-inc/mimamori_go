package modules

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Go and TypeScript sources, comments and tests included, and the English
// catalogs of the frontend contain only ASCII (a rule of the project):
// texts in other scripts are written as escapes.
func TestSourcesAreASCII(t *testing.T) {
	root := filepath.Join("..", "..")
	var checked int
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", ".serena":
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(p)
		english := d.Name() == "en.json" && strings.Contains(filepath.ToSlash(p), "frontend/src/")
		if ext != ".go" && ext != ".ts" && ext != ".tsx" && !english {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		checked++
		for i, line := range strings.Split(string(b), "\n") {
			for _, r := range line {
				if r > 0x7f {
					t.Errorf("%s:%d: non-ASCII character %q", p, i+1, r)
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 50 {
		t.Fatalf("only %d files checked", checked)
	}
}
