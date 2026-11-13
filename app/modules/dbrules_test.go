package modules

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The tests in this file read the sources of the program and keep the
// rules of the database handle (see DB) for code no test runs: nothing
// outside the handle reaches a connection of its own, and a write
// transaction neither asks for a database again nor waits on the outside.

// The project's import paths the checks below resolve calls through.
const (
	importModels  = "mimamori/app/models"
	importModules = "mimamori/app/modules"
	importWorkers = "mimamori/app/workers"
)

// parseGoFiles parses the .go files of dir (without the tests, which may
// open raw connections on purpose to play another process).
func parseGoFiles(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()
	dir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

// importNames returns, for each import path of f, the name the file uses
// for it.
func importNames(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, im := range f.Imports {
		p, err := strconv.Unquote(im.Path.Value)
		if err != nil {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if im.Name != nil {
			name = im.Name.Name
		}
		out[p] = name
	}
	return out
}

// repoRoot is the repository root as an absolute path (the tests run in
// app/modules).
var repoRoot, _ = filepath.Abs(filepath.Join("..", ".."))

// slashPath returns the name of the file at pos relative to the repository
// root, with forward slashes, so that the allowed files can be named the
// same way on every system. The sources are parsed under absolute names;
// a name that is not absolute (the cases of the tests) is kept.
func slashPath(fset *token.FileSet, pos token.Pos) string {
	name := fset.Position(pos).Filename
	if filepath.IsAbs(name) {
		if rel, err := filepath.Rel(repoRoot, name); err == nil {
			name = rel
		}
	}
	return filepath.ToSlash(name)
}

// rawDatabaseNames are the names of package database/sql that give a
// connection of its own or keep one: only the handle uses them.
var rawDatabaseNames = map[string]bool{
	"DB": true, "Tx": true, "Conn": true, "Stmt": true, "Rows": true,
	"Open": true, "OpenDB": true,
}

// rawDatabaseAllowed lists the files that may use rawDatabaseNames: the
// handle itself, and models/common.go for *sql.Rows (the Reader interface
// and collect, which reads the rows to the end inside the package).
var rawDatabaseAllowed = map[string]map[string]bool{
	"app/modules/db.go":       nil,
	"app/modules/dbhandle.go": nil,
	"app/models/common.go":    {"Rows": true},
}

// checkNoRawDatabase returns a problem for every use of rawDatabaseNames
// and every call of a method Begin or BeginTx outside the files of
// rawDatabaseAllowed.
func checkNoRawDatabase(fset *token.FileSet, files []*ast.File) []string {
	var problems []string
	for _, f := range files {
		file := slashPath(fset, f.Pos())
		allowed, inHandle := rawDatabaseAllowed[file]
		if inHandle && allowed == nil {
			continue
		}
		sqlName, usesSQL := importNames(f)["database/sql"]
		if usesSQL && (sqlName == "." || sqlName == "_") {
			problems = append(problems, fmt.Sprintf("%s: imports database/sql as %q", file, sqlName))
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && usesSQL && x.Name == sqlName && rawDatabaseNames[n.Sel.Name] && !allowed[n.Sel.Name] {
					problems = append(problems, fmt.Sprintf("%s:%d: uses sql.%s; only the database handle (app/modules/db.go, dbhandle.go) reaches connections", file, fset.Position(n.Pos()).Line, n.Sel.Name))
				}
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Begin" || sel.Sel.Name == "BeginTx") {
					problems = append(problems, fmt.Sprintf("%s:%d: calls %s; a write transaction is begun only by DB.Write", file, fset.Position(n.Pos()).Line, sel.Sel.Name))
				}
			}
			return true
		})
	}
	return problems
}

// TestNoRawDatabaseOutsideHandle reads the sources of the program and
// checks rule R6 of the handle: no code outside app/modules/db.go and
// dbhandle.go opens, keeps or begins anything of database/sql itself, so
// every access goes through Read or Write. Rows appear only in
// app/models/common.go, where they are read to the end before a function
// returns.
func TestNoRawDatabaseOutsideHandle(t *testing.T) {
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
	for _, p := range checkNoRawDatabase(fset, files) {
		t.Error(p)
	}
	// The count guards the test itself: if the sources were no longer
	// found (a moved directory), it would pass in vain.
	if len(files) < 80 {
		t.Errorf("only %d source files were read", len(files))
	}
}

