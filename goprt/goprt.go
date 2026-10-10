// Package goprt is the runtime support linked into every gotinker session program.
//
// gotinker re-runs the whole session on each input. Cells that already ran are
// replayed with their output muted (see mute), then Unmute is called right
// before the new cell so only its output reaches the terminal. Results of
// expressions and new variables are printed with Show and Var.
package goprt

import (
	"fmt"
	"math"
	"os"
	"reflect"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
	"unsafe"
)

var color = os.Getenv("GOTINKER_COLOR") == "1"

func init() {
	if os.Getenv("GOTINKER_MUTE") == "1" {
		mute()
	}
}

// Unmute restores stdin/stdout/stderr before the newest cell runs.
func Unmute() { unmute() }

// Finish is deferred at the start of the newest cell. On a normal return it
// mutes output again, so deferred calls registered by older cells stay quiet
// when main returns. If the cell panics it reports the panic (output is
// still unmuted, unlike Go's default report, which runs after deferred
// calls) and exits.
func Finish() {
	r := recover()
	if r == nil {
		// Tell gotinker the input ran to completion (an os.Exit inside it
		// would skip this and leave later inputs silently unreachable).
		if done := os.Getenv("GOTINKER_DONE"); done != "" {
			_ = os.WriteFile(done, nil, 0o644)
		}
		mute()
		return
	}
	fmt.Fprintf(os.Stderr, "panic: %s\n\ngoroutine 1 [running]:\n%s\n", panicValue(r), trimStack(debug.Stack()))
	os.Exit(2)
}

func panicValue(r any) string {
	switch v := r.(type) {
	case error:
		return v.Error()
	case fmt.Stringer:
		return v.String()
	case string:
		return v
	}
	return fmt.Sprintf("%v", r)
}

// trimStack drops the frames of the stack that belong to the panic machinery
// (debug.Stack, Finish, runtime.gopanic), keeping the user's frames.
func trimStack(stack []byte) string {
	lines := strings.Split(string(stack), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "panic(") {
			return strings.Join(lines[i+2:], "\n")
		}
	}
	if len(lines) > 1 {
		return strings.Join(lines[1:], "\n")
	}
	return string(stack)
}

// Types carries the static types (as computed by go/types) of the values
// passed to Show or Var, so untyped constants, runes and interfaces print
// with the type the user wrote rather than the dynamic one.
type Types struct{ names []string }

// T records static type names for the following Show or Var call.
func T(names ...string) Types { return Types{names} }

// Show prints the result of an expression cell.
func (t Types) Show(vals ...any) {
	if len(vals) == 1 {
		static := t.name(0)
		body := render(vals[0], static)
		fmt.Fprintln(os.Stdout, body+typeNote(body, static, vals[0]))
		return
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = render(v, t.name(i))
	}
	note := ""
	if len(t.names) > 0 {
		note = "  " + paint(dim, "("+strings.Join(t.names, ", ")+")")
	}
	fmt.Fprintln(os.Stdout, strings.Join(parts, paint(dim, ", "))+note)
}

// Var prints a variable defined by the newest cell, "name = value  type",
// kept to one line: evaluate the name to see all of it.
func (t Types) Var(name string, v any) {
	static := t.name(0)
	body := compact(render(v, static))
	fmt.Fprintln(os.Stdout, paint(dim, name+" = ")+body+typeNote(body, static, v))
}

// compact squeezes a rendering onto one line of at most lineWidth runes.
func compact(body string) string {
	if !strings.Contains(body, "\n") && utf8.RuneCountInString(stripANSI(body)) <= lineWidth {
		return body
	}
	plain := strings.Join(strings.Fields(stripANSI(body)), " ")
	plain = strings.ReplaceAll(plain, "{ ", "{")
	plain = strings.ReplaceAll(plain, ", }", "}")
	if r := []rune(plain); len(r) > lineWidth {
		plain = string(r[:lineWidth-1]) + "…"
	}
	return plain
}

func (t Types) name(i int) string {
	if i < len(t.names) {
		return t.names[i]
	}
	return ""
}

// typeNote returns the dimmed type suffix, skipped when the rendering already
// starts with the type (composite literals like main.Point{X: 1}).
func typeNote(body, static string, v any) string {
	name := static
	if name == "" && v != nil {
		name = typeName(reflect.TypeOf(v))
	}
	if v != nil && reflect.TypeOf(v).Kind() == reflect.Func {
		return "" // the rendering of a func is its type already
	}
	plain := stripANSI(body)
	if name == "" || strings.HasPrefix(plain, name) || strings.HasPrefix(plain, "&"+strings.TrimPrefix(name, "*")) || strings.Contains(body, "\n") {
		return ""
	}
	return "  " + paint(dim, name)
}

