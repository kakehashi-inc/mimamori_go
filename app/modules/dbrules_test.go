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
// program, walk the file system, hash a password or sleep. A call of the
// program's own functions is recognized by the name it is made through.
var outsideWaits = map[string]bool{
	"writeJSON": true, "writeError": true, "writeErr": true, "writeProblems": true,
	"internalError": true, "sendMail": true, "sendCodeMail": true, "SendMailTo": true,
	"Transport": true, "RunCommand": true, "HashPassword": true, "VerifyPassword": true,
	"ScanWritable": true, "ScanWritableFrom": true, "ScanExecFiles": true, "ScanExecFilesFrom": true,
}

// packageWaits are the functions of the standard library that wait on the
// outside, by import path; they are recognized by their package, so that a
// method of the same name elsewhere does not count.
var packageWaits = map[string]map[string]bool{
	"time":          {"Sleep": true},
	"path/filepath": {"Walk": true, "WalkDir": true},
	"io/fs":         {"WalkDir": true},
	"os/exec":       {"Command": true, "CommandContext": true},
	"net/http":      {"Get": true, "Head": true, "Post": true, "PostForm": true},
	"net":           {"Dial": true, "DialTimeout": true},
	"crypto/tls":    {"Dial": true, "DialWithDialer": true},
	"net/smtp":      {"SendMail": true},
}

// methodWaits are the methods of the standard library that wait on the
// outside, with the number of arguments they take: without type checking
// the receiver is not known, and the count keeps apart a method of the same
// name with another signature (http.Client.Do takes one argument; the
// program's HTTPSClient.Do takes three and is found through its body). The
// methods Get and Head of http.Client are not listed: they cannot be told
// from url.Values.Get and http.Header.Get by the count; the program's
// clients go through Do or Dial.
var methodWaits = map[string]int{
	"Do": 1, "Post": 3, "PostForm": 2, "Dial": 2, "DialContext": 3, "Handshake": 0,
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
	// methodKeys maps a method name to the keys it is declared under (one
	// per number of parameters).
	methodKeys map[string][]string
	// dbNames are the names declared with the type *modules.DB (fields,
	// parameters, variables, and the results of OpenSQLite): Close through
	// one of them closes a handle.
	dbNames map[string]bool
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

// paramCount returns the number of parameters of ft and whether the last
// one is variadic.
func paramCount(ft *ast.FuncType) (int, bool) {
	if ft.Params == nil {
		return 0, false
	}
	n, variadic := 0, false
	for _, p := range ft.Params.List {
		if len(p.Names) == 0 {
			n++
		} else {
			n += len(p.Names)
		}
		_, variadic = p.Type.(*ast.Ellipsis)
	}
	return n, variadic
}

// methodKey returns the key a method declaration is kept under: ".Name/N"
// with N its number of parameters, ".Name/vN" when the last one is
// variadic.
func methodKey(fd *ast.FuncDecl) string {
	n, variadic := paramCount(fd.Type)
	if variadic {
		return fmt.Sprintf(".%s/v%d", fd.Name.Name, n)
	}
	return fmt.Sprintf(".%s/%d", fd.Name.Name, n)
}

// shownKey returns a key as the problems name it: the function or method
// name, without the leading dot of a method and its parameter count.
func shownKey(k string) string {
	k = strings.TrimPrefix(k, ".")
	if i := strings.Index(k, "/"); i >= 0 {
		k = k[:i]
	}
	return k
}

// calleeKeys returns the keys under which the function that call calls is
// declared in the packages read: "pkg.Name" for a function of the package
// pkg; for a method (its receiver type is not known without type
// checking) every method of that name whose parameters the arguments can
// fill. A call of another package (models included) has no key.
func (fs *txFuncs) calleeKeys(call *ast.CallExpr, pkg string, imports map[string]string) []string {
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
		args := len(call.Args)
		// f(g()) passes all results of g: the count of the call says
		// nothing then.
		spread := false
		if args == 1 {
			_, spread = call.Args[0].(*ast.CallExpr)
		}
		var keys []string
		for _, k := range fs.methodKeys[fn.Sel.Name] {
			n, variadic := paramCount(fs.decls[k][0].Type)
			if spread || n == args || variadic && args >= n-1 {
				keys = append(keys, k)
			}
		}
		return keys
	}
	return nil
}

// callName returns the name a call is made through: the identifier or the
// selected name.
func callName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// lastName returns the last name of the expression x (v of v, f of a.b.f),
// or "".
func lastName(x ast.Expr) string {
	switch x := x.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

// isDBType reports whether e is *DB in the package modules, or *X.DB with X
// the file's name for it.
func isDBType(e ast.Expr, pkg string, imports map[string]string) bool {
	star, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch t := star.X.(type) {
	case *ast.Ident:
		return pkg == "modules" && t.Name == "DB"
	case *ast.SelectorExpr:
		x, ok := t.X.(*ast.Ident)
		return ok && t.Sel.Name == "DB" && imports[importModules] != "" && x.Name == imports[importModules]
	}
	return false
}

// collectDBNames adds to fs.dbNames the names f declares with the type
// *modules.DB, and the names the result of OpenSQLite is assigned to.
func (fs *txFuncs) collectDBNames(f *ast.File, imports map[string]string) {
	pkg := f.Name.Name
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Field:
			if isDBType(n.Type, pkg, imports) {
				for _, name := range n.Names {
					fs.dbNames[name.Name] = true
				}
			}
		case *ast.ValueSpec:
			if n.Type != nil && isDBType(n.Type, pkg, imports) {
				for _, name := range n.Names {
					fs.dbNames[name.Name] = true
				}
			}
		case *ast.AssignStmt:
			if len(n.Rhs) == 1 && len(n.Lhs) > 0 {
				if call, ok := n.Rhs[0].(*ast.CallExpr); ok && callName(call) == "OpenSQLite" {
					if name := lastName(n.Lhs[0]); name != "" && name != "_" {
						fs.dbNames[name] = true
					}
				}
			}
		}
		return true
	})
}

