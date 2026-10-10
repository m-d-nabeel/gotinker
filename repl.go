package main

import (
	"errors"
	"fmt"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/m-d-nabeel/gotinker/third_party/readline"
	"github.com/m-d-nabeel/gotinker/third_party/readline/inputrc"
)

type repl struct {
	s    *Session
	rl   *readline.Shell
	comp *completer
}

func newREPL(s *Session) *repl {
	r := &repl{s: s, rl: readline.NewShell(inputrc.WithApp("gotinker"))}
	r.comp = newCompleter(s)

	cfg := r.rl.Config
	_ = cfg.Set("history-autosuggest", true)
	_ = cfg.Set("colored-completion-prefix", true)
	_ = cfg.Set("completion-ignore-case", true)
	_ = cfg.Set("menu-complete-display-prefix", true)
	_ = cfg.Set("bell-style", "none")
	// The prompt always starts at column 0, so the width-based position is
	// exact. Probing the terminal (ESC[6n) misrenders when replies interleave
	// with typed keys. (Menu sizing without the probe is patched in
	// third_party/readline.)
	_ = cfg.Set("cursor-position-probe", false)

	r.rl.Prompt.Primary(func() string {
		if !s.ui.color {
			return "go> "
		}
		return sBold + sCyan + "go" + sReset + sGray + " ❯ " + sReset
	})
	r.rl.Prompt.Secondary(func() string {
		if !s.ui.color {
			return "... "
		}
		return sGray + " ·· " + sReset
	})
	if s.ui.color {
		r.rl.SyntaxHighlighter = func(line []rune) string { return highlight(string(line)) }
	}
	r.rl.Completer = r.comp.complete
	r.installEditing()
	r.rl.Hint.SetProvider(r.comp.hint)

	if dir, err := dataDir(); err == nil {
		r.rl.History.AddFromFile("history", filepath.Join(dir, "history"))
	}
	return r
}

func dataDir() (string, error) {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "share")
	}
	dir = filepath.Join(dir, "gotinker")
	return dir, os.MkdirAll(dir, 0o755)
}

func (r *repl) run() {
	u := r.s.ui
	fmt.Fprintf(u.out, "%s %s %s\n",
		u.style(sBold+sCyan, "gotinker "+version),
		u.style(sGray, "· Go "+r.s.goVer+" ·"),
		u.style(sGray, "real compiler, real Go — type :help for commands, Tab to complete, Ctrl-D to exit"))
	for {
		// Reserve a row below the prompt. readline moves the cursor down and
		// back up around its hint area; on the terminal's last row the move
		// down cannot scroll, so the prompt would be redrawn over the line
		// above. A newline scrolls if needed; then return to the row.
		fmt.Fprint(u.out, "\n\x1b[1A")
		line, err := r.rl.Readline()
		if errors.Is(err, readline.ErrInterrupt) {
			continue
		}
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(u.out)
			return
		}
		if err != nil {
			u.errorf("%v", err)
			return
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, ":") {
			if runCommand(r.s, r, trimmed) {
				return
			}
			continue
		}
		evalInterruptible(r.s, line)
		r.comp.invalidate()
	}
}

// inputComplete reports whether src is ready to evaluate, or whether the
// editor should continue on a new line (open brackets, a trailing operator,
// an unterminated raw string). Outside an open { block, two empty lines
// force evaluation so a stray ( or operator can't trap the user in
// continuation mode; inside a block, blank lines are just blank lines.
func inputComplete(src string) bool {
	t := strings.TrimSpace(src)
	if t == "" || strings.HasPrefix(t, ":") {
		return true
	}
	var s scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	unterminated := false
	s.Init(file, []byte(src), func(_ token.Position, msg string) {
		if strings.Contains(msg, "not terminated") {
			unterminated = true
		}
	}, scanner.ScanComments)
	var open []token.Token
	last := token.ILLEGAL
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		switch tok {
		case token.LPAREN, token.LBRACE, token.LBRACK:
			open = append(open, tok)
		case token.RPAREN, token.RBRACE, token.RBRACK:
			if len(open) > 0 {
				open = open[:len(open)-1]
			}
		case token.COMMENT:
			continue
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		last = tok
	}
	inBlock := false
	for _, tok := range open {
		if tok == token.LBRACE {
			inBlock = true
		}
	}
	if !inBlock && !unterminated && blankTail(src) {
		return true
	}
	if unterminated || len(open) > 0 {
		return false
	}
	switch last {
	case token.ADD, token.SUB, token.MUL, token.QUO, token.REM, token.AND, token.OR, token.XOR,
		token.SHL, token.SHR, token.AND_NOT, token.LAND, token.LOR, token.ARROW, token.EQL, token.LSS,
		token.GTR, token.NEQ, token.LEQ, token.GEQ, token.ASSIGN, token.DEFINE, token.COMMA, token.PERIOD,
		token.ADD_ASSIGN, token.SUB_ASSIGN, token.MUL_ASSIGN, token.QUO_ASSIGN, token.COLON:
		return false
	}
	return true
}

// blankTail reports whether the last two lines of src are empty (or hold
// only the indentation gotinker inserted).
func blankTail(src string) bool {
	lines := strings.Split(src, "\n")
	n := len(lines)
	return n >= 3 && strings.TrimSpace(lines[n-1]) == "" && strings.TrimSpace(lines[n-2]) == ""
}
