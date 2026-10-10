package goprt

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type linked struct {
	Val  int
	next *linked
	tags map[string]bool
}

func TestRender(t *testing.T) {
	n := &linked{Val: 1}
	n.next = n
	cases := []struct {
		v      any
		static string
		want   string
	}{
		{42, "int", "42"},
		{'a', "rune", "97 'a'"},
		{3.0, "float64", "3.0"},
		{"hi\n", "string", `"hi\n"`},
		{[]byte("abc"), "[]byte", `[]byte("abc")`},
		{[]byte{0, 1}, "[]byte", "[]byte{0x00, 0x01}"},
		{map[int]string{2: "b", 1: "a"}, "", `map[int]string{1: "a", 2: "b"}`},
		{map[string]bool{"b": true, "a": false}, "", `map[string]bool{"a": false, "b": true}`},
		{n, "", `&goprt.linked{Val: 1, next: &goprt.linked{…cycle}, tags: nil}`},
		{time.Second, "time.Duration", "1s"},
		{errors.New("bad"), "error", `error("bad")`},
		{[]int(nil), "[]int", "nil ([]int)"},
		{nil, "error", "nil"},
		{struct{ A, B int }{1, 2}, "", "{A: 1, B: 2}"},
	}
	for _, c := range cases {
		if got := stripANSI(render(c.v, c.static)); got != c.want {
			t.Errorf("render(%#v) = %s, want %s", c.v, got, c.want)
		}
	}
}

func TestLayoutWraps(t *testing.T) {
	got := render(make([]int, 200), "[]int")
	if !strings.Contains(got, "… 100 more") || strings.Count(got, "\n") > 10 {
		t.Errorf("long slices should be packed and truncated:\n%s", got)
	}
}

func TestCompact(t *testing.T) {
	got := compact(render(make([]string, 50), ""))
	if strings.Contains(got, "\n") || len([]rune(got)) > lineWidth {
		t.Errorf("compact output too long or multi-line: %q", got)
	}
}
