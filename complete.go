package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/m-d-nabeel/gotinker/third_party/readline"
)

// completer provides Tab completion and signature hints from the type
// checker: it sees the session's variables, functions and types, the
// fields and methods of any expression, and every standard library package.
type completer struct {
	s *Session

	version int // bumped when the session changes
	probes  map[string]probeResult

	hintKey  string
	hintText []rune
}

func newCompleter(s *Session) *completer {
	return &completer{s: s, probes: map[string]probeResult{}}
}

func (c *completer) invalidate() {
	c.version++
	c.probes = map[string]probeResult{}
	c.hintKey = ""
}

// probeResult is what the type checker knows at the cursor: the type of an
// expression there, and the scope (so locals of a half-typed function count).
type probeResult struct {
	tv    types.TypeAndValue
	ok    bool
	scope *types.Scope
}

// probe type-checks the input typed so far, with expr placed at the cursor
// inside a call to __gop.P and any open brackets closed, and reports what
// the checker learned about expr. Results are cached per input.
func (c *completer) probe(before, expr string) probeResult {
	key := fmt.Sprint(c.version, "\x00", before, "\x00", expr)
	if r, ok := c.probes[key]; ok {
		return r
	}
	code := repair(before, "__gop.P("+expr+")")
	var res probeResult
	var chk *checked
	if parsesAsDecl(code) {
		d := &cell{id: c.s.nextID, kind: declCell, src: code}
		chk = c.s.check(genInput{decls: append(append([]*cell(nil), c.s.decls...), d), replay: c.s.stmts, imports: c.s.imports}, nil)
	} else {
		cur := &cell{id: c.s.nextID, kind: stmtCell, src: code}
		chk = c.s.check(genInput{decls: c.s.decls, replay: c.s.stmts, imports: c.s.imports, current: cur}, nil)
	}
	if chk.file != nil && chk.pkg != nil {
		// The probe is the last __gop.P call in the file.
		var call *ast.CallExpr
		ast.Inspect(chk.file, func(n ast.Node) bool {
			if ce, ok := n.(*ast.CallExpr); ok {
				if sel, ok := ce.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "P" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "__gop" {
						call = ce
					}
				}
			}
			return true
		})
		if call != nil {
			res.scope = chk.pkg.Scope().Innermost(call.Pos())
			if len(call.Args) == 1 {
				res.tv, res.ok = chk.info.Types[call.Args[0]]
			}
		}
	}
	if len(c.probes) > 64 {
		c.probes = map[string]probeResult{}
	}
	c.probes[key] = res
	return res
}

// repair completes the half-typed input before so that probe sits where the
// cursor is in code that parses: open brackets are closed, and if that is not
// enough (the cursor is in a `for`/`if`/`switch` header) an empty body is
// added after the header.
func repair(before, probe string) string {
	open := closers(before)
	parens := strings.TrimRight(open, "\n}")
	braces := open[len(parens):]
	candidates := []string{
		before + probe + open,
		before + probe + parens + " {}" + braces,
		before + probe + parens + "\n" + braces,
	}
	for _, c := range candidates {
		if parses(c) {
			return c
		}
	}
	return candidates[0]
}

func parses(code string) bool {
	if parsesAsDecl(code) {
		return true
	}
	_, err := parseStmts(code)
	return err == nil
}

// parsesAsDecl reports whether code is package-level declarations (other
// than var: a var probe belongs in main, where locals are visible). A
// function literal such as `func(n int) {...}` starts with "func" but is a
// statement.
func parsesAsDecl(code string) bool {
	if !declKeyword.MatchString(code) || strings.HasPrefix(strings.TrimSpace(code), "var") {
		return false
	}
	_, err := parser.ParseFile(token.NewFileSet(), "", "package main\n"+code, 0)
	return err == nil
}

