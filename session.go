package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const runtimePath = "gotinker.session/goprt"

type cellKind int

const (
	declCell cellKind = iota
	stmtCell
)

// cell is one committed piece of the session program. Declarations live at
// package level; statement cells are replayed in order inside main, each
// opening a nested block so a later `x := ...` can shadow an earlier x.
type cell struct {
	id    int
	kind  cellKind
	src   string   // code as placed in the program (statement cells include `_ = x` guards)
	input string   // what the user typed, for :cells and error context
	raw   string   // code before gotinker's edits; error positions refer to its lines
	out   string   // code for :show/:save (raw minus a trailing display-only expression)
	names []string // declared names; a new declaration replaces cells sharing a name

	committed bool // part of the session (errors in it are "earlier cell" errors)
}

type importSpec struct {
	name string // explicit alias, "" for the default name
	path string
	auto bool // added by gotinker, so it may be swapped for a same-named package
}

func (i importSpec) binding() string {
	if i.name != "" {
		return i.name
	}
	return pkgName(i.path)
}

// Session holds the cells of the program being built up interactively.
type Session struct {
	dir     string // module directory holding main.go and the goprt runtime
	cwd     string // where the user started gotinker; programs run here
	modTmpl string // shared go.mod/go.sum template dir, so `:get` persists
	goVer   string // "1.26"

	fset *token.FileSet
	imp  *exportImporter
	std  *stdIndex

	decls   []*cell
	stmts   []*cell
	imports []importSpec
	nextID  int

	ui *ui
}

func newSession(u *ui) (*Session, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	base = filepath.Join(base, "gotinker")
	sessions := filepath.Join(base, "sessions")
	modTmpl := filepath.Join(base, "mod")
	for _, d := range []string{sessions, modTmpl} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	cleanStaleSessions(sessions)

	goVer, err := goVersion()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(sessions, "s-")
	if err != nil {
		return nil, err
	}
	cwd, _ := os.Getwd()
	s := &Session{dir: dir, cwd: cwd, modTmpl: modTmpl, goVer: goVer, fset: token.NewFileSet(), ui: u, nextID: 1}
	if err := s.writeModule(); err != nil {
		return nil, err
	}
	s.std, err = loadStdIndex(base, goVer)
	if err != nil {
		return nil, fmt.Errorf("listing standard library: %w", err)
	}
	s.imp = newExportImporter(dir, s.fset)
	return s, nil
}

// warm compiles the runtime and common packages into the build cache and
// export-data index, so the first input is fast.
func (s *Session) warm() {
	_ = s.imp.load(runtimePath, "fmt", "strings", "strconv", "os", "time", "math", "sort", "slices", "maps", "errors")
	cmd := exec.Command("go", "build", "-o", os.DevNull, "./goprt")
	cmd.Dir = s.dir
	_ = cmd.Run()
}

func (s *Session) Close() { os.RemoveAll(s.dir) }

func cleanStaleSessions(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

func goVersion() (string, error) {
	out, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		return "", fmt.Errorf("go toolchain not found: %w", err)
	}
	m := regexp.MustCompile(`go(\d+)\.(\d+)`).FindStringSubmatch(string(out))
	if m == nil {
		return "", fmt.Errorf("unrecognised go version %q", out)
	}
	return m[1] + "." + m[2], nil
}

