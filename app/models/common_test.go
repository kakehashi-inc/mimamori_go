package models_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestReadFunctionsOnlySelect reads the sources of this package and checks
// what the type Reader promises: a function that takes a Reader sends only
// SELECT statements through it and hands it only to functions that take a
// Reader too. A function declared as reading that writes would compile (an
// UPDATE ... RETURNING goes through QueryRow) and fail only when it runs on
// a read-only connection; this test fails before that.
func TestReadFunctionsOnlySelect(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	readers, problems := checkReadFunctions(fset, files)
	for _, p := range problems {
		t.Error(p)
	}
	// The count guards the test itself: if the functions were no longer
	// found (a renamed type, a moved directory), it would pass in vain.
	if readers < 50 {
		t.Errorf("only %d functions with a Reader were found", readers)
	}
}

// TestReadFunctionsOnlySelectFindsWrites proves that the check above fails
// for each way a function declared as reading could write.
func TestReadFunctionsOnlySelectFindsWrites(t *testing.T) {
	const head = "package models\n\n" +
		"func get(db Reader, id int64) error { return db.QueryRow(`SELECT 1 WHERE ? > 0`, id).Scan(new(int)) }\n" +
		"func put(db Writer, id int64) error { _, err := db.Exec(`DELETE FROM t WHERE id = ?`, id); return err }\n" +
		"const stored = `SELECT v FROM t`\n" +
		"const storedWrite = `DELETE FROM t RETURNING v`\n"
	cases := []struct {
		name, src, want string
	}{
		{"select", "func f(db Reader) error { return get(db, 1) }", ""},
		{"select in a variable", "func f(db Reader) { q := `SELECT v FROM t`; q += ` WHERE v > 0`; db.Query(q + ` LIMIT 1`) }", ""},
		{"select in a constant", "func f(db Reader) { db.Query(stored) }", ""},
		{"select after white space", "func f(db Reader) { db.Query(\"\\n\\t select v FROM t\") }", ""},
		{"update returning", "func f(db Reader) { db.QueryRow(`UPDATE t SET v = v + 1 RETURNING v`) }", "sends a statement that begins with UPDATE"},
		{"delete returning", "func f(db Reader) { db.Query(`DELETE FROM t RETURNING v`) }", "sends a statement that begins with DELETE"},
		{"write in a variable", "func f(db Reader) { q := `SELECT 1`; q = `INSERT INTO t VALUES (1) RETURNING v`; db.Query(q) }", "sends a statement that begins with INSERT"},
		{"write in a constant", "func f(db Reader) { db.Query(storedWrite) }", "sends a statement that begins with DELETE"},
		{"text made elsewhere", "func f(db Reader, q string) { db.Query(q) }", "cannot be followed to its text"},
		{"text from a call", "func f(db Reader) { db.Query(text()) }\nfunc text() string { return `` }", "cannot be followed to its text"},
		{"passed to a writing function", "func f(db Reader) error { return put(db, 1) }", "passes its Reader to put"},
		{"passed to an unknown function", "func f(db Reader) { fmt.Println(db) }", "uses its Reader db in a way"},
		{"asserted to a Writer", "func f(db Reader) { db.(Writer).Exec(`DELETE FROM t`) }", "uses its Reader db in a way"},
		{"kept in a variable", "func f(db Reader) { x := db; _ = x }", "uses its Reader db in a way"},
		{"inside a function literal", "func f(db Reader) { func() { db.Query(`DROP TABLE t`) }() }", "sends a statement that begins with DROP"},
	}
	for _, tc := range cases {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "case.go", head+tc.src+"\n", parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		_, problems := checkReadFunctions(fset, []*ast.File{f})
		got := strings.Join(problems, "; ")
		if tc.want == "" && got != "" {
			t.Errorf("%s: reported %q, want nothing", tc.name, got)
		}
		if tc.want != "" && (len(problems) != 1 || !strings.Contains(got, tc.want)) {
			t.Errorf("%s: reported %q, want one problem containing %q", tc.name, got, tc.want)
		}
	}
}

// checkReadFunctions returns the number of functions in files that have a
// parameter of type Reader and what is wrong with them. The check works on
// the syntax tree alone: an identifier with the name of the Reader parameter
// may stand only where a statement is sent (db.Query, db.QueryRow) or as the
// argument of a function of the package whose parameter there is a Reader;
// every other use (kept in a variable, asserted to another type, given to a
// function of another package) is reported, because it could lead to a write
// this test does not see.
func checkReadFunctions(fset *token.FileSet, files []*ast.File) (readers int, problems []string) {
	// The parameter types of every function of the package, by name, and
	// the package's string constants and variables.
	params := map[string][]string{}
	globals := map[string]ast.Expr{}
	for _, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					_, types := flatParams(d.Type)
					params[d.Name.Name] = types
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					vs, ok := s.(*ast.ValueSpec)
					if !ok || len(vs.Names) != len(vs.Values) {
						continue
					}
					for i, n := range vs.Names {
						globals[n.Name] = vs.Values[i]
					}
				}
			}
		}
	}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			names, types := flatParams(fd.Type)
			counted := false
			for i, ty := range types {
				if ty != "Reader" || names[i] == "" || names[i] == "_" {
					continue
				}
				if !counted {
					readers++
					counted = true
				}
				for _, p := range checkReaderUse(fd, names[i], params, globals) {
					problems = append(problems, fmt.Sprintf("%s: %s %s", fset.Position(fd.Pos()), fd.Name.Name, p))
				}
			}
		}
	}
	sort.Strings(problems)
	return readers, problems
}

