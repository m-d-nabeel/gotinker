package main

import (
	"bytes"
	"context"
	"fmt"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/term"
)

type command struct {
	name    string
	aliases []string
	args    string
	help    string
	run     func(s *Session, r *repl, arg string) (quit bool)
}

var commands []command

func init() {
	commands = []command{
		{name: "help", aliases: []string{"h", "?"}, args: "[command]", help: "show help", run: cmdHelp},
		{name: "doc", aliases: []string{"d"}, args: "<symbol>", help: "documentation, e.g. :doc strings.Cut, :doc http.Client.Do, :doc myFunc", run: cmdDoc},
		{name: "type", aliases: []string{"t"}, args: "<expr>", help: "show the type of an expression without running it", run: cmdType},
		{name: "vars", aliases: []string{"v"}, help: "list variables in scope with their current values", run: cmdVars},
		{name: "imports", help: "list imported packages", run: cmdImports},
		{name: "cells", aliases: []string{"ls"}, help: "list the inputs that make up the session", run: cmdCells},
		{name: "drop", args: "<cell>...", help: "remove cells from the session (see :cells)", run: cmdDrop},
		{name: "undo", aliases: []string{"u"}, help: "remove the most recent cell", run: cmdUndo},
		{name: "show", aliases: []string{"src"}, help: "print the session as a standalone Go program", run: cmdShow},
		{name: "save", aliases: []string{"w"}, args: "<file.go>", help: "save the session as a standalone Go program", run: cmdSave},
		{name: "load", aliases: []string{"l"}, args: "<file.go>", help: "load a Go file (declarations and main body) into the session", run: cmdLoad},
		{name: "edit", aliases: []string{"e"}, help: "edit the whole session in $EDITOR, then re-run it", run: cmdEdit},
		{name: "get", args: "<module>[@version]", help: "add a third-party module, e.g. :get github.com/google/uuid", run: cmdGet},
		{name: "reset", help: "start over with an empty session", run: cmdReset},
		{name: "clear", aliases: []string{"cls"}, help: "clear the screen", run: cmdClear},
		{name: "quit", aliases: []string{"q", "exit"}, help: "leave the playground (or Ctrl-D)", run: func(*Session, *repl, string) bool { return true }},
	}
}

func findCommand(name string) *command {
	for i, c := range commands {
		if c.name == name {
			return &commands[i]
		}
		for _, a := range c.aliases {
			if a == name {
				return &commands[i]
			}
		}
	}
	return nil
}

// runCommand executes a :command line. It reports whether gotinker should exit.
func runCommand(s *Session, r *repl, line string) bool {
	line = strings.TrimPrefix(strings.TrimSpace(line), ":")
	name, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	c := findCommand(name)
	if c == nil {
		s.ui.errorf("unknown command :%s (try :help)", name)
		return false
	}
	return c.run(s, r, arg)
}

func cmdHelp(s *Session, _ *repl, arg string) bool {
	u := s.ui
	if arg != "" {
		c := findCommand(strings.TrimPrefix(arg, ":"))
		if c == nil {
			u.errorf("unknown command :%s", arg)
			return false
		}
		fmt.Fprintf(u.out, "%s %s\n  %s\n", u.style(sBold+sCyan, ":"+c.name), c.args, c.help)
		if len(c.aliases) > 0 {
			fmt.Fprintf(u.out, "  %s\n", u.style(sGray, "aliases: :"+strings.Join(c.aliases, ", :")))
		}
		return false
	}
	fmt.Fprintln(u.out, u.style(sBold, "Type Go code and press Enter.")+` Statements, expressions and declarations
all work; the session is a real Go program compiled with your Go toolchain.

  `+u.code(`x := []int{3, 1, 2}`)+`        variables persist between inputs
  `+u.code(`slices.Sort(x); x`)+`          packages are imported automatically
  `+u.code(`func sq(n int) int { return n * n }`)+`  redefine functions any time
  `+u.code(`for i := range 3 { fmt.Println(i) }`)+`

`+u.style(sBold, "Keys")+`
  Enter         run; but a new indented line when not on the last line,
                or right after an opening bracket ({|} opens the block)
  Alt-Enter     new line, always          Ctrl-J   run from anywhere
  ↑ / ↓         move between lines; history from the first / last line
  Tab           indent at the start of a line, otherwise complete
  ( [ { " '     close automatically (backquotes too); typing the closer
                steps over it, Backspace in an empty pair deletes both
  Alt-← / Alt-→ previous / next word      Alt-Backspace / Alt-D  delete word
  Ctrl-R        search history            →  accept the grey suggestion
  Ctrl-X Ctrl-E edit the input in $EDITOR
  Ctrl-C        cancel input, or stop a running program
  Ctrl-D        exit

`+u.style(sBold, "Commands"))
	for _, c := range commands {
		name := ":" + c.name
		if c.args != "" {
			name += " " + c.args
		}
		fmt.Fprintf(u.out, "  %s %s\n", u.style(sCyan, fmt.Sprintf("%-22s", name)), c.help)
	}
	fmt.Fprintln(u.out, "\n"+u.style(sGray, `Each input re-runs the session with earlier output hidden, so values from
rand or time.Now are recomputed on every run, and earlier stdin reads see EOF.`))
	return false
}