func (s *Session) writeModule() error {
	gomod := filepath.Join(s.modTmpl, "go.mod")
	if _, err := os.Stat(gomod); err != nil {
		content := "module gotinker.session\n\ngo " + s.goVer + "\n"
		if err := os.WriteFile(gomod, []byte(content), 0o644); err != nil {
			return err
		}
	}
	for _, f := range []string{"go.mod", "go.sum"} {
		if data, err := os.ReadFile(filepath.Join(s.modTmpl, f)); err == nil {
			if err := os.WriteFile(filepath.Join(s.dir, f), data, 0o644); err != nil {
				return err
			}
		}
	}
	rt := filepath.Join(s.dir, "goprt")
	if err := os.MkdirAll(rt, 0o755); err != nil {
		return err
	}
	entries, err := goprtFS.ReadDir("goprt")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		data, _ := goprtFS.ReadFile("goprt/" + e.Name())
		if err := os.WriteFile(filepath.Join(rt, e.Name()), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ------------------------------------------------------------------ parsing

// parsed is a classified input: declarations, imports and statements.
type parsed struct {
	decls   []*cell
	imports []importSpec
	stmts   string // statement source to run, "" if none
}

var declKeyword = regexp.MustCompile(`^\s*(package|import|func|type|const|var)\b`)

// parseInput decides whether input is package-level declarations, statements,
// or a whole file (package clause, declarations and a main function).
func (s *Session) parseInput(src string) (*parsed, error) {
	trimmed := strings.TrimSpace(src)
	if strings.HasPrefix(trimmed, "package ") {
		return s.parseDecls(src, true)
	}
	p, declErr := s.parseDecls(src, false)
	if declErr == nil {
		return p, nil
	}
	_, stmtErr := parseStmts(src)
	if stmtErr == nil {
		// A type pasted along with statements also parses as a local
		// type; put it at package level, as when typed on its own, so
		// methods can be declared on it.
		if hasTypeUnit(src) {
			if mixed, ok := s.splitMixed(src); ok {
				return mixed, nil
			}
		}
		return &parsed{stmts: src}, nil
	}
	if mixed, ok := s.splitMixed(src); ok {
		return mixed, nil
	}
	if !declKeyword.MatchString(src) || strings.HasPrefix(trimmed, "var") || strings.HasPrefix(trimmed, "const") {
		return nil, stmtErr
	}
	return nil, declErr
}

// splitMixed handles input that mixes package-level declarations with
// statements, such as a pasted type, its methods and code that uses them.
// Each top-level unit is classified on its own: declarations go to package
// level (so methods can be declared), statements run in order.
func (s *Session) splitMixed(src string) (*parsed, bool) {
	units := topLevelUnits(src)
	if len(units) < 2 {
		return nil, false
	}
	p := &parsed{}
	var stmts []string
	for _, u := range units {
		if d, err := s.parseDecls(u, false); err == nil {
			p.decls = append(p.decls, d.decls...)
			p.imports = append(p.imports, d.imports...)
			if d.stmts != "" {
				stmts = append(stmts, d.stmts)
			}
			continue
		}
		if _, err := parseStmts(u); err != nil {
			return nil, false
		}
		stmts = append(stmts, u)
	}
	if len(p.decls)+len(p.imports) == 0 {
		return nil, false
	}
	p.stmts = strings.Join(stmts, "\n")
	return p, true
}

func hasTypeUnit(src string) bool {
	units := topLevelUnits(src)
	if len(units) < 2 {
		return false
	}
	for _, u := range units {
		if strings.HasPrefix(u, "type ") || strings.HasPrefix(u, "type(") {
			return true
		}
	}
	return false
}

// topLevelUnits splits src at statement boundaries (newlines that end a
// statement, and semicolons) outside any brackets.
func topLevelUnits(src string) []string {
	var sc scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	sc.Init(file, []byte(src), func(token.Position, string) {}, 0)
	var units []string
	depth, start := 0, 0
	add := func(end int) {
		if u := strings.TrimSpace(src[start:end]); u != "" {
			units = append(units, u)
		}
	}
	for {
		pos, tok, _ := sc.Scan()
		if tok == token.EOF {
			break
		}
		switch tok {
		case token.LPAREN, token.LBRACE, token.LBRACK:
			depth++
		case token.RPAREN, token.RBRACE, token.RBRACK:
			depth--
		case token.SEMICOLON:
			if depth == 0 {
				off := file.Offset(pos)
				add(off)
				start = off + 1
			}
		}
	}
	if start < len(src) {
		add(len(src))
	}
	return units
}

func (s *Session) parseDecls(src string, file bool) (*parsed, error) {
	full := src
	if !file {
		full = "package main\n" + src
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "input", full, parser.ParseComments)
	if err != nil {
		return nil, shiftParseError(err, -1*boolInt(!file))
	}
	if len(f.Decls) == 0 {
		return nil, errors.New("no declarations")
	}
	text := func(from, to token.Pos) string {
		return full[fset.Position(from).Offset:fset.Position(to).Offset]
	}
	p := &parsed{}
	for _, d := range f.Decls {
		start := d.Pos()
		switch d := d.(type) {
		case *ast.BadDecl:
			return nil, errors.New("bad declaration")
		case *ast.GenDecl:
			if d.Doc != nil {
				start = d.Doc.Pos()
			}
			if d.Tok == token.IMPORT {
				for _, spec := range d.Specs {
					is := spec.(*ast.ImportSpec)
					path, _ := strconv.Unquote(is.Path.Value)
					name := ""
					if is.Name != nil {
						name = is.Name.Name
					}
					p.imports = append(p.imports, importSpec{name: name, path: path})
				}
				continue
			}
			c := &cell{kind: declCell, src: text(start, d.End())}
			for _, spec := range d.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					c.names = append(c.names, spec.Name.Name)
				case *ast.ValueSpec:
					for _, n := range spec.Names {
						if n.Name != "_" {
							c.names = append(c.names, n.Name)
						}
					}
				}
			}
			p.decls = append(p.decls, c)
		case *ast.FuncDecl:
			if d.Doc != nil {
				start = d.Doc.Pos()
			}
			if d.Recv == nil && d.Name.Name == "main" && d.Body != nil {
				// A pasted program: its main body becomes the statements to run.
				body := full[fset.Position(d.Body.Lbrace).Offset+1 : fset.Position(d.Body.Rbrace).Offset]
				p.stmts = dedent(body)
				continue
			}
			if d.Recv == nil && d.Name.Name == "init" {
				return nil, errors.New("init functions are not supported in the playground; call the code directly")
			}
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				name = recvTypeName(d.Recv.List[0].Type) + "." + name
			}
			p.decls = append(p.decls, &cell{kind: declCell, src: text(start, d.End()), names: []string{name}})
		}
	}
	return p, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func recvTypeName(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.Ident:
			return t.Name
		default:
			return "?"
		}
	}
}

