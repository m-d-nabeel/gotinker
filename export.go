package main

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
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
// redeclares a variable (which the playground allows but Go does not), and
// `_ = x` is kept only for variables nothing else uses.
func (s *Session) exportSource() string {
	var body strings.Builder
	defined := map[string]bool{}
	depth := 0
	for i, c := range s.stmts {
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
		later := laterSource(s.stmts[i+1:])
		for _, n := range names {
			defined[n] = true
			if !regexp.MustCompile(`\b`+regexp.QuoteMeta(n)+`\b`).MatchString(later) && !usedAfterDef(src, n) {
				body.WriteString("_ = " + n + "\n")
			}
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
	return formatSource(b.String())
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

func laterSource(cells []*cell) string {
	var b strings.Builder
	for _, c := range cells {
		b.WriteString(c.out + "\n")
	}
	return b.String()
}

// usedAfterDef reports whether name appears in src after its first
// definition (a rough check that keeps exported code free of `_ = x` noise).
func usedAfterDef(src, name string) bool {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
	locs := re.FindAllStringIndex(src, -1)
	return len(locs) > 1
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