// reachesHandleDirectly returns how call itself asks for a database, or "":
// a statement through Read(), a Write with a transaction function, an open
// (OpenSQLite) or a Close of a handle.
func (fs *txFuncs) reachesHandleDirectly(call *ast.CallExpr, imports map[string]string) string {
	name := callName(call)
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	switch {
	case name == "OpenSQLite":
		return "OpenSQLite"
	case isSel && name == "Close" && len(call.Args) == 0 && fs.dbNames[lastName(sel.X)]:
		return "Close"
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
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		if outsideWaits[fn.Name] {
			return fn.Name
		}
	case *ast.SelectorExpr:
		if x, ok := fn.X.(*ast.Ident); ok {
			for path, names := range packageWaits {
				if imports[path] == "" || x.Name != imports[path] {
					continue
				}
				if names[fn.Sel.Name] {
					return path[strings.LastIndex(path, "/")+1:] + "." + fn.Sel.Name
				}
				return ""
			}
		}
		if outsideWaits[fn.Sel.Name] {
			return fn.Sel.Name
		}
		if n, ok := methodWaits[fn.Sel.Name]; ok && n == len(call.Args) {
			return fn.Sel.Name
		}
		// A call through the field Transport (rt.Transport.Sync) reaches
		// the central.
		if inner, ok := fn.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Transport" {
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
					for _, k := range fs.calleeKeys(call, pkg, imports) {
						if s, ok := how[k]; ok && k != key {
							found = shownKey(k) + " -> " + s
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
		declImports: map[*ast.FuncDecl]map[string]string{}, txNames: map[string]bool{},
		methodKeys: map[string][]string{}, dbNames: map[string]bool{}}
	fileImports := map[*ast.File]map[string]string{}
	for _, f := range files {
		imports := importNames(f)
		fileImports[f] = imports
		fs.collectDBNames(f, imports)
		pkg := f.Name.Name
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			key := pkg + "." + fd.Name.Name
			if fd.Recv != nil {
				key = methodKey(fd)
				if len(fs.decls[key]) == 0 {
					fs.methodKeys[fd.Name.Name] = append(fs.methodKeys[fd.Name.Name], key)
				}
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
			for _, k := range fs.calleeKeys(call, pkg, imports) {
				if s, ok := reaches[k]; ok {
					problems = append(problems, fmt.Sprintf("%s: inside a write transaction calls %s, which asks for a database (%s)", where, shownKey(k), s))
				}
				if s, ok := waits[k]; ok {
					problems = append(problems, fmt.Sprintf("%s: inside a write transaction calls %s, which waits on the outside (%s)", where, shownKey(k), s))
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
	const head = "package workers\n\nimport (\n\t\"net\"\n\t\"net/http\"\n\t\"os\"\n\t\"os/exec\"\n\t\"path/filepath\"\n\t\"time\"\n" +
		"\t\"mimamori/app/models\"\n\t\"mimamori/app/modules\"\n)\n\n" +
		"type core struct{ db *modules.DB; web *http.Client; cl *client }\n" +
		"type client struct{ addr string }\n" +
		"func (c *client) Do(method, path string, body []byte) error { var d net.Dialer; _, err := d.Dial(\"tcp\", c.addr); return err }\n" +
		"func (c *client) JSON(method, path string, in, out any) error { return c.Do(method, path, nil) }\n" +
		"type content struct{}\n" +
		"func (content) JSON() string { return \"\" }\n" +
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
		{"a file walk", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { return filepath.WalkDir(\".\", nil) }) }", "waits on the outside (filepath.WalkDir)"},
		{"a scan", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { _, err := modules.ScanWritable(\"/\", nil, nil, \"\"); return err }) }", "waits on the outside (ScanWritable)"},
		{"a mail sent directly", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { return modules.SendMailTo(modules.SMTPConfig{}, nil, \"\", \"\") }) }", "waits on the outside (SendMailTo)"},
		{"an external program", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { return exec.Command(\"true\").Run() }) }", "waits on the outside (exec.Command)"},
		{"an HTTP request", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { _, err := http.Get(\"x\"); return err }) }", "waits on the outside (http.Get)"},
		{"an HTTP client request", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { _, err := c.web.Do(nil); return err }) }", "waits on the outside (Do)"},
		{"the agent's client", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { return c.cl.JSON(\"POST\", \"/x\", nil, nil) }) }", "calls JSON, which waits on the outside (Do -> Dial)"},
		{"a method of the same name", "func (c *core) f(r content) error { return c.db.Write(func(tx models.Writer) error { _ = r.JSON(); return nil }) }", ""},
		{"close of the handle", "func (c *core) f() error { return c.db.Write(func(tx models.Writer) error { return c.db.Close() }) }", "asks for a database (Close)"},
		{"close of another handle", "func (c *core) f(e *modules.DB) error { return c.db.Write(func(tx models.Writer) error { return e.Close() }) }", "asks for a database (Close)"},
		{"close of an opened handle", "func (c *core) f() error { h, _ := modules.OpenSQLite(\"x\", \"y\"); return c.db.Write(func(tx models.Writer) error { return h.Close() }) }", "asks for a database (Close)"},
		{"close of a file", "func (c *core) f() error { fh, _ := os.Open(\"x\"); return c.db.Write(func(tx models.Writer) error { return fh.Close() }) }", ""},
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