// closers returns the brackets needed to close those left open in src,
// with a newline before each closing brace so statements stay terminated.
func closers(src string) string {
	var s scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	s.Init(file, []byte(src), func(token.Position, string) {}, 0)
	var stack []token.Token
	for {
		_, tok, _ := s.Scan()
		if tok == token.EOF {
			break
		}
		switch tok {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			stack = append(stack, tok)
		case token.RPAREN, token.RBRACK, token.RBRACE:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	var b strings.Builder
	for i := len(stack) - 1; i >= 0; i-- {
		switch stack[i] {
		case token.LPAREN:
			b.WriteString(")")
		case token.LBRACK:
			b.WriteString("]")
		case token.LBRACE:
			b.WriteString("\n}")
		}
	}
	return b.String()
}

var keywords = []string{"break", "case", "chan", "const", "continue", "default", "defer", "else",
	"fallthrough", "for", "func", "go", "goto", "if", "import", "interface", "map", "package",
	"range", "return", "select", "struct", "switch", "type", "var"}

func isIdentRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// chainAt splits the text before the cursor into a selector chain and the
// partial identifier being typed: "fmt.Println(strings.To" -> "strings", "To".
func chainAt(line []rune, cursor int) (start int, base, partial string) {
	i := cursor
	for i > 0 && isIdentRune(line[i-1]) {
		i--
	}
	partial = string(line[i:cursor])
	start = i
	if i > 0 && line[i-1] == '.' {
		// Walk back over the operand: identifiers, dots and balanced
		// (...) / [...] groups, e.g. `xs[0].Name.` or `f(1).`.
		j := i - 1
		depth := 0
		for j > 0 {
			r := line[j-1]
			switch {
			case r == ')' || r == ']':
				depth++
			case (r == '(' || r == '[') && depth > 0:
				depth--
			case depth > 0 || isIdentRune(r) || r == '.':
			default:
				goto done
			}
			j--
		}
	done:
		base = string(line[j : i-1])
		start = j
	}
	return start, base, partial
}

func (c *completer) complete(line []rune, cursor int) readline.Completions {
	text := string(line[:cursor])
	if strings.HasPrefix(strings.TrimSpace(text), ":") {
		return c.completeCommand(line, cursor)
	}
	if inStringOrComment(text) {
		return readline.Completions{}
	}
	start, base, partial := chainAt(line, cursor)
	before := string(line[:start])
	var cands []readline.Completion
	if base != "" {
		cands = c.members(before, base, partial)
	} else {
		cands = c.identifiers(before, partial)
	}
	prefix := string(line[start:cursor])
	if prefix == "" && cursor > 0 && !unicode.IsSpace(line[cursor-1]) {
		// readline needs a non-empty prefix to replace; anchor on the
		// previous character (e.g. the "(" in "fmt.Println(").
		prefix = string(line[cursor-1 : cursor])
	}
	head := strings.TrimSuffix(prefix, partial)
	for i := range cands {
		cands[i].Value = head + cands[i].Value
	}
	comps := readline.CompleteRaw(cands)
	comps.PREFIX = prefix
	if prefix == "" {
		comps.PREFIX = ""
	}
	return comps
}

// inStringOrComment reports whether the cursor is inside a string, rune or
// comment, where completion would only get in the way.
func inStringOrComment(text string) bool {
	var s scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(text))
	inside := false
	s.Init(file, []byte(text), func(_ token.Position, msg string) {
		if strings.Contains(msg, "not terminated") {
			inside = true
		}
	}, scanner.ScanComments)
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.COMMENT && strings.HasPrefix(lit, "//") && file.Offset(pos)+len(lit) >= len(text) {
			return true
		}
	}
	return inside
}

func describe(obj types.Object) (tag, desc string) {
	switch o := obj.(type) {
	case *types.Func:
		sig := o.Type().(*types.Signature)
		tag := "functions"
		if sig.Recv() != nil {
			tag = "methods"
		}
		return tag, "func" + strings.TrimPrefix(types.TypeString(sig, qualifier), "func")
	case *types.Var:
		if o.IsField() {
			return "fields", typeString(o.Type())
		}
		return "variables", typeString(o.Type())
	case *types.Const:
		val := ""
		if o.Val() != nil {
			val = " = " + o.Val().ExactString()
			if len(val) > 30 {
				val = ""
			}
		}
		return "constants", typeString(o.Type()) + val
	case *types.TypeName:
		under := o.Type().Underlying()
		kind := "type"
		switch under.(type) {
		case *types.Struct:
			kind = "struct"
		case *types.Interface:
			kind = "interface"
		default:
			kind = typeString(under)
		}
		return "types", kind
	case *types.PkgName:
		return "packages", o.Imported().Path()
	case *types.Builtin:
		return "builtins", "builtin"
	}
	return "", ""
}

