# gotinker — a Go playground for the terminal

[![CI](https://github.com/m-d-nabeel/gotinker/actions/workflows/ci.yml/badge.svg)](https://github.com/m-d-nabeel/gotinker/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/m-d-nabeel/gotinker.svg)](https://pkg.go.dev/github.com/m-d-nabeel/gotinker)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

`gotinker` is an interactive Go REPL that runs **real Go**: every input becomes part
of a program that is type-checked, compiled with your installed toolchain and
run. Generics, iterators, `range` over ints, `min`/`max`, the whole standard
library and third-party modules all work — there is no interpreter to lag
behind the language.

```
go ❯ words := strings.Fields("the quick brown fox")
words = []string{"the", "quick", "brown", "fox"}
go ❯ slices.SortStableFunc(words, func(a, b string) int { return len(a) - len(b) }); words
[]string{"the", "fox", "quick", "brown"}
go ❯ type Point struct{ X, Y int }
✓ type Point struct{X int; Y int}
go ❯ func (p Point) Dist() float64 { return math.Hypot(float64(p.X), float64(p.Y)) }
✓ func (Point) Dist() float64
go ❯ Point{3, 4}.Dist()
5.0  float64
```

## Install

```sh
go install github.com/m-d-nabeel/gotinker@latest
```

This puts `gotinker` in `$(go env GOPATH)/bin`; make sure that is on your `PATH`.
gotinker needs Go 1.26 or newer on `PATH`, since it compiles your code with it.
Reinstall gotinker after upgrading Go so its type checker matches the toolchain.

CI runs on Linux and macOS. Windows is untested.

## Features

- **Fast** — about 0.1–0.25 s per input; constants and types are answered
  instantly by the type checker without compiling.
- **Persistent session** — variables, functions and types carry over. Redefine
  anything: `x := "now a string"` shadows an earlier `x`, and a new
  `func f` replaces the old one.
- **Automatic imports** — use `strings.ToUpper` and `strings` is imported.
  Ambiguous names pick the package that has the symbol (`rand.IntN` →
  `math/rand/v2`, `rand.Intn` → `math/rand`).
- **Third-party modules** — `import "github.com/google/uuid"` fetches it;
  modules stay available in later sessions.
- **Pretty results** — colored, type-annotated values with real type names,
  unexported fields, cycle detection, `String()`/`Error()` methods honored,
  long collections wrapped and truncated.
- **Type-aware completion** (Tab) — package members, fields and methods of
  any expression, session variables, and locals inside a function you are
  still typing, each with its signature.
- **Signature hints** while typing a call, with the current argument
  highlighted.
- **A real multi-line editor** — blocks open automatically on an unclosed
  `{`, `(` or trailing operator, with auto-indent (and gofmt-style `case`
  alignment); brackets and quotes close automatically (typing the closer, or `}` on
  the line after, steps over it) and closing brackets dedent; ↑/↓ move between lines and reach
  history only from the first/last line; Enter on an earlier line adds a line
  there, Enter between brackets (`{|}`) opens them onto separate lines,
  Alt-Enter always adds a line, Ctrl-J runs from anywhere; Tab indents at
  the start of a line and completes elsewhere; Alt-←/→ and Alt-Backspace work
  on Go identifiers. Pasting a type with its methods and code using them
  just works.
- **Syntax highlighting**, persistent history with search (Ctrl-R) and
  fish-style suggestions, `.inputrc` support (vi mode too).
- **Errors point at your input** with a caret; panics show a short trace
  through your cells; a failing input is discarded so the session stays
  valid.
- **Ctrl-C stops a running program** (infinite loops included) without
  losing the session.
- **Export** — `:show` / `:save` turn the session into a standalone program
  that compiles; `:edit` opens the whole session in `$EDITOR`.

## Commands

| Command | |
|---|---|
| `:help [cmd]` | help |
| `:doc <symbol>` | docs (`:doc strings.Cut`, `:doc -all fmt`, or your own `:doc myFunc`) |
| `:type <expr>` | type of an expression, without running it |
| `:vars` | variables in scope and their current values |
| `:imports` | imported packages |
| `:cells`, `:drop <n>`, `:undo` | inspect and edit the session's history |
| `:show`, `:save <file>` | print or save the session as a Go program |
| `:load <file>` | load a Go file (declarations + `main` body) |
| `:edit` | edit the whole session in `$EDITOR`, then re-run it |
| `:get <module>` | add a module (`:get github.com/google/uuid@v1.6.0`) |
| `:reset`, `:clear`, `:quit` | |

Non-interactive use: `gotinker -e 'expr'`, `gotinker file.go`, or pipe code to stdin.

## How it works

Each input is parsed as declarations or statements. Declarations go to
package level; statement cells are replayed inside `main`, each in a nested
block so redeclaration is legal Go. The program is type-checked in-process
with `go/types` (against compiler export data, so it sees exactly what the
build sees), missing imports are added, then it is built with `go build` and
run. Replayed cells run with stdin/stdout/stderr pointed at `/dev/null`; the
newest cell's output goes to your terminal.

Because the session re-runs on each input:

- values from `rand` or `time.Now()` are recomputed every time;
- earlier reads from stdin see EOF;
- slow earlier cells (big loops, sleeps, network calls) slow down every input
  — `:cells` and `:drop` help.

## Files

- `~/.cache/gotinker/` (your OS user cache directory) — per-session build directories and the shared `go.mod`
  holding fetched modules
- `~/.local/share/gotinker/history` (or `$XDG_DATA_HOME/gotinker`) — input history

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE). The line editor in `third_party/readline` is a modified copy of
[reeflective/readline](https://github.com/reeflective/readline) (Apache 2.0);
see its [PATCHES.md](third_party/readline/PATCHES.md).