// TestNoRawDatabaseFindsUses proves that the check above fails for each way
// code could reach a connection outside the handle.
func TestNoRawDatabaseFindsUses(t *testing.T) {
	cases := []struct {
		file, src, want string
	}{
		{"app/workers/x.go", "import \"database/sql\"\nvar _ = sql.ErrNoRows", ""},
		{"app/workers/x.go", "import \"database/sql\"\nfunc f() { sql.Open(\"sqlite\", \"x\") }", "uses sql.Open"},
		{"app/workers/x.go", "import \"database/sql\"\ntype c struct{ db *sql.DB }", "uses sql.DB"},
		{"app/workers/x.go", "import q \"database/sql\"\nfunc f(tx *q.Tx) {}", "uses sql.Tx"},
		{"app/workers/x.go", "import \"database/sql\"\nfunc f() (*sql.Rows, error) { return nil, nil }", "uses sql.Rows"},
		{"app/models/x.go", "import \"database/sql\"\nfunc f(r *sql.Rows) {}", "uses sql.Rows"},
		{"app/models/common.go", "import \"database/sql\"\nfunc f(r *sql.Rows) {}", ""},
		{"app/models/common.go", "import \"database/sql\"\nfunc f(r *sql.DB) {}", "uses sql.DB"},
		{"app/modules/x.go", "func f(d interface{ Begin() }) { d.Begin() }", "calls Begin"},
		{"app/modules/dbhandle.go", "import \"database/sql\"\nfunc f(d *sql.DB) { d.Begin() }", ""},
		{"app/modules/x.go", "import . \"database/sql\"", "imports database/sql as \".\""},
	}
	for _, tc := range cases {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, tc.file, "package x\n\n"+tc.src+"\n", parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		got := strings.Join(checkNoRawDatabase(fset, []*ast.File{f}), "\n")
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s in %s: problems %q, want %q", tc.src, tc.file, got, tc.want)
		}
	}
}

// outsideWaits are the calls a write transaction must not make (rule R4):
// they answer the request, send mail, reach the central, run an external
// program, hash a password or sleep. A call is recognized by the name it
// is made through; time.Sleep also by its package.
var outsideWaits = map[string]bool{
	"writeJSON": true, "writeError": true, "writeErr": true, "writeProblems": true,
	"internalError": true, "sendMail": true, "sendCodeMail": true, "Transport": true,
	"RunCommand": true, "HashPassword": true, "VerifyPassword": true,
}

// txFuncs is what checkWriteCallbacks knows about the functions of the
// packages it reads.
type txFuncs struct {
	fset *token.FileSet
	// decls maps a key (see calleeKeys) to the functions declared under it.
	decls map[string][]*ast.FuncDecl
	// declPkg is the package each declaration is in, and declImports the
	// import names of its file.
	declPkg     map[*ast.FuncDecl]string
	declImports map[*ast.FuncDecl]map[string]string
	// txNames are the names of the declared functions that take a
	// models.Writer: passing one of them to Write is a write transaction.
	txNames map[string]bool
}

// isWriterType reports whether e is the type models.Writer as the file
// with the import names imports names it.
func isWriterType(e ast.Expr, imports map[string]string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Writer" {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && imports[importModels] != "" && x.Name == imports[importModels]
}

// takesWriter reports whether the function type ft has a parameter of type
// models.Writer: its body runs inside a write transaction.
func takesWriter(ft *ast.FuncType, imports map[string]string) bool {
	if ft.Params == nil {
		return false
	}
	for _, p := range ft.Params.List {
		if isWriterType(p.Type, imports) {
			return true
		}
	}
	return false
}

// calleeKeys returns the keys under which the function that call calls is
// declared in the packages read: "pkg.Name" for a function of the package
// pkg, ".Name" for a method (its receiver type is not known without type
// checking, so every method of that name counts). A call of another
// package (models included) has no key.
func calleeKeys(call *ast.CallExpr, pkg string, imports map[string]string) []string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return []string{pkg + "." + fn.Name}
	case *ast.SelectorExpr:
		if x, ok := fn.X.(*ast.Ident); ok {
			for path, name := range imports {
				if name != x.Name {
					continue
				}
				switch path {
				case importModules:
					return []string{"modules." + fn.Sel.Name}
				case importWorkers:
					return []string{"workers." + fn.Sel.Name}
				}
				return nil
			}
		}
		return []string{"." + fn.Sel.Name}
	}
	return nil
}

// callName returns the name a call is made through (the identifier or the
// selected name), and whether it is time.Sleep.
func callName(call *ast.CallExpr, imports map[string]string) (string, bool) {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name, false
	case *ast.SelectorExpr:
		if x, ok := fn.X.(*ast.Ident); ok && imports["time"] != "" && x.Name == imports["time"] && fn.Sel.Name == "Sleep" {
			return "time.Sleep", true
		}
		return fn.Sel.Name, false
	}
	return "", false
}