func candidate(name string, obj types.Object) readline.Completion {
	tag, desc := describe(obj)
	if len(desc) > 70 {
		desc = desc[:67] + "..."
	}
	return readline.Completion{Value: name, Display: name, Description: desc, Tag: tag}
}

// members completes after "base.": package members, or the fields and
// methods of base's type.
func (c *completer) members(before, base, partial string) []readline.Completion {
	var out []readline.Completion
	add := func(name string, obj types.Object) {
		if strings.HasPrefix(strings.ToLower(name), strings.ToLower(partial)) {
			out = append(out, candidate(name, obj))
		}
	}
	if pkg := c.packageFor(before, base); pkg != nil {
		scope := pkg.Scope()
		for _, n := range scope.Names() {
			if token.IsExported(n) {
				add(n, scope.Lookup(n))
			}
		}
		return out
	}
	pr := c.probe(before, base)
	if !pr.ok || pr.tv.Type == nil {
		return nil
	}
	tv, t := pr.tv, pr.tv.Type
	seen := map[string]bool{}
	// Fields, including promoted ones from embedded structs.
	var walk func(t types.Type, depth int)
	walk = func(t types.Type, depth int) {
		if depth > 4 {
			return
		}
		if p, ok := t.Underlying().(*types.Pointer); ok {
			t = p.Elem()
		}
		st, ok := t.Underlying().(*types.Struct)
		if !ok {
			return
		}
		for i := 0; i < st.NumFields(); i++ {
			f := st.Field(i)
			if !seen[f.Name()] && (f.Exported() || f.Pkg() == nil || f.Pkg().Path() == "main") {
				seen[f.Name()] = true
				add(f.Name(), f)
			}
			if f.Embedded() {
				walk(f.Type(), depth+1)
			}
		}
	}
	if !tv.IsType() {
		walk(t, 0)
	}
	// Methods: the method set of *T covers both receiver kinds.
	mt := t
	if _, isPtr := t.Underlying().(*types.Pointer); !isPtr && !types.IsInterface(t) {
		mt = types.NewPointer(t)
	}
	ms := types.NewMethodSet(mt)
	for i := 0; i < ms.Len(); i++ {
		f := ms.At(i).Obj()
		if !seen[f.Name()] && (f.Exported() || f.Pkg() == nil || f.Pkg().Path() == "main") {
			seen[f.Name()] = true
			add(f.Name(), f)
		}
	}
	sortCandidates(out)
	return out
}

// packageFor returns the package that base names, if it is a package:
// either imported in the session or a standard library package that would
// be auto-imported.
func (c *completer) packageFor(before, base string) *types.Package {
	if strings.ContainsAny(base, ".()[]") {
		return nil
	}
	// A variable in scope shadows a package of the same name.
	if sc := c.probe(before, "").scope; sc != nil {
		if _, obj := sc.LookupParent(base, token.NoPos); obj != nil {
			if _, isPkg := obj.(*types.PkgName); !isPkg {
				return nil
			}
		}
	}
	for _, imp := range c.s.imports {
		if imp.binding() == base {
			if pkg, err := c.s.imp.Import(imp.path); err == nil {
				return pkg
			}
		}
	}
	if paths := c.s.std.byName[base]; len(paths) > 0 {
		if pkg, err := c.s.imp.Import(paths[0]); err == nil {
			return pkg
		}
	}
	return nil
}