func cmdDoc(s *Session, _ *repl, arg string) bool {
	if arg == "" {
		s.ui.errorf("usage: :doc <symbol>   e.g. :doc strings.Index, :doc -all fmt")
		return false
	}
	// Symbols defined in the session: show their source.
	head, _, _ := strings.Cut(arg, ".")
	for _, d := range s.decls {
		for _, n := range d.names {
			if n == arg || (strings.Contains(n, ".") && n == arg) || (n == head && !strings.Contains(arg, ".")) {
				fmt.Fprintln(s.ui.out, s.ui.code(d.src))
				return false
			}
		}
	}
	args := strings.Fields(arg)
	// Resolve a bare package name to its import path, e.g. rand -> math/rand/v2.
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		pkg, rest, _ := strings.Cut(a, ".")
		for _, imp := range s.imports {
			if imp.binding() == pkg {
				args[i] = imp.path
				if rest != "" {
					args[i] += "." + rest
				}
			}
		}
	}
	cmd := exec.Command("go", append([]string{"doc"}, args...)...)
	cmd.Dir = s.dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()
	page(s.ui, colorDoc(s.ui, out.String()))
	return false
}

// colorDoc highlights declarations in `go doc` output (lines that are not
// indented prose).
func colorDoc(u *ui, doc string) string {
	if !u.color {
		return doc
	}
	lines := strings.Split(doc, "\n")
	inBlock := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "    ") || t == "":
			if strings.HasPrefix(t, "//") {
				lines[i] = u.style(sGray, l)
			}
		case strings.HasPrefix(t, "package "):
			lines[i] = highlight(l)
		default:
			lines[i] = highlight(l)
		}
		if strings.HasSuffix(t, "{") {
			inBlock = true
		} else if t == "}" || t == ")" {
			inBlock = false
		} else if inBlock && strings.HasPrefix(l, "\t") {
			lines[i] = highlight(l)
		}
	}
	return strings.Join(lines, "\n")
}

// page prints text, through a pager when it does not fit the terminal.
func page(u *ui, text string) {
	_, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err == nil && strings.Count(text, "\n") > height-2 {
		pager := os.Getenv("PAGER")
		if pager == "" {
			pager = "less"
		}
		args := []string{}
		if filepath.Base(pager) == "less" {
			args = append(args, "-RFX")
		}
		cmd := exec.Command(pager, args...)
		cmd.Stdin = strings.NewReader(text)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if cmd.Run() == nil {
			return
		}
	}
	fmt.Fprint(u.out, text)
	if !strings.HasSuffix(text, "\n") {
		fmt.Fprintln(u.out)
	}
}

func cmdType(s *Session, _ *repl, arg string) bool {
	if arg == "" {
		s.ui.errorf("usage: :type <expr>")
		return false
	}
	t, err := s.TypeOf(arg)
	if err != nil {
		s.ui.inputError(arg, err)
		return false
	}
	if tv := t; tv.IsType() {
		s.ui.typeInfo(tv.Type)
		return false
	}
	typ := t.Type
	if b, ok := typ.(*types.Basic); ok && t.Value == nil && b.Info()&types.IsUntyped != 0 {
		typ = types.Default(typ) // e.g. a comparison is just a bool
	}
	fmt.Fprintln(s.ui.out, s.ui.code(typeArgsPlain(typ)))
	if named, ok := types.Unalias(t.Type).(*types.Named); ok {
		s.ui.typeInfo(named)
	}
	return false
}

func typeArgsPlain(t types.Type) string {
	if tup, ok := t.(*types.Tuple); ok {
		var parts []string
		for i := 0; i < tup.Len(); i++ {
			parts = append(parts, typeString(tup.At(i).Type()))
		}
		return "(" + strings.Join(parts, ", ") + ")"
	}
	return typeString(t)
}