// ---------------------------------------------------------------- printer

const (
	maxDepth   = 8
	maxElems   = 100
	maxString  = 4000
	lineWidth  = 90
	indentUnit = "  "
)

const (
	dim     = "2"
	green   = "32"
	cyan    = "36"
	yellow  = "33"
	magenta = "35"
	red     = "31"
	blue    = "34"
)

func paint(code, s string) string {
	if !color || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b[") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// node is a rendered value: either a leaf, or a composite with children that
// is laid out on one line when it fits and expanded otherwise.
type node struct {
	label    string // "Field: " or "key: " prefix inside a parent
	leaf     string
	open     string
	close    string
	children []node
	more     string // "… 12 more" trailer
}

func (n node) flat() string {
	if n.children == nil {
		return n.label + n.leaf
	}
	parts := make([]string, 0, len(n.children)+1)
	for _, c := range n.children {
		parts = append(parts, c.flat())
	}
	if n.more != "" {
		parts = append(parts, paint(dim, n.more))
	}
	return n.label + n.open + strings.Join(parts, ", ") + n.close
}

func (n node) layout(indent string) string {
	f := n.flat()
	if n.children == nil || (utf8.RuneCountInString(stripANSI(f))+len(indent) <= lineWidth && !strings.Contains(f, "\n")) {
		return f
	}
	inner := indent + indentUnit
	var b strings.Builder
	b.WriteString(n.label + n.open)
	b.WriteString("\n")
	if n.allLeaves() {
		// Pack scalars into lines instead of one per line.
		line := ""
		for _, c := range n.children {
			item := c.flat() + ","
			if line != "" && utf8.RuneCountInString(stripANSI(inner+line+" "+item)) > lineWidth {
				b.WriteString(inner + line + "\n")
				line = ""
			}
			if line != "" {
				line += " "
			}
			line += item
		}
		if line != "" {
			b.WriteString(inner + line + "\n")
		}
	} else {
		for _, c := range n.children {
			b.WriteString(inner + c.layout(inner) + ",\n")
		}
	}
	if n.more != "" {
		b.WriteString(inner + paint(dim, n.more) + "\n")
	}
	b.WriteString(indent + n.close)
	return b.String()
}

func (n node) allLeaves() bool {
	for _, c := range n.children {
		if c.children != nil || c.label != "" {
			return false
		}
	}
	return true
}

type printer struct {
	seen map[uintptr]bool
}

func render(v any, static string) (out string) {
	if v != nil && static != "" && reflect.TypeOf(v).Kind() == reflect.Func {
		if reflect.ValueOf(v).IsNil() {
			return paint(magenta, "nil") + paint(dim, " ("+static+")")
		}
		return paint(dim, static) // keeps parameter names, unlike reflect
	}
	defer func() {
		if r := recover(); r != nil {
			out = fmt.Sprintf("%v %s", v, paint(dim, fmt.Sprintf("(print failed: %v)", r)))
		}
	}()
	if v == nil {
		return paint(magenta, "nil")
	}
	rv := reflect.ValueOf(v)
	// Copy into an addressable value so unexported fields can be read.
	addr := reflect.New(rv.Type()).Elem()
	addr.Set(rv)
	p := &printer{seen: map[uintptr]bool{}}
	n := p.value(addr, 0)
	if (static == "rune" || static == "untyped rune") && rv.CanInt() {
		r := rv.Int()
		if r >= 0 && r <= utf8.MaxRune {
			n.leaf += " " + paint(green, strconv.QuoteRune(rune(r)))
		}
	}
	return n.layout("")
}

func typeName(t reflect.Type) string {
	if t == nil {
		return ""
	}
	s := t.String()
	if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 && t.Elem().Name() == "uint8" {
		s = "[]byte"
	}
	s = strings.ReplaceAll(s, "main.", "")
	s = strings.ReplaceAll(s, "interface {}", "any")
	return s
}

// readable returns v made readable through reflection even when it was
// reached through an unexported field.
func readable(v reflect.Value) reflect.Value {
	if v.CanInterface() || !v.CanAddr() {
		return v
	}
	return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
}

var (
	errorType    = reflect.TypeOf((*error)(nil)).Elem()
	stringerType = reflect.TypeOf((*fmt.Stringer)(nil)).Elem()
)

// custom renders values whose type has an Error or String method, which is
// almost always more useful than their raw fields (time.Time, errors, ...).
func (p *printer) custom(v reflect.Value) (node, bool) {
	t := v.Type()
	if t.Name() == "" && t.Kind() != reflect.Pointer {
		return node{}, false
	}
	call := func(m reflect.Value) (s string, ok bool) {
		defer func() {
			if recover() != nil {
				ok = false
			}
		}()
		return m.Call(nil)[0].String(), true
	}
	try := func(method string, wrap func(string) string) (node, bool) {
		var m reflect.Value
		if v.CanInterface() {
			m = v.MethodByName(method)
			if !m.IsValid() && v.CanAddr() {
				m = v.Addr().MethodByName(method)
			}
		}
		if !m.IsValid() || m.Type().NumIn() != 0 || m.Type().NumOut() != 1 || m.Type().Out(0).Kind() != reflect.String {
			return node{}, false
		}
		if v.Kind() == reflect.Pointer && v.IsNil() {
			return node{}, false
		}
		s, ok := call(m)
		if !ok {
			return node{}, false
		}
		return node{leaf: wrap(s)}, true
	}
	if t.Implements(errorType) || reflect.PointerTo(t).Implements(errorType) {
		if n, ok := try("Error", func(s string) string {
			return paint(red, "error") + paint(dim, "(") + strconv.Quote(s) + paint(dim, ")")
		}); ok {
			return n, true
		}
	}
	if t.Implements(stringerType) || reflect.PointerTo(t).Implements(stringerType) {
		return try("String", func(s string) string {
			if s == "" {
				return typeName(t) + paint(dim, `("")`) // e.g. an empty strings.Builder
			}
			if len(s) > maxString {
				s = s[:maxString] + "…"
			}
			return paint(blue, s)
		})
	}
	return node{}, false
}

func (p *printer) value(v reflect.Value, depth int) node {
	if !v.IsValid() {
		return node{leaf: paint(magenta, "nil")}
	}
	v = readable(v)
	if depth > maxDepth {
		return node{leaf: paint(dim, "…")}
	}
	if n, ok := p.custom(v); ok {
		return n
	}
	t := v.Type()
	switch v.Kind() {
	case reflect.Bool:
		return node{leaf: paint(yellow, strconv.FormatBool(v.Bool()))}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return node{leaf: paint(cyan, strconv.FormatInt(v.Int(), 10))}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return node{leaf: paint(cyan, strconv.FormatUint(v.Uint(), 10))}
	case reflect.Uintptr:
		return node{leaf: paint(cyan, fmt.Sprintf("0x%x", v.Uint()))}
	case reflect.Float32, reflect.Float64:
		return node{leaf: paint(cyan, formatFloat(v.Float(), t.Bits()))}
	case reflect.Complex64, reflect.Complex128:
		return node{leaf: paint(cyan, strconv.FormatComplex(v.Complex(), 'g', -1, t.Bits()))}
	case reflect.String:
		s := v.String()
		trunc := ""
		if len(s) > maxString {
			trunc = paint(dim, fmt.Sprintf("… (%d bytes total)", len(s)))
			s = s[:maxString]
		}
		return node{leaf: paint(green, strconv.Quote(s)) + trunc}
	case reflect.Pointer:
		if v.IsNil() {
			return p.nilValue(t, depth)
		}
		ptr := v.Pointer()
		if p.seen[ptr] {
			return node{leaf: paint(dim, "&"+typeName(t.Elem())+"{…cycle}")}
		}
		p.seen[ptr] = true
		defer delete(p.seen, ptr)
		inner := p.value(v.Elem(), depth+1)
		switch t.Elem().Kind() {
		case reflect.Struct, reflect.Array, reflect.Slice, reflect.Map:
			if inner.children != nil {
				inner.open = "&" + inner.open
			} else {
				inner.leaf = "&" + inner.leaf
			}
			return inner
		}
		return node{leaf: paint(dim, fmt.Sprintf("(%s)(0x%x)", typeName(t), ptr)) + " → " + inner.flat()}
	case reflect.Interface:
		if v.IsNil() {
			return node{leaf: paint(magenta, "nil")}
		}
		return p.value(v.Elem(), depth)
	case reflect.Struct:
		return p.structValue(v, depth)
	case reflect.Slice:
		if v.IsNil() {
			return p.nilValue(t, depth)
		}
		if t.Elem().Kind() == reflect.Uint8 {
			return p.bytes(v)
		}
		return p.list(v, depth, typeName(t))
	case reflect.Array:
		return p.list(v, depth, typeName(t))
	case reflect.Map:
		if v.IsNil() {
			return p.nilValue(t, depth)
		}
		return p.mapValue(v, depth)
	case reflect.Chan:
		if v.IsNil() {
			return p.nilValue(t, depth)
		}
		return node{leaf: paint(dim, fmt.Sprintf("%s (len %d, cap %d)", typeName(t), v.Len(), v.Cap()))}
	case reflect.Func:
		if v.IsNil() {
			return p.nilValue(t, depth)
		}
		return node{leaf: paint(dim, typeName(t))}
	case reflect.UnsafePointer:
		return node{leaf: paint(cyan, fmt.Sprintf("unsafe.Pointer(0x%x)", v.Pointer()))}
	}
	return node{leaf: fmt.Sprint(v)}
}

func (p *printer) nilValue(t reflect.Type, depth int) node {
	if depth > 0 {
		return node{leaf: paint(magenta, "nil")}
	}
	return node{leaf: paint(magenta, "nil") + paint(dim, " ("+typeName(t)+")")}
}

func formatFloat(f float64, bits int) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return strconv.FormatFloat(f, 'g', -1, bits)
	}
	s := strconv.FormatFloat(f, 'g', -1, bits)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

func (p *printer) structValue(v reflect.Value, depth int) node {
	t := v.Type()
	name := typeName(t)
	if t.Name() == "" {
		name = ""
	}
	n := node{open: name + "{", close: "}", children: []node{}}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		val := p.value(v.Field(i), depth+1)
		val.label = paint(dim, f.Name+": ")
		n.children = append(n.children, val)
	}
	return n
}