// identifiers completes a bare identifier: session names, builtins,
// keywords and package names.
func (c *completer) identifiers(before, partial string) []readline.Completion {
	if partial == "" {
		return nil
	}
	lower := strings.ToLower(partial)
	match := func(n string) bool { return strings.HasPrefix(strings.ToLower(n), lower) }
	seen := map[string]bool{}
	var out []readline.Completion
	add := func(comp readline.Completion) {
		if !seen[comp.Value] && match(comp.Value) {
			seen[comp.Value] = true
			out = append(out, comp)
		}
	}

	scope := c.probe(before, "").scope
	if scope == nil {
		scope = types.Universe
	}
	for sc := scope; sc != nil; sc = sc.Parent() {
		for _, n := range sc.Names() {
			if strings.HasPrefix(n, "__") || n == "_" || n == "main" {
				continue
			}
			obj := sc.Lookup(n)
			if _, ok := obj.(*types.PkgName); ok && n == "__gop" {
				continue
			}
			add(candidate(n, obj))
		}
	}
	for _, imp := range c.s.imports {
		add(readline.Completion{Value: imp.binding(), Display: imp.binding(), Description: imp.path, Tag: "packages"})
	}
	for _, n := range c.s.std.names() {
		desc := "package"
		if p := c.s.std.byName[n][0]; p != n {
			desc = "package " + p
		}
		add(readline.Completion{Value: n, Display: n, Description: desc, Tag: "packages"})
	}
	for _, k := range keywords {
		add(readline.Completion{Value: k, Display: k, Tag: "keywords"})
	}
	sortCandidates(out)
	return out
}

// tagOrder ranks completion groups: what the user defined first, then the
// standard library, then language keywords.
var tagOrder = map[string]int{"variables": 0, "fields": 1, "methods": 2, "functions": 3, "constants": 4,
	"types": 5, "packages": 6, "builtins": 7, "keywords": 8}

func sortCandidates(c []readline.Completion) {
	sort.SliceStable(c, func(i, j int) bool {
		ti, tj := tagOrder[c[i].Tag], tagOrder[c[j].Tag]
		if ti != tj {
			return ti < tj
		}
		return c[i].Value < c[j].Value
	})
}

func (c *completer) completeCommand(line []rune, cursor int) readline.Completions {
	text := strings.TrimLeft(string(line[:cursor]), " ")
	name, arg, hasArg := strings.Cut(strings.TrimPrefix(text, ":"), " ")
	if !hasArg {
		var vals []string
		for _, cmd := range commands {
			vals = append(vals, ":"+cmd.name, cmd.help)
		}
		comps := readline.CompleteValuesDescribed(vals...)
		comps.PREFIX = text
		return comps.Tag("commands")
	}
	switch name {
	case "doc", "d", "type", "t":
		start, base, partial := chainAt(line, cursor)
		var cands []readline.Completion
		if base != "" {
			cands = c.members("", base, partial)
		} else {
			cands = c.identifiers("", partial)
		}
		prefix := string(line[start:cursor])
		head := strings.TrimSuffix(prefix, partial)
		for i := range cands {
			cands[i].Value = head + cands[i].Value
		}
		comps := readline.CompleteRaw(cands)
		comps.PREFIX = prefix
		return comps
	case "help", "h":
		var vals []string
		for _, cmd := range commands {
			vals = append(vals, cmd.name, cmd.help)
		}
		comps := readline.CompleteValuesDescribed(vals...)
		comps.PREFIX = arg
		return comps
	case "load", "l", "save", "w":
		return completeFiles(c.s.cwd, arg)
	}
	return readline.Completions{}
}

// ------------------------------------------------------------------ hints

// hint shows the signature of the call the cursor is in, or of the
// function name just typed.
func (c *completer) hint(line []rune, cursor int) []rune {
	if cursor > len(line) {
		cursor = len(line)
	}
	text := string(line[:cursor])
	if strings.TrimSpace(text) == "" || strings.HasPrefix(strings.TrimSpace(text), ":") || inStringOrComment(text) {
		return nil
	}
	callee, calleeStart, argIndex := enclosingCall(line[:cursor])
	if callee == "" {
		return nil
	}
	before := string(line[:calleeStart])
	key := fmt.Sprint(c.version, "\x00", before, "\x00", callee)
	if key == c.hintKey {
		if c.hintText == nil {
			return nil
		}
		return c.decorate(c.hintText, argIndex)
	}
	c.hintKey, c.hintText = key, nil
	pr := c.probe(before, callee)
	if !pr.ok || pr.tv.Type == nil || pr.tv.IsType() {
		return nil
	}
	sig, ok := pr.tv.Type.Underlying().(*types.Signature)
	if !ok {
		return nil
	}
	c.hintText = []rune(callee + strings.TrimPrefix(types.TypeString(sig, qualifier), "func"))
	return c.decorate(c.hintText, argIndex)
}

