package main

import (
	"bufio"
	"fmt"
	"go/constant"
	"go/token"
	"go/types"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// ui renders everything gop itself prints (results come from goprt).
type ui struct {
	color bool
	out   io.Writer
	err   io.Writer
	fset  *token.FileSet
}

func newUI() *ui {
	color := term.IsTerminal(int(os.Stdout.Fd())) && os.Getenv("NO_COLOR") == ""
	return &ui{color: color, out: os.Stdout, err: os.Stderr}
}

func (u *ui) style(style, s string) string {
	if !u.color || s == "" {
		return s
	}
	return style + s + sReset
}

func (u *ui) code(s string) string {
	if !u.color {
		return s
	}
	return highlight(s)
}

func (u *ui) note(format string, args ...any) {
	fmt.Fprintln(u.out, u.style(sGray, "· "+fmt.Sprintf(format, args...)))
}

func (u *ui) errorf(format string, args ...any) {
	fmt.Fprintln(u.err, u.style(sRed+sBold, "error: ")+fmt.Sprintf(format, args...))
}

func (u *ui) declared(s string) {
	fmt.Fprintln(u.out, u.style(sGreen, "✓ ")+u.style(sGray, s))
}

func (u *ui) timing(build, run time.Duration) {
	if build+run < 400*time.Millisecond {
		return
	}
	fmt.Fprintln(u.out, u.style(sGray, fmt.Sprintf("⏱ build %s · run %s", round(build), round(run))))
}

func round(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// sourceError prints msg with the offending line and a caret under col.
func (u *ui) sourceError(prefix, msg string, src string, line, col int) {
	fmt.Fprintln(u.err, u.style(sRed+sBold, "error: ")+prefix+msg)
	ls := strings.Split(src, "\n")
	if line < 1 || line > len(ls) {
		return
	}
	text := strings.ReplaceAll(ls[line-1], "\t", "    ")
	raw := ls[line-1]
	caretCol := col - 1
	if caretCol < 0 {
		caretCol = 0
	}
	if caretCol > len(raw) {
		caretCol = len(raw)
	}
	// Account for tabs expanded above and multi-byte runes.
	pad := len([]rune(strings.ReplaceAll(raw[:caretCol], "\t", "    ")))
	gutter := fmt.Sprintf("%3d │ ", line)
	if len(ls) == 1 {
		gutter = "  │ "
	}
	fmt.Fprintln(u.err, u.style(sGray, gutter)+u.code(text))
	fmt.Fprintln(u.err, u.style(sGray, strings.Repeat(" ", len([]rune(gutter))-2)+"│ ")+strings.Repeat(" ", pad)+u.style(sRed+sBold, "^"))
}

func (u *ui) inputError(src string, err error) {
	if pe, ok := err.(*posError); ok {
		u.sourceError("", pe.msg, src, pe.line, pe.col)
		return
	}
	u.errorf("%v", err)
}

var reCellPos = regexp.MustCompile(`^cell(\d+):(\d+)(?::(\d+))?$`)

// locate maps a position in the generated program back to a cell.
func locate(pos token.Position, lookup func(int) *cell) (c *cell, line, col int) {
	m := reCellPos.FindStringSubmatch(fmt.Sprintf("%s:%d:%d", pos.Filename, pos.Line, pos.Column))
	if m == nil || lookup == nil {
		return nil, 0, 0
	}
	id, _ := strconv.Atoi(m[1])
	line, _ = strconv.Atoi(m[2])
	col, _ = strconv.Atoi(m[3])
	return lookup(id), line, col
}

const maxErrors = 6

func (u *ui) typeErrors(c *checked, lookup func(int) *cell) {
	for i, e := range c.errs {
		if i == maxErrors {
			fmt.Fprintln(u.err, u.style(sGray, fmt.Sprintf("… and %d more errors", len(c.errs)-maxErrors)))
			break
		}
		pos := e.Fset.Position(e.Pos)
		u.cellError(pos, e.Msg, lookup)
	}
}

// newestID is the id of the cell the user just entered; errors elsewhere
// are attributed to earlier cells.
func (u *ui) cellError(pos token.Position, msg string, lookup func(int) *cell) {
	cl, line, col := locate(pos, lookup)
	if cl == nil {
		u.errorf("%s", msg)
		return
	}
	prefix := ""
	if cl.committed {
		prefix = u.style(sYellow, fmt.Sprintf("in earlier cell [%d]: ", cl.id))
	}
	src := cl.input
	if cl.kind == stmtCell || cl.raw != "" {
		src = cl.raw
	}
	u.sourceError(prefix, msg, src, line, col)
}

var reBuildErr = regexp.MustCompile(`^(cell\d+):(\d+):(\d+): (.*)$`)

func (u *ui) buildErrors(out string, lookup func(int) *cell) {
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(l, "#") || l == "" {
			continue
		}
		if m := reBuildErr.FindStringSubmatch(l); m != nil {
			line, _ := strconv.Atoi(m[2])
			col, _ := strconv.Atoi(m[3])
			u.cellError(token.Position{Filename: m[1], Line: line, Column: col}, m[4], lookup)
		} else {
			fmt.Fprintln(u.err, u.style(sRed, l))
		}
		n++
		if n == maxErrors {
			break
		}
	}
}

var (
	reFrameFile = regexp.MustCompile(`^\t(cell(\d+)):(\d+)`)
	reFrameFunc = regexp.MustCompile(`^([^\s(]+)\(`)
)

// programStderr streams the program's stderr, turning Go panic traces into
// short traces that point at cells instead of generated code.
func (u *ui) programStderr(r io.Reader, lookup func(int) *cell) {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	inTrace := false
	fn := ""
	for sc.Scan() {
		l := sc.Text()
		switch {
		case !inTrace && strings.HasPrefix(l, "goroutine ") && strings.HasSuffix(l, ":"):
			inTrace = true
		case !inTrace:
			if strings.HasPrefix(l, "panic: ") || strings.HasPrefix(l, "fatal error: ") {
				l = u.style(sRed+sBold, l)
			}
			fmt.Fprintln(u.err, l)
		case l == "":
			inTrace = false
		case strings.HasPrefix(l, "\t"):
			if m := reFrameFile.FindStringSubmatch(l); m != nil {
				id, _ := strconv.Atoi(m[2])
				where := "cell [" + m[2] + "]"
				if c := lookup(id); c != nil && !c.committed {
					where = "your input"
				}
				fmt.Fprintln(u.err, u.style(sGray, fmt.Sprintf("    at %s (%s, line %s)", strings.TrimPrefix(fn, "main."), where, m[3])))
			}
		default:
			if m := reFrameFunc.FindStringSubmatch(l); m != nil {
				fn = m[1]
			}
		}
	}
}

func (u *ui) typeInfo(t types.Type) {
	named, _ := t.(*types.Named)
	name := types.TypeString(t, qualifier)
	fmt.Fprintln(u.out, u.code("type "+name+" "+types.TypeString(t.Underlying(), qualifier)))
	if named == nil {
		return
	}
	ms := types.NewMethodSet(types.NewPointer(t))
	if ms.Len() == 0 {
		return
	}
	for i := 0; i < ms.Len(); i++ {
		f := ms.At(i).Obj()
		if !f.Exported() && f.Pkg() != nil && f.Pkg().Path() != "main" {
			continue
		}
		sig := strings.TrimPrefix(types.TypeString(f.Type(), qualifier), "func")
		fmt.Fprintln(u.out, u.style(sGray, "  ")+u.code("func "+f.Name()+sig))
	}
}

func (u *ui) constant(v constant.Value, typ string) {
	text := v.ExactString()
	switch v.Kind() {
	case constant.Float:
		if f, _ := constant.Float64Val(v); true {
			text = strconv.FormatFloat(f, 'g', -1, 64)
			if !strings.ContainsAny(text, ".eEn") {
				text += ".0"
			}
		}
	case constant.Int:
		if typ == "rune" || typ == "untyped rune" {
			if r, ok := constant.Int64Val(v); ok {
				text += " " + strconv.QuoteRune(rune(r))
			}
		}
	}
	fmt.Fprintln(u.out, u.code(text)+"  "+u.style(sGray, typ+" constant"))
}
