package main

import (
	"go/scanner"
	"go/token"
	"strings"
	"unicode"

	"github.com/m-d-nabeel/gotinker/third_party/readline/inputrc"
)

// Line-editing behaviour for Go code, layered on readline: auto-indent,
// Tab that indents at the start of a line, closing brackets that dedent,
// Go-aware word motion, and multi-line navigation with the arrow keys.

// editKeymaps are the keymaps gotinker's bindings apply to: emacs (the
// default) and vi insert mode.
var editKeymaps = []string{"emacs", "emacs-standard", "vi-insert"}

func (r *repl) installEditing() {
	rl := r.rl
	rl.NewlineText = newlineInsert

	// Enter inserts a newline instead of running while the cursor is not on
	// the last line (so lines can be added inside a finished block), or sits
	// just after an opening bracket (so `{|}` opens the block for its body).
	rl.AcceptMultiline = func(line []rune) bool {
		pos := rl.Cursor().Pos()
		if pos < len(line) && strings.ContainsRune(string(line[pos:]), '\n') {
			return false
		}
		if afterOpener(line, pos) {
			// Spaces between the cursor and the rest of the line would end
			// up leading the next line; drop them.
			end := pos
			for end < len(line) && (line[end] == ' ' || line[end] == '\t') {
				end++
			}
			if end > pos {
				rl.Line().Cut(pos, end)
			}
			return false
		}
		return inputComplete(string(line))
	}

	commands := rl.Keymap.Commands()
	complete := commands["complete"]
	accept := commands["accept-line"]

	rl.Keymap.Register(map[string]func(){
		"gotinker-tab": func() {
			if onlySpaceBefore(*rl.Line(), rl.Cursor().Pos()) {
				r.insert("\t")
				return
			}
			if complete != nil {
				complete()
			}
		},
		"gotinker-newline": func() {
			text, at := newlineInsert(*rl.Line(), rl.Cursor().Pos())
			r.insert(text)
			rl.Cursor().Move(at - len([]rune(text)))
		},
		"gotinker-run": func() {
			rl.Cursor().Set(len(*rl.Line()))
			if accept != nil {
				accept()
			}
		},
		"gotinker-colon": func() {
			r.alignCase()
			r.insert(":")
		},
		"gotinker-close": func() {
			keys := rl.Keys.Caller()
			if len(keys) == 0 {
				return
			}
			r.dedent()
			r.insert(string(keys[0]))
		},
		"gotinker-backward-word": func() {
			rl.Cursor().Set(backwardWord(*rl.Line(), rl.Cursor().Pos()))
		},
		"gotinker-forward-word": func() {
			rl.Cursor().Set(forwardWord(*rl.Line(), rl.Cursor().Pos()))
		},
		"gotinker-backward-kill-word": func() {
			pos := rl.Cursor().Pos()
			r.kill(backwardWord(*rl.Line(), pos), pos)
		},
		"gotinker-kill-word": func() {
			pos := rl.Cursor().Pos()
			r.kill(pos, forwardWord(*rl.Line(), pos))
		},
	})

	binds := map[string][]string{
		// Arrows move between the lines of a multi-line input and only
		// reach into history from the first/last line (as Ctrl-P/N do).
		"up-line-or-history":   {"\x1b[A", "\x1bOA"},
		"down-line-or-history": {"\x1b[B", "\x1bOB"},
		"gotinker-tab":         {"\t"},
		// Alt-Enter (ESC CR / ESC LF) always inserts a newline.
		"gotinker-newline": {"\x1b\r", "\x1b\n"},
		// Ctrl-J runs the input from anywhere in it.
		"gotinker-run":   {"\n"},
		"gotinker-close": {"}", ")", "]"},
		"gotinker-colon": {":"},
		// Alt/Ctrl+arrows (xterm, kitty, ...), Esc-prefixed Alt+arrows
		// (terminals where Alt sends ESC), and Alt-b / Alt-f.
		"gotinker-backward-word":      {"\x1b[1;3D", "\x1b[1;5D", "\x1b[1;9D", "\x1b\x1b[D", "\x1b\x1bOD", "\x1bb"},
		"gotinker-forward-word":       {"\x1b[1;3C", "\x1b[1;5C", "\x1b[1;9C", "\x1b\x1b[C", "\x1b\x1bOC", "\x1bf"},
		"gotinker-backward-kill-word": {"\x1b\x7f", "\x1b\b", "\x1b[3;5~"},
		"gotinker-kill-word":          {"\x1bd", "\x1b[3;3~"},
	}
	for action, seqs := range binds {
		for _, seq := range seqs {
			for _, km := range editKeymaps {
				bindKey(rl.Config, km, seq, action)
			}
		}
	}
}