const stmtPrefix = "package main\nfunc _() {\n"

// parseStmts parses src as a statement list.
func parseStmts(src string) ([]ast.Stmt, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "input", stmtPrefix+src+"\n}", 0)
	if err != nil {
		return nil, shiftParseError(err, -2)
	}
	return f.Decls[0].(*ast.FuncDecl).Body.List, nil
}

// shiftParseError moves error line numbers back into the user's input.
func shiftParseError(err error, lines int) error {
	var list scanner.ErrorList
	if !errors.As(err, &list) || len(list) == 0 {
		return err
	}
	first := list[0]
	return &posError{line: first.Pos.Line + lines, col: first.Pos.Column, msg: first.Msg}
}

// posError is an error located in the user's input.
type posError struct {
	cell      int // 0 = the current input
	line, col int
	msg       string
}

func (e *posError) Error() string { return fmt.Sprintf("%d:%d: %s", e.line, e.col, e.msg) }

func dedent(s string) string {
	lines := strings.Split(strings.Trim(s, "\n"), "\n")
	prefix := ""
	first := true
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		ws := l[:len(l)-len(strings.TrimLeft(l, " \t"))]
		if first || !strings.HasPrefix(ws, prefix) {
			if first {
				prefix = ws
			} else {
				for !strings.HasPrefix(ws, prefix) {
					prefix = prefix[:len(prefix)-1]
				}
			}
			first = false
		}
	}
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, prefix)
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------- program

// edit inserts text at a byte offset of a cell's source.
type edit struct {
	off  int
	text string
}

func applyEdits(src string, edits []edit) string {
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].off > edits[j].off })
	for _, e := range edits {
		src = src[:e.off] + e.text + src[e.off:]
	}
	return src
}

// stmtPlan describes the statements of the newest cell.
type stmtPlan struct {
	id      int
	src     string     // raw statements
	stmts   []ast.Stmt // parsed with stmtPrefix offsets
	guards  []edit     // `; _ = x` after top-level definitions
	last    ast.Stmt
	pure    bool     // no calls/receives/assignments: no need to replay
	defined []string // names defined by the last statement
}

func planStmts(id int, src string) (*stmtPlan, error) {
	stmts, err := parseStmts(src)
	if err != nil {
		return nil, err
	}
	p := &stmtPlan{id: id, src: src, stmts: stmts, pure: true}
	base := len(stmtPrefix)
	for i, st := range stmts {
		var names []string
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
		if len(names) > 0 {
			blanks := strings.TrimSuffix(strings.Repeat("_, ", len(names)), ", ")
			end := int(st.End()) - 1 - base
			// The trailing /*line*/ restores column numbers for anything
			// after the guard on the same line.
			p.guards = append(p.guards, edit{end, fmt.Sprintf("; %s = %s/*line cell%d:%s*/", blanks, strings.Join(names, ", "), id, lineColumn(src, end))})
		}
		if i == len(stmts)-1 {
			p.last = st
			p.defined = names
		}
		if es, ok := st.(*ast.ExprStmt); !ok || hasEffects(es.X) {
			p.pure = false
		}
	}
	return p, nil
}

// hasEffects reports whether evaluating e could change program state.
func hasEffects(e ast.Expr) bool {
	effects := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			effects = true
		case *ast.UnaryExpr:
			if n.Op == token.ARROW {
				effects = true
			}
		case *ast.FuncLit:
			return false
		}
		return !effects
	})
	return effects
}

// lastExpr returns the expression of the final statement if it is an
// expression statement, with its byte offset within the cell source.
func (p *stmtPlan) lastExpr() (ast.Expr, int, int) {
	es, ok := p.last.(*ast.ExprStmt)
	if !ok {
		return nil, 0, 0
	}
	base := len(stmtPrefix)
	return es.X, int(es.X.Pos()) - 1 - base, int(es.X.End()) - 1 - base
}

// program is a generated main.go and where the newest cell landed in it.
type program struct {
	src       string
	cellStart int // byte offset of the newest statement cell in src, -1 if none
}

type genInput struct {
	decls   []*cell
	replay  []*cell
	imports []importSpec
	current *cell  // newest statement cell (src already transformed), or nil
	tail    string // code run after the newest cell, e.g. variable display
	unmute  bool   // unmute even without a current cell (to show decl vars)
}