// reachesHandleDirectly returns how call itself asks for a database, or "":
// a statement through Read(), a Write with a transaction function, or an
// open (OpenSQLite).
func (fs *txFuncs) reachesHandleDirectly(call *ast.CallExpr, imports map[string]string) string {
	name, _ := callName(call, imports)
	_, isSel := call.Fun.(*ast.SelectorExpr)
	switch {
	case name == "OpenSQLite":
		return "OpenSQLite"
	case isSel && name == "Read" && len(call.Args) == 0:
		return "Read()"
	case isSel && name == "Write" && len(call.Args) == 1:
		switch a := call.Args[0].(type) {
		case *ast.FuncLit:
			if takesWriter(a.Type, imports) {
				return "Write"
			}
		case *ast.Ident:
			if fs.txNames[a.Name] {
				return "Write"
			}
		case *ast.SelectorExpr:
			if fs.txNames[a.Sel.Name] {
				return "Write"
			}
		}
	}
	return ""
}

// waitsOutsideDirectly returns the outside operation call is, or "".
func waitsOutsideDirectly(call *ast.CallExpr, imports map[string]string) string {
	name, sleep := callName(call, imports)
	if sleep || outsideWaits[name] {
		return name
	}
	// A call through the field Transport (rt.Transport.Sync) reaches the
	// central.
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Transport" {
			return "Transport"
		}
	}
	return ""
}

// closure returns, for every key of fs.decls whose functions reach what
// direct finds (directly or through calls of other declared functions),
// the chain that shows how.
func (fs *txFuncs) closure(direct func(*ast.CallExpr, map[string]string) string) map[string]string {
	how := map[string]string{}
	for changed := true; changed; {
		changed = false
		for key, decls := range fs.decls {
			if _, done := how[key]; done {
				continue
			}
			for _, d := range decls {
				if d.Body == nil {
					continue
				}
				pkg, imports := fs.declPkg[d], fs.declImports[d]
				found := ""
				ast.Inspect(d.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok || found != "" {
						return found == ""
					}
					if s := direct(call, imports); s != "" {
						found = s
						return false
					}
					for _, k := range calleeKeys(call, pkg, imports) {
						if s, ok := how[k]; ok && k != key {
							found = strings.TrimPrefix(k, ".") + " -> " + s
							return false
						}
					}
					return true
				})
				if found != "" {
					how[key] = found
					changed = true
					break
				}
			}
		}
	}
	return how
}

// checkWriteCallbacks checks rules R3 and R4 on the files of the packages
// it is given: inside every function that runs in a write transaction (a
// function literal or declaration with a parameter of type models.Writer)
// no call asks for a database (this file's or another one's) and no call
// waits on the outside, directly or through the functions of these
// packages it calls. It returns the problems and the number of transaction
// functions it checked.
func checkWriteCallbacks(fset *token.FileSet, files []*ast.File) ([]string, int) {
	fs := &txFuncs{fset: fset, decls: map[string][]*ast.FuncDecl{}, declPkg: map[*ast.FuncDecl]string{},
		declImports: map[*ast.FuncDecl]map[string]string{}, txNames: map[string]bool{}}
	fileImports := map[*ast.File]map[string]string{}
	for _, f := range files {
		imports := importNames(f)
		fileImports[f] = imports
		pkg := f.Name.Name
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			key := pkg + "." + fd.Name.Name
			if fd.Recv != nil {
				key = "." + fd.Name.Name
			}
			fs.decls[key] = append(fs.decls[key], fd)
			fs.declPkg[fd] = pkg
			fs.declImports[fd] = imports
			if takesWriter(fd.Type, imports) {
				fs.txNames[fd.Name.Name] = true
			}
		}
	}
	reaches := fs.closure(fs.reachesHandleDirectly)
	waits := fs.closure(waitsOutsideDirectly)

	var problems []string
	checked := 0
	checkBody := func(body *ast.BlockStmt, pkg string, imports map[string]string) {
		checked++
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			pos := fset.Position(call.Pos())
			where := fmt.Sprintf("%s:%d", slashPath(fset, call.Pos()), pos.Line)
			if s := fs.reachesHandleDirectly(call, imports); s != "" {
				problems = append(problems, fmt.Sprintf("%s: inside a write transaction asks for a database (%s); use the transaction", where, s))
			}
			if s := waitsOutsideDirectly(call, imports); s != "" {
				problems = append(problems, fmt.Sprintf("%s: inside a write transaction waits on the outside (%s)", where, s))
			}
			for _, k := range calleeKeys(call, pkg, imports) {
				if s, ok := reaches[k]; ok {
					problems = append(problems, fmt.Sprintf("%s: inside a write transaction calls %s, which asks for a database (%s)", where, strings.TrimPrefix(k, "."), s))
				}
				if s, ok := waits[k]; ok {
					problems = append(problems, fmt.Sprintf("%s: inside a write transaction calls %s, which waits on the outside (%s)", where, strings.TrimPrefix(k, "."), s))
				}
			}
			return true
		})
	}
	for _, f := range files {
		imports, pkg := fileImports[f], f.Name.Name
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Body != nil && takesWriter(n.Type, imports) {
					checkBody(n.Body, pkg, imports)
					return false
				}
			case *ast.FuncLit:
				if takesWriter(n.Type, imports) {
					checkBody(n.Body, pkg, imports)
					return false
				}
			}
			return true
		})
	}
	sort.Strings(problems)
	return problems, checked
}