func (p *printer) list(v reflect.Value, depth int, name string) node {
	n := node{open: name + "{", close: "}", children: []node{}}
	l := v.Len()
	for i := 0; i < l && i < maxElems; i++ {
		n.children = append(n.children, p.value(v.Index(i), depth+1))
	}
	if l > maxElems {
		n.more = fmt.Sprintf("… %d more", l-maxElems)
	}
	if v.Kind() == reflect.Slice {
		n.close = "}" + paint(dim, fmt.Sprintf(" len %d cap %d", l, v.Cap()))
		if l <= maxElems && l == v.Cap() {
			n.close = "}"
		}
	}
	return n
}

func (p *printer) bytes(v reflect.Value) node {
	b := v.Bytes()
	if utf8.Valid(b) && printable(b) {
		s := string(b)
		trunc := ""
		if len(s) > maxString {
			trunc = paint(dim, fmt.Sprintf("… (%d bytes total)", len(s)))
			s = s[:maxString]
		}
		return node{leaf: typeName(v.Type()) + "(" + paint(green, strconv.Quote(s)) + ")" + trunc}
	}
	n := node{open: typeName(v.Type()) + "{", close: "}", children: []node{}}
	for i, c := range b {
		if i == maxElems {
			n.more = fmt.Sprintf("… %d more", len(b)-maxElems)
			break
		}
		n.children = append(n.children, node{leaf: paint(cyan, fmt.Sprintf("0x%02x", c))})
	}
	return n
}