func generate(in genInput) program {
	var b strings.Builder
	b.WriteString("package main\n\nimport (\n\t__gop \"" + runtimePath + "\"\n")
	for _, imp := range in.imports {
		if imp.name != "" {
			fmt.Fprintf(&b, "\t%s %q\n", imp.name, imp.path)
		} else {
			fmt.Fprintf(&b, "\t%q\n", imp.path)
		}
	}
	b.WriteString(")\n\nvar _ = __gop.T\n\n")
	for _, c := range in.decls {
		fmt.Fprintf(&b, "//line cell%d:1:1\n%s\n\n", c.id, c.src)
	}
	b.WriteString("//line gotinker:1:1\nfunc main() {\n")
	depth := 0
	for _, c := range in.replay {
		fmt.Fprintf(&b, "{\n//line cell%d:1:1\n%s\n//line gotinker:1:1\n", c.id, c.src)
		depth++
	}
	p := program{cellStart: -1}
	if in.current != nil || in.unmute {
		b.WriteString("{\n__gop.Unmute(); defer __gop.Finish()\n")
		depth++
		if in.current != nil {
			fmt.Fprintf(&b, "//line cell%d:1:1\n", in.current.id)
			p.cellStart = b.Len()
			b.WriteString(in.current.src + "\n")
		}
		if in.tail != "" {
			b.WriteString("//line gotinker:1:1\n" + in.tail + "\n")
		}
	}
	b.WriteString(strings.Repeat("}\n", depth))
	b.WriteString("}\n")
	p.src = b.String()
	return p
}

// ------------------------------------------------------------- type check

type checked struct {
	file    *ast.File
	info    *types.Info
	pkg     *types.Package
	errs    []types.Error
	imports []importSpec // imports actually used
	prog    program
}

var (
	reUnusedImport = regexp.MustCompile(`^"([^"]+)" imported (?:as \S+ )?and not used`)
	reUndefined    = regexp.MustCompile(`^undefined: (\w+)$`)
	reUndefinedSel = regexp.MustCompile(`^undefined: (\w+)\.(\w+)`)
)

// check type-checks the program built from in, adding missing standard
// library imports and dropping unused ones until the import set is stable.
// ignore reports errors that are expected at this stage.
func (s *Session) check(in genInput, ignore func(*checked, types.Error) bool) *checked {
	imports := append([]importSpec(nil), in.imports...)
	tried := map[string]bool{} // import paths already swapped in
	var c *checked
	for iter := 0; iter < 6; iter++ {
		in.imports = imports
		prog := generate(in)
		c = s.typecheck(prog)
		c.imports = imports
		changed := false

		selectors := map[token.Pos]string{} // position of X in X.Sel -> Sel
		if c.file != nil {
			ast.Inspect(c.file, func(n ast.Node) bool {
				if se, ok := n.(*ast.SelectorExpr); ok {
					if id, ok := se.X.(*ast.Ident); ok {
						selectors[id.Pos()] = se.Sel.Name
					}
				}
				return true
			})
		}
		var kept []types.Error
		for _, e := range c.errs {
			if m := reUnusedImport.FindStringSubmatch(e.Msg); m != nil {
				imports = removeImport(imports, m[1])
				changed = true
				continue
			}
			if m := reUndefined.FindStringSubmatch(e.Msg); m != nil {
				if sel, ok := selectors[e.Pos]; ok {
					if path := s.resolvePackage(m[1], sel); path != "" && !hasImport(imports, path) {
						imports = append(imports, importSpec{path: path, auto: true})
						changed = true
						continue
					}
				}
			}
			// rand.Intn with rand bound to math/rand/v2: switch an
			// automatic import to the same-named package that has it.
			if m := reUndefinedSel.FindStringSubmatch(e.Msg); m != nil {
				if alt := s.alternative(imports, m[1], m[2]); alt != "" && !tried[alt] {
					tried[alt] = true
					imports = setImport(imports, importSpec{path: alt, auto: true})
					changed = true
					continue
				}
			}
			if ignore != nil && ignore(c, e) {
				continue
			}
			kept = append(kept, e)
		}
		c.errs = kept
		if !changed {
			break
		}
	}
	// Explain name clashes that swapping could not fix (e.g. rand.IntN and
	// rand.Intn both used: one name cannot be two packages).
	for i, e := range c.errs {
		m := reUndefinedSel.FindStringSubmatch(e.Msg)
		if m == nil {
			continue
		}
		for _, imp := range c.imports {
			if imp.binding() != m[1] {
				continue
			}
			if alt := s.alternative([]importSpec{{path: imp.path, auto: true}}, m[1], m[2]); alt != "" {
				c.errs[i].Msg = fmt.Sprintf("%s\n       %s here means %q (an earlier cell uses it); for %s.%s add: import %s2 %q",
					e.Msg, m[1], imp.path, pkgName(alt), m[2], m[1], alt)
			}
		}
	}
	return c
}

func removeImport(list []importSpec, path string) []importSpec {
	out := list[:0:0]
	for _, i := range list {
		if i.path != path {
			out = append(out, i)
		}
	}
	return out
}

func hasImport(list []importSpec, path string) bool {
	for _, i := range list {
		if i.path == path {
			return true
		}
	}
	return false
}

