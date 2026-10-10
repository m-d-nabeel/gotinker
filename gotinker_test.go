package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-d-nabeel/gotinker/third_party/readline/inputrc"
)

func TestInputComplete(t *testing.T) {
	cases := map[string]bool{
		"x := 1":                     true,
		"for i := range 3 {":         false,
		"for i := range 3 {\n}":      true,
		"fmt.Println(1,":             false,
		"x := 1 +":                   false,
		"s := `raw":                  false,
		"s := `raw\nmore`":           true,
		"x++":                        true,
		":help":                      true,
		"f(\n":                       false,
		"f(\n\n\n":                   true, // two empty lines force evaluation
		"f(\n\t\n\t":                 true, // ... even when auto-indented
		"f(\n\t":                     false,
		"func f() {\n\tx := 1\n\n\n": false, // blank lines inside a block are fine
		"m := map[string]int{":       false,
		"// a comment {":             true,
		"x := \"{\"":                 true,
		"return a.":                  false,
		"if x {\n} else {\n":         false,
		"type T struct {\nA int\n}":  true,
	}
	for src, want := range cases {
		if got := inputComplete(src); got != want {
			t.Errorf("inputComplete(%q) = %v, want %v", src, got, want)
		}
	}
}

func TestChainAt(t *testing.T) {
	cases := []struct {
		line, base, partial string
	}{
		{"strings.To", "strings", "To"},
		{"fmt.Println(strings.", "strings", ""},
		{"xs[0].Name.Fi", "xs[0].Name", "Fi"},
		{"f(1, g(2)).Me", "f(1, g(2))", "Me"},
		{"foo", "", "foo"},
		{"x := ba", "", "ba"},
	}
	for _, c := range cases {
		r := []rune(c.line)
		_, base, partial := chainAt(r, len(r))
		if base != c.base || partial != c.partial {
			t.Errorf("chainAt(%q) = %q, %q; want %q, %q", c.line, base, partial, c.base, c.partial)
		}
	}
}

func TestEnclosingCall(t *testing.T) {
	cases := []struct {
		line, callee string
		arg          int
	}{
		{`strings.Cut("a,b", `, "strings.Cut", 1},
		{`fmt.Println(`, "fmt.Println", 0},
		{`f(g(1), h(`, "h", 0},
		{`f(g(1), "x,y", `, "f", 2},
		{`x := []int{1, `, "", 0},
		{`if (`, "", 0},
	}
	for _, c := range cases {
		callee, _, arg := enclosingCall([]rune(c.line))
		if callee != c.callee || (callee != "" && arg != c.arg) {
			t.Errorf("enclosingCall(%q) = %q, %d; want %q, %d", c.line, callee, arg, c.callee, c.arg)
		}
	}
}

func TestPlanStmtsGuards(t *testing.T) {
	p, err := planStmts(3, "a, b := 1, 2; c := a\nvar d, _ int")
	if err != nil {
		t.Fatal(err)
	}
	got := applyEdits(p.src, p.guards)
	for _, want := range []string{"_, _ = a, b", "_ = c", "_ = d"} {
		if !strings.Contains(got, want) {
			t.Errorf("guards missing %q in:\n%s", want, got)
		}
	}
	if p.pure {
		t.Error("definitions are not pure")
	}
	if q, _ := planStmts(4, "x * 2"); !q.pure {
		t.Error("x * 2 should be pure")
	}
	if q, _ := planStmts(5, "f(x)"); q.pure {
		t.Error("a call is not pure")
	}
}

func TestDedent(t *testing.T) {
	got := dedent("\n\tx := 1\n\tif x > 0 {\n\t\tx++\n\t}\n")
	want := "x := 1\nif x > 0 {\n\tx++\n}"
	if got != want {
		t.Errorf("dedent = %q, want %q", got, want)
	}
}

