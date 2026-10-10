package main

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"regexp"
	"strings"
)

func formatSource(src string) string {
	out, err := format.Source([]byte(src))
	if err != nil {
		return src
	}
	return string(out)
}

// exportSource renders the session as a standalone program: statement cells
// are flattened into main, opening a nested block only where a cell
// redeclares a variable (which the playground allows but Go does not).
// Variables that end up unused get a `_ = x`, as the type checker reports
// them, so the result compiles.
func (s *Session) exportSource() string {
	var body strings.Builder
	defined := map[string]bool{}
	depth := 0
	for _, c := range s.stmts {
		src := c.out
		if strings.TrimSpace(src) == "" {
			continue
		}
		stmts, err := parseStmts(src)
		if err != nil {
			body.WriteString(src + "\n")
			continue
		}
		names := topLevelDefs(stmts)
		redeclared := false
		for _, n := range names {
			if defined[n] {
				redeclared = true
			}
		}
		if redeclared {
			body.WriteString("{\n")
			depth++
			defined = map[string]bool{}
		}
		body.WriteString(strings.TrimRight(src, "\n") + "\n")
		for _, n := range names {
			defined[n] = true
		}
	}
	body.WriteString(strings.Repeat("}\n", depth))

	var code strings.Builder
	for _, d := range s.decls {
		code.WriteString(strings.TrimRight(d.src, "\n") + "\n\n")
	}
	code.WriteString("func main() {\n" + body.String() + "}\n")

	var b strings.Builder
	b.WriteString("package main\n\n")
	used := referencedImports(code.String(), s.usedImports())
	if len(used) > 0 {
		b.WriteString("import (\n")
		for _, imp := range used {
			if imp.name != "" {
				fmt.Fprintf(&b, "\t%s %q\n", imp.name, imp.path)
			} else {
				fmt.Fprintf(&b, "\t%q\n", imp.path)
			}
		}
		b.WriteString(")\n\n")
	}
	b.WriteString(code.String())
	return formatSource(s.useUnused(b.String()))
}

var reUnusedVar = regexp.MustCompile(`^declared and not used: (\w+)$`)

// useUnused adds `_ = x` after the statement declaring each variable the
// type checker reports as unused.
func (s *Session) useUnused(src string) string {
	for range 5 {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "main.go", src, 0)
		if err != nil {
			return src
		}
		var unused []types.Error
		conf := types.Config{Importer: s.imp, GoVersion: "go" + s.goVer, Error: func(err error) {
			if te, ok := err.(types.Error); ok && reUnusedVar.MatchString(te.Msg) {
				unused = append(unused, te)
			}
		}}
		_, _ = conf.Check("main", fset, []*ast.File{f}, nil)
		if len(unused) == 0 {
			return src
		}
		var edits []edit
		for _, e := range unused {
			name := reUnusedVar.FindStringSubmatch(e.Msg)[1]
			// The innermost statement that declares the variable.
			var decl ast.Stmt
			ast.Inspect(f, func(n ast.Node) bool {
				switch st := n.(type) {
				case *ast.AssignStmt, *ast.DeclStmt, *ast.RangeStmt:
					if st.Pos() <= e.Pos && e.Pos < st.End() {
						decl = st.(ast.Stmt)
					}
				}
				return true
			})
			if decl == nil {
				continue
			}
			if _, isRange := decl.(*ast.RangeStmt); isRange {
				continue // a loop variable: leave it to the user
			}
			edits = append(edits, edit{fset.Position(decl.End()).Offset, "\n_ = " + name})
		}
		if len(edits) == 0 {
			return src
		}
		src = applyEdits(src, edits)
	}
	return src
}

// referencedImports keeps the imports whose name is used as a selector
// qualifier (pkg.Name) somewhere in the declarations and body of code.
func referencedImports(code string, imports []importSpec) []importSpec {
	f, err := parser.ParseFile(token.NewFileSet(), "", "package main\n"+code, 0)
	if err != nil {
		return imports
	}
	names := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if se, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := se.X.(*ast.Ident); ok {
				names[id.Name] = true
			}
		}
		return true
	})
	var out []importSpec
	for _, imp := range imports {
		if names[imp.binding()] {
			out = append(out, imp)
		}
	}
	return out
}

func topLevelDefs(stmts []ast.Stmt) []string {
	var names []string
	for _, st := range stmts {
		switch st := st.(type) {
		case *ast.AssignStmt:
			if st.Tok == token.DEFINE {
				for _, l := range st.Lhs {
					if id, ok := l.(*ast.Ident); ok && id.Name != "_" {
						names = append(names, id.Name)
					}
				}
			}
		case *ast.DeclStmt:
			if gd, ok := st.Decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
				for _, spec := range gd.Specs {
					for _, n := range spec.(*ast.ValueSpec).Names {
						if n.Name != "_" {
							names = append(names, n.Name)
						}
					}
				}
			}
		}
	}
	return names
}