func cmdVars(s *Session, _ *repl, _ string) bool {
	vars := s.Vars()
	if len(vars) == 0 {
		s.ui.note("no variables yet")
		return false
	}
	var tail strings.Builder
	for _, v := range vars {
		fmt.Fprintf(&tail, "__gop.T(%q).Var(%q, %s)\n", v.typ, v.name, v.name)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prog := generate(genInput{decls: s.decls, replay: s.stmts, imports: s.usedImports(), tail: tail.String(), unmute: true})
	s.buildAndRun(ctx, prog.src, s.cellByID(nil, nil))
	return false
}

func cmdImports(s *Session, _ *repl, _ string) bool {
	if len(s.imports) == 0 {
		s.ui.note("no imports yet — packages are imported automatically when you use them")
		return false
	}
	list := append([]importSpec(nil), s.imports...)
	sort.Slice(list, func(i, j int) bool { return list[i].path < list[j].path })
	for _, imp := range list {
		if imp.name != "" {
			fmt.Fprintf(s.ui.out, "%s %q\n", imp.name, imp.path)
		} else {
			fmt.Fprintf(s.ui.out, "%q\n", imp.path)
		}
	}
	return false
}

func cmdCells(s *Session, _ *repl, _ string) bool {
	all := s.cells()
	if len(all) == 0 {
		s.ui.note("the session is empty")
		return false
	}
	for _, c := range all {
		text := strings.TrimSpace(c.raw)
		first, _, multi := strings.Cut(text, "\n")
		if multi {
			first += s.ui.style(sGray, fmt.Sprintf(" … (+%d lines)", strings.Count(text, "\n")))
		}
		fmt.Fprintf(s.ui.out, "%s %s\n", s.ui.style(sGray, fmt.Sprintf("[%d]", c.id)), s.ui.code(first))
	}
	return false
}

func cmdDrop(s *Session, _ *repl, arg string) bool {
	if arg == "" {
		s.ui.errorf("usage: :drop <cell>...   (see :cells)")
		return false
	}
	for _, f := range strings.Fields(arg) {
		id, err := strconv.Atoi(strings.Trim(f, "[]"))
		if err != nil || !s.drop(id) {
			s.ui.errorf("no cell %s", f)
			continue
		}
		s.ui.note("dropped [%d]", id)
	}
	return false
}

func cmdUndo(s *Session, _ *repl, _ string) bool {
	all := s.cells()
	if len(all) == 0 {
		s.ui.note("nothing to undo")
		return false
	}
	last := all[len(all)-1]
	s.drop(last.id)
	s.ui.note("removed [%d] %s", last.id, firstLine(last.raw))
	return false
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

func cmdShow(s *Session, _ *repl, _ string) bool {
	page(s.ui, s.ui.code(s.exportSource()))
	return false
}

func cmdSave(s *Session, _ *repl, arg string) bool {
	if arg == "" {
		arg = "main.go"
	}
	path := expandHome(arg)
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.cwd, path)
	}
	if _, err := os.Stat(path); err == nil && !strings.HasSuffix(arg, "!") {
		s.ui.errorf("%s exists; use :save %s! to overwrite", arg, arg)
		return false
	}
	path = strings.TrimSuffix(path, "!")
	if err := os.WriteFile(path, []byte(s.exportSource()), 0o644); err != nil {
		s.ui.errorf("%v", err)
		return false
	}
	s.ui.note("saved %s", path)
	return false
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func cmdLoad(s *Session, _ *repl, arg string) bool {
	if arg == "" {
		s.ui.errorf("usage: :load <file.go>")
		return false
	}
	path := expandHome(arg)
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.cwd, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		s.ui.errorf("%v", err)
		return false
	}
	evalInterruptible(s, string(data))
	return false
}

func cmdEdit(s *Session, _ *repl, _ string) bool {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	f, err := os.CreateTemp("", "gotinker-*.go")
	if err != nil {
		s.ui.errorf("%v", err)
		return false
	}
	defer os.Remove(f.Name())
	before := s.exportSource()
	f.WriteString(before)
	f.Close()

	parts := strings.Fields(editor)
	cmd := exec.Command(parts[0], append(parts[1:], f.Name())...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		s.ui.errorf("%s: %v", editor, err)
		return false
	}
	after, err := os.ReadFile(f.Name())
	if err != nil || string(after) == before {
		s.ui.note("no changes")
		return false
	}
	saved := s.snapshot()
	s.reset()
	evalInterruptible(s, string(after))
	if len(s.cells()) == 0 {
		s.restore(saved)
		s.ui.note("edit failed; the previous session was kept")
	}
	return false
}

func cmdGet(s *Session, _ *repl, arg string) bool {
	if arg == "" {
		s.ui.errorf("usage: :get <module>[@version]")
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.goGet(ctx, arg); err != nil {
		s.ui.errorf("%v", err)
		return false
	}
	s.ui.note("added %s — import it with: import %q", arg, strings.SplitN(arg, "@", 2)[0])
	return false
}

func cmdReset(s *Session, r *repl, _ string) bool {
	s.reset()
	if r != nil {
		r.comp.invalidate()
	}
	s.ui.note("session cleared")
	return false
}

func cmdClear(s *Session, _ *repl, _ string) bool {
	fmt.Fprint(s.ui.out, "\x1b[H\x1b[2J\x1b[3J")
	return false
}
