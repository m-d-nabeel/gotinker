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
	// Backspace inside an empty pair deletes both halves.
	_ = rl.Config.Set("autopairs-delete", true)

	// Enter inserts a newline instead of running while the cursor is not on
	// the last line (so lines can be added inside a finished block), or sits
	// just after an opening bracket (so `{|}` opens the block for its body).
	rl.AcceptMultiline = func(line []rune) bool {
		pos := rl.Cursor().Pos()
		if pos < len(line) && strings.ContainsRune(string(line[pos:]), '\n') {
			return false
		}
		if inRawString(string(line[:pos])) {
			return false // a newline belongs to the raw string
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
		"gotinker-open": func() {
			keys := rl.Keys.Caller()
			if len(keys) == 0 {
				return
			}
			r.openPair(keys[0])
		},
		"gotinker-quote": func() {
			keys := rl.Keys.Caller()
			if len(keys) == 0 {
				return
			}
			r.quote(keys[0])
		},
		"gotinker-close": func() {
			keys := rl.Keys.Caller()
			if len(keys) == 0 {
				return
			}
			if r.stepOver(keys[0]) {
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
		"gotinker-open":  {"{", "(", "["},
		"gotinker-quote": {"\"", "'", "`"},
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
				// In vi insert mode Esc leaves insert mode, so "Esc b" is
				// "normal mode, back a word": keep Alt-letter style binds
				// to emacs. Terminal key sequences (ESC [ / ESC O) are fine.
				if km == "vi-insert" && strings.HasPrefix(seq, "\x1b") &&
					!strings.HasPrefix(seq, "\x1b[") && !strings.HasPrefix(seq, "\x1bO") {
					continue
				}
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

var closerOf = map[rune]rune{'(': ')', '[': ']', '{': '}'}

// pairAllowed reports whether a pair may be auto-inserted at pos: not inside
// a string or comment, and only before the end of the line, a space or a
// closing/separating character (so `(x` does not become `()x`).
func pairAllowed(line []rune, pos int) bool {
	if inStringOrComment(string(line[:pos])) {
		return false
	}
	return pos == len(line) || strings.ContainsRune(" \t\n)]},;:", line[pos])
}

// openPair inserts an opening bracket, with its closer when allowed.
func (r *repl) openPair(open rune) {
	line, pos := *r.rl.Line(), r.rl.Cursor().Pos()
	if pairAllowed(line, pos) {
		r.insert(string(open) + string(closerOf[open]))
		r.rl.Cursor().Dec()
		return
	}
	r.insert(string(open))
}

// quote types a quote character: stepping over an auto-inserted closing
// quote, inserting a pair outside strings, or a single quote otherwise.
func (r *repl) quote(q rune) {
	line, pos := *r.rl.Line(), r.rl.Cursor().Pos()
	before := string(line[:pos])
	if pos < len(line) && line[pos] == q && inStringOrComment(before) {
		r.rl.Cursor().Inc() // closing the literal we are in
		return
	}
	prevIdent := pos > 0 && isWordRune(line[pos-1])
	if !inStringOrComment(before) && !prevIdent && pairAllowed(line, pos) {
		r.insert(string(q) + string(q))
		r.rl.Cursor().Dec()
		return
	}
	r.insert(string(q))
}

// stepOver moves past a closer that is already there instead of typing a
// second one: right after the cursor, or (when the cursor is on an empty
// line) at the start of the next line, which happens after Enter opened an
// auto-closed block and the user types the closing brace out of habit. The
// empty line is removed in that case.
func (r *repl) stepOver(closer rune) bool {
	line, pos := *r.rl.Line(), r.rl.Cursor().Pos()
	if inStringOrComment(string(line[:pos])) {
		return false
	}
	if pos < len(line) && line[pos] == closer {
		r.rl.Cursor().Inc()
		return true
	}
	ls := lineStart(line, pos)
	if ls == 0 || !onlySpaceBefore(line, pos) {
		return false
	}
	// Skip this and any further blank lines; the next non-blank character
	// must be the closer, at the start of its line.
	j := pos
	lastNL := -1
	for j < len(line) && (line[j] == ' ' || line[j] == '\t' || line[j] == '\n') {
		if line[j] == '\n' {
			lastNL = j
		}
		j++
	}
	if lastNL < 0 || j >= len(line) || line[j] != closer {
		return false
	}
	// Remove the blank lines: from the newline before the cursor's line up
	// to (not including) the newline before the closer's line.
	r.rl.History.Save()
	r.rl.Line().Cut(ls-1, lastNL)
	r.rl.Cursor().Set(j - (lastNL - (ls - 1)) + 1)
	return true
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

// inRawString reports whether the end of src is inside a raw (backquoted)
// string literal.
func inRawString(src string) bool {
	var s scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	open := false
	s.Init(file, []byte(src), func(_ token.Position, msg string) {
		if msg == "raw string literal not terminated" {
			open = true
		}
	}, 0)
	for {
		if _, tok, _ := s.Scan(); tok == token.EOF {
			return open
		}
	}
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
	if inRawString(string(line[:cursor])) {
		return "\n", 1 // inside a raw string, indentation would be content
	}
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
