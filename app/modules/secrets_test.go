package modules

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// splitPasswordHash returns the iteration count, the salt and the key of a
// hash in the format pbkdf2-sha256$<iterations>$<salt>$<key>.
func splitPasswordHash(t *testing.T, h string) (int, []byte, []byte) {
	t.Helper()
	parts := strings.Split(h, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		t.Fatalf("hash %q is not pbkdf2-sha256$<iterations>$<salt>$<key>", h)
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("iterations of %q: %v", h, err)
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	key, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		t.Fatalf("salt or key of %q: %v %v", h, err1, err2)
	}
	return iter, salt, key
}

// The program hashes passwords with 600,000 iterations of PBKDF2-SHA256, a
// salt of 16 bytes and a key of 32 bytes, and the key is really derived with
// that count.
func TestPasswordHashProductionCount(t *testing.T) {
	if PasswordIterations != 600000 {
		t.Fatalf("PasswordIterations = %d, the documents say 600,000", PasswordIterations)
	}
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	iter, salt, key := splitPasswordHash(t, h)
	if iter != PasswordIterations || len(salt) != 16 || len(key) != 32 || !strings.HasPrefix(h, "pbkdf2-sha256$600000$") {
		t.Fatalf("hash %q: %d iterations, salt %d bytes, key %d bytes", h, iter, len(salt), len(key))
	}
	want, err := pbkdf2.Key(sha256.New, "correct horse battery", salt, PasswordIterations, 32)
	if err != nil || string(want) != string(key) {
		t.Fatalf("the key is not PBKDF2-SHA256 with %d iterations: %v", PasswordIterations, err)
	}
	if !VerifyPassword(h, "correct horse battery") {
		t.Fatal("verification of the production hash")
	}
}

// The test hook changes the count of new hashes and nothing else: the hash
// keeps its format, verification reads the count from the hash, 0 gives the
// production count back, and outside a test binary the hook refuses.
func TestPasswordHashTestHook(t *testing.T) {
	t.Cleanup(func() { UseTestPasswordIterations(0) })
	UseTestPasswordIterations(1000)
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	iter, salt, key := splitPasswordHash(t, h)
	if iter != 1000 || len(salt) != 16 || len(key) != 32 {
		t.Fatalf("hash with the hook: %q", h)
	}
	if !VerifyPassword(h, "correct horse battery") || VerifyPassword(h, "wrong") {
		t.Fatal("verification of a hash made with the hook")
	}
	// A hash of another count verifies whatever the hook says.
	other, err := hashPassword("another password", 3)
	if err != nil || !VerifyPassword(other, "another password") || VerifyPassword(other, "another passworD") {
		t.Fatalf("hash of 3 iterations %q: %v", other, err)
	}

	for _, tc := range []struct {
		what   string
		inTest bool
		n      int
	}{{"outside a test binary", false, 1000}, {"a negative count", true, -1}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", tc.what)
				}
			}()
			setTestPasswordIterations(tc.inTest, tc.n)
		}()
		if got := testPasswordIterations.Load(); got != 1000 {
			t.Fatalf("%s: the count changed to %d", tc.what, got)
		}
	}

	// 0 is the program's count again (TestPasswordHashProductionCount
	// hashes with it).
	UseTestPasswordIterations(0)
	if got := testPasswordIterations.Load(); got != 0 {
		t.Fatalf("after the reset the hook holds %d", got)
	}
}

// testOnlyHooks are the functions only tests may call, with the file that
// declares each.
var testOnlyHooks = map[string]string{
	"UseTestPasswordIterations": "app/modules/secrets.go",
}

// checkTestOnlyHooks returns a problem for every use of a name of
// testOnlyHooks in files, other than its declaration.
func checkTestOnlyHooks(fset *token.FileSet, files []*ast.File) []string {
	var problems []string
	for _, f := range files {
		file := slashPath(fset, f.Pos())
		declared := map[*ast.Ident]bool{}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && testOnlyHooks[fd.Name.Name] == file {
				declared[fd.Name] = true
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && testOnlyHooks[id.Name] != "" && !declared[id] {
				problems = append(problems, fmt.Sprintf("%s:%d: uses %s, which only tests may call", file, fset.Position(id.Pos()).Line, id.Name))
			}
			return true
		})
	}
	return problems
}

// TestTestHooksOnlyInTests reads the sources of the program: no code of the
// program refers to a test hook, so the program always hashes passwords
// with PasswordIterations.
func TestTestHooksOnlyInTests(t *testing.T) {
	fset := token.NewFileSet()
	var files []*ast.File
	for _, dir := range []string{"../models", ".", "../workers"} {
		files = append(files, parseGoFiles(t, fset, dir)...)
	}
	for _, name := range []string{"main.go", "embed.go"} {
		f, err := parser.ParseFile(fset, filepath.Join(repoRoot, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	for _, p := range checkTestOnlyHooks(fset, files) {
		t.Error(p)
	}
	if len(files) < 80 {
		t.Errorf("only %d source files were read", len(files))
	}
}

// TestTestHooksOnlyInTestsFindsUses proves that the check above fails for
// every way the program could reach a hook.
func TestTestHooksOnlyInTestsFindsUses(t *testing.T) {
	for _, tc := range []struct{ file, src, want string }{
		{"app/workers/x.go", "func f() { modules.UseTestPasswordIterations(1) }", "app/workers/x.go:3: uses UseTestPasswordIterations"},
		{"app/modules/x.go", "func f() { UseTestPasswordIterations(1) }", "app/modules/x.go:3: uses UseTestPasswordIterations"},
		{"app/modules/x.go", "var g = UseTestPasswordIterations", "uses UseTestPasswordIterations"},
		{"app/modules/x.go", "func UseTestPasswordIterations(int) {}", "uses UseTestPasswordIterations"},
		{"app/modules/secrets.go", "func init() { UseTestPasswordIterations(1) }", "app/modules/secrets.go:3: uses UseTestPasswordIterations"},
		{"app/modules/secrets.go", "func UseTestPasswordIterations(n int) {}", ""},
		{"app/modules/secrets.go", "func HashPassword(p string) {}", ""},
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, tc.file, "package x\n\n"+tc.src+"\n", parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		got := strings.Join(checkTestOnlyHooks(fset, []*ast.File{f}), "\n")
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s in %s: problems %q, want %q", tc.src, tc.file, got, tc.want)
		}
	}
}