// resolvePackage picks the standard library package named name that
// exports sel, preferring the conventional choice for ambiguous names.
func (s *Session) resolvePackage(name, sel string) string {
	cands := s.std.byName[name]
	if len(cands) == 0 {
		return ""
	}
	for _, path := range cands {
		pkg, err := s.imp.Import(path)
		if err == nil && pkg.Scope().Lookup(sel) != nil {
			return path
		}
	}
	return cands[0]
}

// alternative returns another standard library package named name that
// exports sel, if the package currently bound to name was auto-imported.
func (s *Session) alternative(imports []importSpec, name, sel string) string {
	current := ""
	for _, imp := range imports {
		if imp.binding() == name {
			if !imp.auto {
				return ""
			}
			current = imp.path
		}
	}
	if current == "" {
		return ""
	}
	for _, path := range s.std.byName[name] {
		if path == current {
			continue
		}
		if pkg, err := s.imp.Import(path); err == nil && pkg.Scope().Lookup(sel) != nil {
			return path
		}
	}
	return ""
}

func (s *Session) typecheck(prog program) *checked {
	c := &checked{prog: prog}
	f, err := parser.ParseFile(s.fset, "main.go", prog.src, parser.ParseComments|parser.AllErrors)
	if err != nil {
		c.errs = append(c.errs, types.Error{Fset: s.fset, Msg: err.Error()})
		if f == nil {
			return c
		}
	}
	c.file = f
	c.info = &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	conf := types.Config{
		Importer:  s.imp,
		GoVersion: "go" + s.goVer,
		Error: func(err error) {
			if te, ok := err.(types.Error); ok {
				c.errs = append(c.errs, te)
			}
		},
	}
	c.pkg, _ = conf.Check("main", s.fset, []*ast.File{f}, c.info)
	return c
}

// qualifier prints package-qualified names the way the user writes them.
func qualifier(p *types.Package) string {
	if p == nil || p.Path() == "main" {
		return ""
	}
	return p.Name()
}

func typeString(t types.Type) string {
	if t == nil {
		return ""
	}
	if b, ok := t.(*types.Basic); ok && b.Info()&types.IsUntyped != 0 {
		return b.Name()
	}
	return types.TypeString(t, qualifier)
}

// ------------------------------------------------------------------- eval

// Eval runs one input through the session.
func (s *Session) Eval(ctx context.Context, src string) {
	if strings.TrimSpace(src) == "" {
		return
	}
	p, err := s.parseInput(src)
	if err != nil {
		s.ui.inputError(src, err)
		return
	}
	// Explicit imports update the session's bindings straight away.
	imports := append([]importSpec(nil), s.imports...)
	for _, imp := range p.imports {
		if !s.std.isStd(imp.path) && imp.path != runtimePath {
			if err := s.ensureModule(ctx, imp.path); err != nil {
				s.ui.errorf("%v", err)
				return
			}
		}
		imports = setImport(imports, imp)
	}

	decls := s.decls
	var newDecls []*cell
	for _, d := range p.decls {
		d.id = s.newID()
		d.input, d.raw, d.out = d.src, d.src, d.src
		decls = replaceDecl(decls, d)
		newDecls = append(newDecls, d)
	}

	if p.stmts == "" && len(newDecls) == 0 {
		// Only imports: validate them by type-checking.
		c := s.check(genInput{decls: s.decls, replay: s.stmts, imports: imports}, ignoreUnusedImports)
		if len(c.errs) > 0 {
			s.ui.typeErrors(c, s.cellByID(decls, nil))
			return
		}
		s.imports = imports
		for _, imp := range p.imports {
			s.ui.note("imported %s", imp.path)
		}
		return
	}

	if p.stmts == "" {
		s.evalDecls(ctx, decls, newDecls, imports)
		return
	}

	plan, err := planStmts(s.newID(), p.stmts)
	if err != nil {
		s.ui.inputError(p.stmts, err)
		return
	}
	s.evalStmts(ctx, decls, newDecls, imports, plan, src)
}

func (s *Session) newID() int {
	id := s.nextID
	s.nextID++
	return id
}

func ignoreUnusedImports(*checked, types.Error) bool { return false }

func setImport(list []importSpec, imp importSpec) []importSpec {
	out := list[:0:0]
	for _, i := range list {
		if i.binding() != imp.binding() && i.path != imp.path {
			out = append(out, i)
		}
	}
	return append(out, imp)
}