// flatParams returns the names and the type names of the parameters of a
// function, one entry per parameter ("" for a type that is not a plain
// identifier or for a parameter without a name).
func flatParams(ft *ast.FuncType) (names, types []string) {
	if ft.Params == nil {
		return nil, nil
	}
	for _, field := range ft.Params.List {
		ty := ""
		if id, ok := field.Type.(*ast.Ident); ok {
			ty = id.Name
		}
		if len(field.Names) == 0 {
			names, types = append(names, ""), append(types, ty)
			continue
		}
		for _, n := range field.Names {
			names, types = append(names, n.Name), append(types, ty)
		}
	}
	return names, types
}

// checkReaderUse checks every place in the body of fd where the Reader
// parameter named db stands.
func checkReaderUse(fd *ast.FuncDecl, db string, params map[string][]string, globals map[string]ast.Expr) []string {
	var problems []string
	// What is assigned to each local variable (a statement text is followed
	// through them). "+=" only appends, so it does not change how a text
	// begins.
	locals := map[string][]ast.Expr{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if (n.Tok == token.DEFINE || n.Tok == token.ASSIGN) && len(n.Lhs) == len(n.Rhs) {
				for i, l := range n.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						locals[id.Name] = append(locals[id.Name], n.Rhs[i])
					}
				}
			}
		case *ast.ValueSpec:
			if len(n.Names) == len(n.Values) {
				for i, id := range n.Names {
					locals[id.Name] = append(locals[id.Name], n.Values[i])
				}
			}
		}
		return true
	})
	// The identifiers that stand in an allowed place, and the ones that only
	// share the name (the field of a selector, the key of a literal).
	allowed := map[*ast.Ident]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			allowed[n.Sel] = true
		case *ast.KeyValueExpr:
			if id, ok := n.Key.(*ast.Ident); ok {
				allowed[id] = true
			}
		case *ast.CallExpr:
			switch fun := n.Fun.(type) {
			case *ast.SelectorExpr:
				x, ok := fun.X.(*ast.Ident)
				if !ok || x.Name != db || (fun.Sel.Name != "Query" && fun.Sel.Name != "QueryRow") || len(n.Args) == 0 {
					break
				}
				allowed[x] = true
				verbs, ok := statementVerbs(n.Args[0], locals, globals, map[string]bool{})
				if !ok {
					problems = append(problems, fmt.Sprintf("sends a statement through %s that cannot be followed to its text", fun.Sel.Name))
				}
				for _, v := range verbs {
					if v != "SELECT" {
						problems = append(problems, fmt.Sprintf("sends a statement that begins with %s through %s", v, fun.Sel.Name))
					}
				}
			case *ast.Ident:
				for i, a := range n.Args {
					id, ok := a.(*ast.Ident)
					if !ok || id.Name != db {
						continue
					}
					types, known := params[fun.Name]
					if !known {
						break // reported below as a use that cannot be followed
					}
					allowed[id] = true
					if i >= len(types) || types[i] != "Reader" {
						problems = append(problems, fmt.Sprintf("passes its Reader to %s, which does not take a Reader there", fun.Name))
					}
				}
			}
		}
		return true
	})
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == db && !allowed[id] {
			problems = append(problems, fmt.Sprintf("uses its Reader %s in a way this test cannot follow (only %s.Query, %s.QueryRow and passing it to a function of the package are allowed)", db, db, db))
		}
		return true
	})
	return problems
}

// statementVerbs returns the first word (in upper case) of every text the
// statement expression e can begin with; ok is false when a text cannot be
// found. A variable is followed to everything that is assigned to it, a
// constant of the package to its value.
func statementVerbs(e ast.Expr, locals map[string][]ast.Expr, globals map[string]ast.Expr, seen map[string]bool) (verbs []string, ok bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return nil, false
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			return nil, false
		}
		fields := strings.Fields(s)
		if len(fields) == 0 {
			return nil, false
		}
		return []string{strings.ToUpper(fields[0])}, true
	case *ast.ParenExpr:
		return statementVerbs(v.X, locals, globals, seen)
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return nil, false
		}
		return statementVerbs(v.X, locals, globals, seen)
	case *ast.Ident:
		if seen[v.Name] {
			// q = q + ...: begins as q began.
			return nil, true
		}
		seen[v.Name] = true
		values := locals[v.Name]
		if len(values) == 0 {
			if g, isGlobal := globals[v.Name]; isGlobal {
				values = []ast.Expr{g}
			}
		}
		if len(values) == 0 {
			return nil, false
		}
		for _, x := range values {
			more, ok := statementVerbs(x, locals, globals, seen)
			if !ok {
				return nil, false
			}
			verbs = append(verbs, more...)
		}
		return verbs, true
	}
	return nil, false
}
