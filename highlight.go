package main

import (
	"go/scanner"
	"go/token"
	"strings"
)

// ANSI styles shared by the highlighter and output.
const (
	sReset   = "\x1b[0m"
	sBold    = "\x1b[1m"
	sDim     = "\x1b[2m"
	sItalic  = "\x1b[3m"
	sRed     = "\x1b[31m"
	sGreen   = "\x1b[32m"
	sYellow  = "\x1b[33m"
	sBlue    = "\x1b[34m"
	sMagenta = "\x1b[35m"
	sCyan    = "\x1b[36m"
	sGray    = "\x1b[90m"
	sOrange  = "\x1b[38;5;208m"
)

var builtins = map[string]bool{
	"append": true, "cap": true, "clear": true, "close": true, "complex": true, "copy": true,
	"delete": true, "imag": true, "len": true, "make": true, "max": true, "min": true, "new": true,
	"panic": true, "print": true, "println": true, "real": true, "recover": true,
}

var basicTypes = map[string]bool{
	"any": true, "bool": true, "byte": true, "comparable": true, "complex64": true, "complex128": true,
	"error": true, "float32": true, "float64": true, "int": true, "int8": true, "int16": true,
	"int32": true, "int64": true, "rune": true, "string": true, "uint": true, "uint8": true,
	"uint16": true, "uint32": true, "uint64": true, "uintptr": true,
}

var constants = map[string]bool{"true": true, "false": true, "nil": true, "iota": true}

// highlight colours Go source for the terminal while keeping every byte of
// the input (including whitespace and invalid text) in place.
func highlight(src string) string {
	if strings.HasPrefix(strings.TrimSpace(src), ":") {
		return highlightCommand(src)
	}
	var s scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	s.Init(file, []byte(src), func(token.Position, string) {}, scanner.ScanComments)

	var b strings.Builder
	last := 0
	prevTok := token.ILLEGAL
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		off := file.Offset(pos)
		if off < last || off > len(src) {
			continue
		}
		// Automatic semicolons have no text in the source.
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		text := lit
		if text == "" || tok.IsOperator() {
			text = tok.String()
		}
		end := off + len(text)
		if end > len(src) {
			end = len(src)
		}
		b.WriteString(src[last:off])
		text = src[off:end]
		style := ""
		switch {
		case tok == token.COMMENT:
			style = sGray + sItalic
		case tok.IsKeyword():
			style = sMagenta
		case tok == token.STRING || tok == token.CHAR:
			style = sGreen
		case tok == token.INT || tok == token.FLOAT || tok == token.IMAG:
			style = sCyan
		case tok == token.IDENT:
			switch {
			case constants[text]:
				style = sOrange
			case basicTypes[text]:
				style = sYellow
			case builtins[text] && prevTok != token.PERIOD:
				style = sBlue
			case prevTok == token.PERIOD:
				style = ""
			}
		}
		if style != "" {
			b.WriteString(style + text + sReset)
		} else {
			b.WriteString(text)
		}
		last = end
		prevTok = tok
	}
	b.WriteString(src[last:])
	return b.String()
}

func highlightCommand(src string) string {
	i := strings.IndexAny(strings.TrimLeft(src, " "), " \t")
	lead := len(src) - len(strings.TrimLeft(src, " "))
	if i < 0 {
		return sBold + sCyan + src + sReset
	}
	return sBold + sCyan + src[:lead+i] + sReset + src[lead+i:]
}