func replaceDecl(decls []*cell, d *cell) []*cell {
	out := decls[:0:0]
	for _, old := range decls {
		if !overlaps(old.names, d.names) {
			out = append(out, old)
		}
	}
	return append(out, d)
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func (s *Session) cellByID(extra []*cell, cur *cell) func(int) *cell {
	return func(id int) *cell {
		if cur != nil && cur.id == id {
			return cur
		}
		for _, list := range [][]*cell{extra, s.decls, s.stmts} {
			for _, c := range list {
				if c.id == id {
					return c
				}
			}
		}
		return nil
	}
}

// evalDecls handles input made only of declarations.
func (s *Session) evalDecls(ctx context.Context, decls, newDecls []*cell, imports []importSpec) {
	c := s.check(genInput{decls: decls, replay: s.stmts, imports: imports}, nil)
	if len(c.errs) > 0 {
		// `var y = x * 2` where x is local to main: retry as a statement.
		if len(newDecls) == 1 && isVarDecl(newDecls[0].src) {
			if plan, err := planStmts(newDecls[0].id, newDecls[0].src); err == nil {
				probe := s.check(genInput{decls: s.decls, replay: s.stmts, imports: imports, current: &cell{id: plan.id, src: applyEdits(plan.src, plan.guards)}}, nil)
				if len(probe.errs) == 0 {
					s.evalStmts(ctx, s.decls, nil, imports, plan, newDecls[0].src)
					return
				}
			}
		}
		s.ui.typeErrors(c, s.cellByID(decls, nil))
		return
	}

	// Package-level variables are shown by running the program.
	var vars []string
	var tail strings.Builder
	for _, d := range newDecls {
		for _, n := range d.names {
			if obj, ok := c.pkg.Scope().Lookup(n).(*types.Var); ok {
				vars = append(vars, n)
				fmt.Fprintf(&tail, "__gop.T(%q).Var(%q, %s)\n", typeString(obj.Type()), n, n)
			}
		}
	}
	if len(vars) > 0 {
		run := c
		run.prog = generate(genInput{decls: decls, replay: s.stmts, imports: c.imports, tail: tail.String(), unmute: true})
		if !s.buildAndRun(ctx, run.prog.src, nil) {
			return
		}
	}
	s.decls = decls
	s.imports = mergeImports(s.imports, c.imports)
	s.announce(c, newDecls)
}

// announce commits new declarations and prints a ✓ line for each (variables
// are skipped: they are printed with their values).
func (s *Session) announce(c *checked, newDecls []*cell) {
	for _, d := range newDecls {
		d.committed = true
	}
	s.printDecls(c, newDecls)
}

func (s *Session) printDecls(c *checked, newDecls []*cell) {
	for _, d := range newDecls {
		for _, n := range d.names {
			if strings.Contains(n, ".") {
				s.ui.declared(methodString(c.pkg, n))
				continue
			}
			obj := c.pkg.Scope().Lookup(n)
			if obj == nil {
				continue
			}
			if _, ok := obj.(*types.Var); ok {
				continue
			}
			s.ui.declared(objectString(obj))
		}
	}
}

func isVarDecl(src string) bool {
	t := strings.TrimSpace(src)
	return strings.HasPrefix(t, "var ") || strings.HasPrefix(t, "var(")
}

func objectString(obj types.Object) string {
	s := types.ObjectString(obj, qualifier)
	if tn, ok := obj.(*types.TypeName); ok {
		under := types.TypeString(tn.Type().Underlying(), qualifier)
		s = "type " + tn.Name() + " " + under
	}
	if len(s) > 160 {
		s = s[:157] + "..."
	}
	return s
}

func methodString(pkg *types.Package, name string) string {
	parts := strings.SplitN(name, ".", 2)
	obj := pkg.Scope().Lookup(parts[0])
	if obj == nil {
		return "func (" + parts[0] + ") " + parts[1]
	}
	ms := types.NewMethodSet(types.NewPointer(obj.Type()))
	for i := 0; i < ms.Len(); i++ {
		if f := ms.At(i).Obj(); f.Name() == parts[1] {
			sig := f.Type().(*types.Signature)
			recv := types.TypeString(sig.Recv().Type(), qualifier)
			return "func (" + recv + ") " + f.Name() + strings.TrimPrefix(types.TypeString(sig, qualifier), "func")
		}
	}
	return "func (" + parts[0] + ") " + parts[1]
}

func mergeImports(have, add []importSpec) []importSpec {
	out := append([]importSpec(nil), have...)
	for _, imp := range add {
		if !hasImport(out, imp.path) {
			out = setImport(out, imp)
		}
	}
	return out
}

// evalStmts type-checks, compiles and runs a statement cell.
func (s *Session) evalStmts(ctx context.Context, decls, newDecls []*cell, imports []importSpec, plan *stmtPlan, input string) {
	cur := &cell{id: plan.id, kind: stmtCell, src: applyEdits(plan.src, plan.guards), input: input, raw: plan.src}
	_, exprStart, exprEnd := plan.lastExpr()

	// Pass 1: learn the type of the final expression. A bare non-call
	// expression is "not used" until we wrap it for display.
	in := genInput{decls: decls, replay: s.stmts, imports: imports, current: cur}
	c := s.check(in, notUsed(plan))
	if len(c.errs) > 0 {
		s.ui.typeErrors(c, s.cellByID(decls, cur))
		return
	}
	tv, pos, showExpr := exprType(c, plan)
	if showExpr && (tv.IsVoid() || isPrintCall(c, pos)) {
		showExpr = false
	}

	if showExpr && tv.IsType() && len(plan.stmts) > 1 {
		s.ui.inputError(plan.src, &posError{line: strings.Count(plan.src[:exprStart], "\n") + 1, col: 1, msg: "a type is not an expression"})
		return
	}
	// Constant and type expressions are answered by the type checker alone
	// (constants of named types run, so their String method is used).
	if showExpr && len(plan.stmts) == 1 {
		if tv.IsType() {
			s.ui.typeInfo(tv.Type)
			return
		}
		if _, basic := tv.Type.(*types.Basic); tv.Value != nil && basic {
			s.ui.constant(tv.Value, typeString(tv.Type))
			return
		}
	}

	// A value-producing final expression is replayed as `_ = expr`, since a
	// bare value is not a valid statement. Exported programs keep it only
	// if it is a real function call (it may have effects).
	cur.out = plan.src
	if showExpr && !isFuncCall(c, pos) {
		cur.out = strings.TrimRight(plan.src[:exprStart], " \t\n;")
	}
	if showExpr {
		n := 1
		if tup, ok := tv.Type.(*types.Tuple); ok {
			n = tup.Len()
		}
		blank := strings.TrimSuffix(strings.Repeat("_, ", n), ", ") + " = "
		cur.src = applyEdits(plan.src, append(append([]edit(nil), plan.guards...), edit{exprStart, blank}))
	}

	edits := append([]edit(nil), plan.guards...)
	if showExpr {
		lineCol := lineColumn(plan.src, exprStart)
		edits = append(edits,
			edit{exprStart, fmt.Sprintf("__gop.T(%s).Show(/*line cell%d:%s*/", typeArgs(tv.Type), plan.id, lineCol)},
			edit{exprEnd, ")"})
	}
	var tail strings.Builder
	if len(plan.defined) > 0 {
		for _, n := range plan.defined {
			fmt.Fprintf(&tail, "__gop.T(%q).Var(%q, %s)\n", definedType(c, n, plan.id), n, n)
		}
	}
	run := &cell{id: plan.id, kind: stmtCell, src: applyEdits(plan.src, edits)}
	s.printDecls(c, newDecls) // declarations are final once they type-check
	in = genInput{decls: decls, replay: s.stmts, imports: c.imports, current: run, tail: tail.String()}
	prog := generate(in)
	if !s.buildAndRun(ctx, prog.src, s.cellByID(decls, cur)) {
		return
	}

	// Success: commit.
	s.decls = decls
	s.imports = mergeImports(s.imports, c.imports)
	for _, d := range newDecls {
		d.committed = true
	}
	if !plan.pure {
		cur.committed = true
		s.stmts = append(s.stmts, cur)
	}
}

// guardShift is how far edits before off move it (guards precede the final
// expression only when it is not the first statement).
func guardShift(guards []edit, off int) int {
	n := 0
	for _, g := range guards {
		if g.off <= off {
			n += len(g.text)
		}
	}
	return n
}

func (c *checked) fileStart() token.Pos {
	if c.file == nil {
		return token.NoPos
	}
	return token.Pos(c.file.FileStart)
}

func lineColumn(src string, off int) string {
	line := strings.Count(src[:off], "\n") + 1
	col := off - strings.LastIndex(src[:off], "\n")
	return fmt.Sprintf("%d:%d", line, col)
}

func typeArgs(t types.Type) string {
	var names []string
	if tup, ok := t.(*types.Tuple); ok {
		for i := 0; i < tup.Len(); i++ {
			names = append(names, strconv.Quote(typeString(tup.At(i).Type())))
		}
	} else {
		// A non-constant untyped value (a comparison) has its default type.
		names = append(names, strconv.Quote(typeString(types.Default(t))))
	}
	return strings.Join(names, ", ")
}

// definedType finds the type of a variable defined by the newest cell.
func definedType(c *checked, name string, id int) string {
	var best types.Object
	for ident, obj := range c.info.Defs {
		if ident.Name != name || obj == nil {
			continue
		}
		if _, ok := obj.(*types.Var); !ok {
			continue
		}
		pos := c.prog.cellStart
		if pos < 0 || int(ident.Pos()-c.fileStart()) < pos {
			continue
		}
		if best == nil || obj.Pos() > best.Pos() {
			best = obj
		}
	}
	if best == nil {
		return ""
	}
	return typeString(best.Type())
}

// isFuncCall reports whether the expression at pos calls a function (as
// opposed to a conversion or a builtin such as len).
func isFuncCall(c *checked, pos token.Pos) bool {
	var call *ast.CallExpr
	ast.Inspect(c.file, func(n ast.Node) bool {
		if ce, ok := n.(*ast.CallExpr); ok && ce.Pos() == pos {
			call = ce
		}
		return call == nil
	})
	if call == nil {
		return false
	}
	tv, ok := c.info.Types[call.Fun]
	return ok && tv.IsValue() && !tv.IsBuiltin()
}

// isPrintCall reports whether expr is a call of fmt.Print*/Fprint*, whose
// (n int, err error) results are noise in a playground.
func isPrintCall(c *checked, pos token.Pos) bool {
	var call *ast.CallExpr
	ast.Inspect(c.file, func(n ast.Node) bool {
		if ce, ok := n.(*ast.CallExpr); ok && ce.Pos() == pos {
			call = ce
		}
		return call == nil
	})
	if call == nil {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	obj := c.info.Uses[sel.Sel]
	if obj == nil || obj.Pkg() == nil || obj.Pkg().Path() != "fmt" {
		return false
	}
	n := obj.Name()
	return strings.HasPrefix(n, "Print") || strings.HasPrefix(n, "Fprint")
}

// ------------------------------------------------------------- build/run

// buildAndRun compiles and runs a program, streaming its output. It reports
// whether the program built and exited successfully.
func (s *Session) buildAndRun(ctx context.Context, src string, lookup func(int) *cell) bool {
	if err := os.WriteFile(filepath.Join(s.dir, "main.go"), []byte(src), 0o644); err != nil {
		s.ui.errorf("%v", err)
		return false
	}
	bin := filepath.Join(s.dir, "prog")
	start := time.Now()
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-ldflags=-s -w", "-o", bin, ".")
	build.Dir = s.dir
	out, err := build.CombinedOutput()
	buildTime := time.Since(start)
	if err != nil {
		if ctx.Err() != nil {
			s.ui.note("interrupted")
			return false
		}
		s.ui.buildErrors(string(out), lookup)
		return false
	}

	cmd := exec.Command(bin)
	cmd.Dir = s.cwd
	cmd.Stdin = os.Stdin
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	cmd.Env = append(os.Environ(), "GOTINKER_MUTE=1")
	if s.ui.color {
		cmd.Env = append(cmd.Env, "GOTINKER_COLOR=1")
	}

	runStart := time.Now()
	if err := cmd.Start(); err != nil {
		s.ui.errorf("%v", err)
		return false
	}
	// Output is copied as it arrives; remembering the last byte lets us end
	// an unterminated last line before the next prompt.
	progOut := &lastByteWriter{w: s.ui.out}
	outDone := make(chan struct{})
	go func() {
		io.Copy(progOut, stdout)
		close(outDone)
	}()
	errDone := make(chan struct{})
	go func() {
		s.ui.programStderr(stderr, lookup)
		close(errDone)
	}()
	waitErr := make(chan error, 1)
	go func() {
		<-outDone
		<-errDone
		waitErr <- cmd.Wait()
	}()

	interrupted := false
	var runErr error
	select {
	case runErr = <-waitErr:
	case <-ctx.Done():
		// The terminal already delivered SIGINT to the program; give it a
		// moment to exit on its own before killing it.
		interrupted = true
		select {
		case runErr = <-waitErr:
		case <-time.After(time.Second):
			_ = cmd.Process.Kill()
			runErr = <-waitErr
		}
	}
	runTime := time.Since(runStart)
	if interrupted || (progOut.n > 0 && progOut.last != '\n') {
		fmt.Fprintln(s.ui.out) // end the line (or move past the echoed ^C)
	}
	s.ui.timing(buildTime, runTime)
	if interrupted {
		s.ui.note("interrupted (input discarded)")
		return false
	}
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) && ee.ExitCode() == 2 {
			s.ui.note("input discarded")
		} else if errors.As(runErr, &ee) && ee.ExitCode() > 0 {
			s.ui.note("exit status %d (input discarded)", ee.ExitCode())
		} else {
			s.ui.note("%v (input discarded)", runErr)
		}
		return false
	}
	return true
}

type lastByteWriter struct {
	w    io.Writer
	n    int
	last byte
}

func (l *lastByteWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		l.n += len(p)
		l.last = p[len(p)-1]
	}
	return l.w.Write(p)
}

// ensureModule adds a third-party module providing importPath to go.mod.
func (s *Session) ensureModule(ctx context.Context, importPath string) error {
	list := exec.CommandContext(ctx, "go", "list", "-e", "-f", "{{if .Error}}missing{{end}}", importPath)
	list.Dir = s.dir
	if out, err := list.Output(); err == nil && strings.TrimSpace(string(out)) == "" {
		return nil
	}
	return s.goGet(ctx, importPath)
}

func (s *Session) goGet(ctx context.Context, target string) error {
	s.ui.note("fetching %s ...", target)
	cmd := exec.CommandContext(ctx, "go", "get", target)
	cmd.Dir = s.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go get %s: %s", target, strings.TrimSpace(string(out)))
	}
	for _, f := range []string{"go.mod", "go.sum"} {
		if data, err := os.ReadFile(filepath.Join(s.dir, f)); err == nil {
			_ = os.WriteFile(filepath.Join(s.modTmpl, f), data, 0o644)
		}
	}
	s.imp.reset()
	return nil
}