func printable(b []byte) bool {
	for _, r := range string(b) {
		if r < 0x20 && r != '\n' && r != '\t' && r != '\r' {
			return false
		}
	}
	return true
}

func (p *printer) mapValue(v reflect.Value, depth int) node {
	keys := v.MapKeys()
	sortKeys(keys)
	n := node{open: typeName(v.Type()) + "{", close: "}", children: []node{}}
	for i, k := range keys {
		if i == maxElems {
			n.more = fmt.Sprintf("… %d more", len(keys)-maxElems)
			break
		}
		kn := p.value(k, depth+1)
		vn := p.value(v.MapIndex(k), depth+1)
		vn.label = kn.flat() + ": "
		n.children = append(n.children, vn)
	}
	return n
}

func sortKeys(keys []reflect.Value) {
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		switch a.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return a.Int() < b.Int()
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			return a.Uint() < b.Uint()
		case reflect.Float32, reflect.Float64:
			return a.Float() < b.Float()
		case reflect.String:
			return a.String() < b.String()
		case reflect.Bool:
			return !a.Bool() && b.Bool()
		}
		return fmt.Sprint(a) < fmt.Sprint(b)
	})
}

// P is a no-op used by gotinker's completer to ask the type checker about the
// expression under the cursor.
func P(...any) {}