func TestHighlightKeepsText(t *testing.T) {
	for _, src := range []string{
		"x := map[string]int{\"a\": 1} // note",
		"s := `unterminated",
		"for i := range 10 { fmt.Println(i, len(\"x\"), nil) }",
		"€ weird ✓ input",
	} {
		if got := stripANSI(highlight(src)); got != src {
			t.Errorf("highlight changed text:\n got %q\nwant %q", got, src)
		}
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// newTestSession returns a session whose output is captured.
func newTestSession(t *testing.T) (*Session, *bytes.Buffer) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	var out bytes.Buffer
	u := &ui{out: &out, err: &out}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	s, err := newSession(u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, &out
}

func TestSessionEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles programs")
	}
	s, out := newTestSession(t)
	steps := []struct{ in, want string }{
		{"x := []int{3, 1, 2}", "x = []int{3, 1, 2}"},
		{"slices.Sort(x); x", "[]int{1, 2, 3}"},
		{"for i := range 2 { fmt.Println(\"i\", i) }", "i 1"},
		{"x := \"shadowed\"", `x = "shadowed"`},
		{"func sq(n int) int { return n * n }", "func sq(n int) int"},
		{"sq(len(x))", "64  int"},
		{"1 << 70", "1180591620717411303424"},
		{"time.Duration(90) * time.Second", "1m30s"},
		{"y", "undefined: y"},
		{"var z = sq(2)", "z = 4"},
		{"panic(\"boom\")", "panic: boom"},
		{"z + 1", "5  int"}, // the panic was discarded; the session still works
		// A pasted type, method and code using them.
		{"type T struct{ N int }\nfunc (t T) Double() int { return t.N * 2 }\nT{N: 21}.Double()", "42  int"},
		{"func (t T) Triple() int { return t.N * 3 }", "func (T) Triple() int"},
	}
	for _, st := range steps {
		out.Reset()
		s.Eval(context.Background(), st.in)
		if !strings.Contains(out.String(), st.want) {
			t.Errorf("%s\n got: %s\nwant substring: %s", st.in, out.String(), st.want)
		}
	}

	// The exported program must compile on its own.
	dir := t.TempDir()
	src := s.exportSource()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo "+s.goVer+"\n"), 0o644)
	cmd := exec.Command("go", "build", "-o", os.DevNull, ".")
	cmd.Dir = dir
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("exported program does not build: %v\n%s\n%s", err, msg, src)
	}
}

func TestNewlineText(t *testing.T) {
	cases := []struct{ before, want string }{
		{"x := 1", "\n"},
		{"func f() {", "\n\t"},
		{"\tif x {", "\n\t\t"},
		{"\tfmt.Println(", "\n\t\t"},
		{"switch x {", "\n"},          // case clauses sit at the switch's level
		{"case 1:", "\n\t"},           // ... and their bodies one level in
		{"\tfoo() // call {", "\n\t"}, // comments are ignored
	}
	for _, c := range cases {
		line := []rune("prev\n" + c.before)
		if got := newlineText(line, len(line)); got != c.want {
			t.Errorf("newlineText(%q) = %q, want %q", c.before, got, c.want)
		}
	}
}

func TestWordMotion(t *testing.T) {
	line := []rune("fmt.Println(strings.ToUpper(name), x+1)")
	var stops []string
	for pos := len(line); pos > 0; {
		pos = backwardWord(line, pos)
		stops = append(stops, string(line[pos:forwardWord(line, pos)]))
	}
	want := "1 x name ToUpper strings Println fmt"
	if got := strings.Join(stops, " "); got != want {
		t.Errorf("backward word stops = %q, want %q", got, want)
	}
	if got := forwardWord(line, 0); got != 3 {
		t.Errorf("forwardWord from 0 = %d, want 3 (end of fmt)", got)
	}
}

func TestTopLevelUnits(t *testing.T) {
	src := "type T struct{ N int }\nfunc (t T) Double() int {\n\treturn t.N * 2\n}\nx := T{N: 1}; x.Double()"
	got := topLevelUnits(src)
	want := []string{"type T struct{ N int }", "func (t T) Double() int {\n\treturn t.N * 2\n}", "x := T{N: 1}", "x.Double()"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("topLevelUnits = %q\nwant %q", got, want)
	}
}

func TestBindKeyReplacesMetaForm(t *testing.T) {
	cfg := inputrc.NewDefaultConfig()
	up := inputrc.Unescape(`\M-[A`) // stored in meta form by default
	cfg.Binds["emacs"][up] = inputrc.Bind{Action: "previous-history"}
	bindKey(cfg, "emacs", "\x1b[A", "up-line-or-history")
	if _, ok := cfg.Binds["emacs"][up]; ok {
		t.Error("meta-form bind for the same keys was not removed")
	}
	if got := cfg.Binds["emacs"]["\x1b[A"].Action; got != "up-line-or-history" {
		t.Errorf("bind = %q", got)
	}
}