// bindKey binds seq in keymap, replacing any existing bind for the same
// keys. readline stores some default binds in "meta" form (\M-[A), which
// it prefers over an equivalent ESC form, so those must be removed.
func bindKey(cfg *inputrc.Config, keymap, seq, action string) {
	binds := cfg.Binds[keymap]
	if binds == nil {
		binds = map[string]inputrc.Bind{}
		cfg.Binds[keymap] = binds
	}
	for existing := range binds {
		if demeta(existing) == seq {
			delete(binds, existing)
		}
	}
	binds[seq] = inputrc.Bind{Action: action}
}

func demeta(seq string) string {
	var b strings.Builder
	for _, r := range seq {
		if inputrc.IsMeta(r) {
			b.WriteRune(inputrc.Esc)
			b.WriteRune(inputrc.Demeta(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// insert inserts text at the cursor as one undoable edit.
func (r *repl) insert(text string) {
	r.rl.History.Save()
	runes := []rune(text)
	r.rl.Line().Insert(r.rl.Cursor().Pos(), runes...)
	r.rl.Cursor().Move(len(runes))
}

// kill deletes line[from:to], keeping it for Ctrl-Y.
func (r *repl) kill(from, to int) {
	if from >= to {
		return
	}
	r.rl.History.Save()
	line := r.rl.Line()
	killed := append([]rune(nil), (*line)[from:to]...)
	line.Cut(from, to)
	r.rl.Cursor().Set(from)
	r.rl.Buffers.Write(killed...)
}

// dedent removes one level of indentation when the cursor follows only
// whitespace on its line (so a closing bracket lines up with its opener).
func (r *repl) dedent() {
	line := *r.rl.Line()
	pos := r.rl.Cursor().Pos()
	if !onlySpaceBefore(line, pos) || pos == 0 {
		return
	}
	n := 0
	switch {
	case line[pos-1] == '\t':
		n = 1
	case line[pos-1] == ' ':
		for n < 4 && pos-n > 0 && line[pos-n-1] == ' ' {
			n++
		}
	}
	if n > 0 {
		r.rl.History.Save()
		r.rl.Line().Cut(pos-n, pos)
		r.rl.Cursor().Set(pos - n)
	}
}

// alignCase moves a `case ...` / `default` line being finished with ':'
// back to the indentation of its switch or select, as gofmt would.
func (r *repl) alignCase() {
	line := *r.rl.Line()
	pos := r.rl.Cursor().Pos()
	start := lineStart(line, pos)
	cur := string(line[start:pos])
	trimmed := strings.TrimLeft(cur, " \t")
	if !strings.HasPrefix(trimmed, "case ") && trimmed != "default" {
		return
	}
	indent := cur[:len(cur)-len(trimmed)]
	// Find the innermost enclosing switch/select header above.
	lines := strings.Split(string(line[:start]), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		t := strings.TrimLeft(l, " \t")
		ind := l[:len(l)-len(t)]
		if (strings.HasPrefix(t, "switch") || strings.HasPrefix(t, "select")) && strings.HasSuffix(strings.TrimSpace(t), "{") && len(ind) <= len(indent) {
			if ind != indent {
				r.rl.History.Save()
				r.rl.Line().Cut(start, start+len([]rune(indent)))
				r.rl.Line().Insert(start, []rune(ind)...)
				r.rl.Cursor().Set(pos - len([]rune(indent)) + len([]rune(ind)))
			}
			return
		}
	}
}

// lineStart returns the index where the line containing pos begins.
func lineStart(line []rune, pos int) int {
	for i := pos - 1; i >= 0; i-- {
		if line[i] == '\n' {
			return i + 1
		}
	}
	return 0
}

func onlySpaceBefore(line []rune, pos int) bool {
	for _, r := range line[lineStart(line, pos):pos] {
		if r != ' ' && r != '\t' {
			return false
		}
	}
	return true
}

// afterOpener reports whether the cursor follows an opening bracket on its
// line (ignoring spaces) and is not at the end of the input.
func afterOpener(line []rune, pos int) bool {
	if strings.TrimSpace(string(line[pos:])) == "" {
		return false // at the end: the usual completeness rules apply
	}
	i := pos
	for i > lineStart(line, pos) && (line[i-1] == ' ' || line[i-1] == '\t') {
		i--
	}
	return i > 0 && strings.ContainsRune("{([", line[i-1])
}

// newlineInsert returns the text for a line break at cursor and where the
// cursor lands in it. Between a bracket pair (`{|}`) the closer moves to its
// own line at the opener's indentation, and the cursor sits on an indented
// line in between.
func newlineInsert(line []rune, cursor int) (string, int) {
	text := newlineText(line, cursor)
	at := len([]rune(text))
	rest := strings.TrimLeft(string(line[cursor:]), " \t")
	if afterOpener(line, cursor) && rest != "" && strings.ContainsRune(")]}", []rune(rest)[0]) {
		cur := string(line[lineStart(line, cursor):cursor])
		base := cur[:len(cur)-len(strings.TrimLeft(cur, " \t"))]
		text += "\n" + base
	}
	return text, at
}

// newlineText returns the newline plus indentation for a line break at
// cursor: the current line's indentation, one level deeper after an
// opening bracket or a case clause.
func newlineText(line []rune, cursor int) string {
	cur := string(line[lineStart(line, cursor):cursor])
	indent := cur[:len(cur)-len(strings.TrimLeft(cur, " \t"))]
	trimmed := strings.TrimSpace(cur)
	switch lastToken(cur) {
	case token.LBRACE:
		// gofmt puts case clauses at the level of their switch/select.
		if !strings.HasPrefix(trimmed, "switch") && !strings.HasPrefix(trimmed, "select") {
			indent += "\t"
		}
	case token.LPAREN, token.LBRACK:
		indent += "\t"
	case token.COLON:
		if strings.HasPrefix(trimmed, "case ") || strings.HasPrefix(trimmed, "default") {
			indent += "\t"
		}
	}
	return "\n" + indent
}

// lastToken returns the last token of a line of Go code, ignoring comments.
func lastToken(src string) token.Token {
	var s scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	s.Init(file, []byte(src), func(token.Position, string) {}, 0)
	last := token.ILLEGAL
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return last
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		last = tok
	}
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// backwardWord returns the start of the identifier before pos, skipping
// punctuation and whitespace (so `)`, `,` and `.` are never stops).
func backwardWord(line []rune, pos int) int {
	i := pos
	for i > 0 && !isWordRune(line[i-1]) {
		i--
	}
	for i > 0 && isWordRune(line[i-1]) {
		i--
	}
	return i
}

// forwardWord returns the end of the identifier after pos.
func forwardWord(line []rune, pos int) int {
	i := pos
	for i < len(line) && !isWordRune(line[i]) {
		i++
	}
	for i < len(line) && isWordRune(line[i]) {
		i++
	}
	return i
}