// decorate highlights the parameter the cursor is on.
func (c *completer) decorate(sig []rune, argIndex int) []rune {
	s := string(sig)
	if !c.s.ui.color {
		return []rune(s)
	}
	open := strings.Index(s, "(")
	if open < 0 {
		return []rune(sGray + s + sReset)
	}
	// Find the parameter list's closing paren.
	depth, closeIdx := 0, -1
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 && closeIdx < 0 {
				closeIdx = i
			}
		}
		if closeIdx >= 0 {
			break
		}
	}
	if closeIdx < 0 {
		return []rune(sGray + s + sReset)
	}
	params := splitTopLevel(s[open+1 : closeIdx])
	if argIndex >= len(params) && len(params) > 0 && strings.Contains(params[len(params)-1], "...") {
		argIndex = len(params) - 1
	}
	var b strings.Builder
	b.WriteString(sGray + s[:open+1])
	for i, p := range params {
		if i > 0 {
			b.WriteString(", ")
		}
		if i == argIndex {
			b.WriteString(sReset + sBold + sYellow + p + sReset + sGray)
		} else {
			b.WriteString(p)
		}
	}
	b.WriteString(s[closeIdx:] + sReset)
	return []rune(b.String())
}

func splitTopLevel(s string) []string {
	var parts []string
	depth, last := 0, 0
	for i, r := range s {
		switch r {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(s[last:i]))
				last = i + 1
			}
		}
	}
	if strings.TrimSpace(s[last:]) != "" {
		parts = append(parts, strings.TrimSpace(s[last:]))
	}
	return parts
}

// enclosingCall finds the innermost unclosed call before the cursor and
// returns the callee expression and the index of the argument being typed.
func enclosingCall(line []rune) (callee string, start, arg int) {
	depth := 0
	args := 0
	for i := len(line) - 1; i >= 0; i-- {
		switch line[i] {
		case ')', ']', '}':
			depth++
		case '[', '{':
			if depth == 0 {
				return "", 0, 0
			}
			depth--
		case ',':
			if depth == 0 {
				args++
			}
		case '"', '`', '\'':
			// Skip back over a string literal.
			q := line[i]
			for i--; i >= 0 && line[i] != q; i-- {
			}
		case '(':
			if depth > 0 {
				depth--
				continue
			}
			st, base, partial := chainAt(line, i)
			if partial == "" || (base == "" && keywordSet[partial]) {
				return "", 0, 0
			}
			if base != "" {
				return base + "." + partial, st, args
			}
			return partial, st, args
		}
	}
	return "", 0, 0
}

var keywordSet = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range keywords {
		m[k] = true
	}
	return m
}()

// completeFiles completes paths for :load and :save.
func completeFiles(cwd, arg string) readline.Completions {
	dir, partial := filepath.Split(arg)
	base := expandHome(dir)
	if !filepath.IsAbs(base) {
		base = filepath.Join(cwd, base)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return readline.Completions{}
	}
	var cands []readline.Completion
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, partial) || (strings.HasPrefix(name, ".") && !strings.HasPrefix(partial, ".")) {
			continue
		}
		switch {
		case e.IsDir():
			cands = append(cands, readline.Completion{Value: dir + name + "/", Display: name + "/", Tag: "directories"})
		case strings.HasSuffix(name, ".go"):
			cands = append(cands, readline.Completion{Value: dir + name, Display: name, Tag: "go files"})
		}
	}
	comps := readline.CompleteRaw(cands)
	comps.PREFIX = arg
	return comps.NoSpace('/')
}