// TestWriteCallbacksUseOnlyTheirTransaction reads the sources of
// app/modules and app/workers and checks rules R3 and R4 of the handle for
// every path, also the ones no test runs: a function that runs inside a
// write transaction reaches the database only through its transaction (a
// second Write would wait for itself for ever; the guard turns that into a
// panic, but only on a path that runs) and does not wait on the outside
// while every other writer of the file waits for it.
func TestWriteCallbacksUseOnlyTheirTransaction(t *testing.T) {
	fset := token.NewFileSet()
	files := append(parseGoFiles(t, fset, "."), parseGoFiles(t, fset, "../workers")...)
	problems, checked := checkWriteCallbacks(fset, files)
	for _, p := range problems {
		t.Error(p)
	}
	// The count guards the test itself: if the transaction functions were
	// no longer found (a renamed type), it would pass in vain.
	if checked < 100 {
		t.Errorf("only %d functions that run in a write transaction were found", checked)
	}
}

// TestWriteCallbacksFindsNestedUse proves that the check above fails for
// each way a transaction could reach a database again or wait outside.
func TestWriteCallbacksFindsNestedUse(t *testing.T) {
	const head = "package workers\n\nimport (\n\t\"time\"\n\t\"mimamori/app/models\"\n\t\"mimamori/app/modules\"\n)\n\n" +
		"type core struct{ db *modules.DB }\n" +
		"func (c *core) load() string { v, _ := models.Get(c.db.Read()); return v }\n" +
		"func (c *core) save() error { return c.db.Write(func(tx models.Writer) error { return nil }) }\n" +
		"func (c *core) saveVia() error { return c.save() }\n" +
		"func (c *core) mail() { c.sendMail(nil) }\n" +
		"func store(tx models.Writer) error { return models.Put(tx) }\n"
	cases := []struct {
		name, src, want string
	}{
		{"only the transaction", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { return store(tx) }) }", ""},
		{"read after the transaction", "func (c *core) f() { _ = c.db.Write(store); c.load() }", ""},
		{"read side inside", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { models.Get(c.db.Read()); return nil }) }", "asks for a database (Read())"},
		{"write inside", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { return c.db.Write(store) }) }", "asks for a database (Write)"},
		{"open inside", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { _, err := modules.OpenSQLite(\"x\", \"y\"); return err }) }", "asks for a database (OpenSQLite)"},
		{"through a function", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { c.load(); return nil }) }", "calls load, which asks for a database (Read())"},
		{"through two functions", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { return c.saveVia() }) }", "calls saveVia, which asks for a database (save -> Write)"},
		{"in a declared transaction function", "func put(tx models.Writer, c *core) error { c.load(); return nil }", "calls load, which asks for a database"},
		{"in a nested literal", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { g := func() { c.load() }; g(); return nil }) }", "calls load"},
		{"a sleep", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { time.Sleep(1); return nil }) }", "waits on the outside (time.Sleep)"},
		{"an answer", "func (c *core) f(w any) error { return c.db.Write(func(tx models.Writer) error { writeError(w, 500, nil); return nil }) }", "waits on the outside (writeError)"},
		{"a mail through a function", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { c.mail(); return nil }) }", "calls mail, which waits on the outside (sendMail)"},
		{"the central", "func (c *core) f(rt *modules.AgentRuntime) error { return c.db.Write(func(tx models.Writer) error { rt.Transport.Sync(nil); return nil }) }", "waits on the outside (Transport)"},
		{"a password hash", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { modules.HashPassword(\"\"); return nil }) }", "waits on the outside (HashPassword)"},
	}
	for _, tc := range cases {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "case.go", head+tc.src+"\n", parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		problems, _ := checkWriteCallbacks(fset, []*ast.File{f})
		got := strings.Join(problems, "\n")
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: problems %q, want %q", tc.name, got, tc.want)
		}
	}
}
