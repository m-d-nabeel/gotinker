# Contributing

Thanks for helping improve gotinker! Bug reports, ideas and pull requests are all
welcome.

## Reporting a bug

Open an issue with the input you typed, what you expected, what happened, and
the output of `gotinker -version` and `go version`. A sequence of inputs that
reproduces the problem from a fresh session is ideal; piping it works too:

```sh
printf 'x := 1\nx + 1\n' | gotinker
```

## Development

```sh
git clone https://github.com/m-d-nabeel/gotinker && cd gotinker
go test ./...          # unit tests plus an end-to-end session (needs Go on PATH)
go run .               # try your change
```

Layout:

| Path | |
|---|---|
| `session.go` | parses inputs into cells, generates and type-checks the program, builds and runs it |
| `goprt/` | runtime linked into every session program: value printer, output muting, panic reports |
| `complete.go` | Tab completion and signature hints, via `go/types` |
| `repl.go`, `editing.go` | the prompt loop and Go-aware line editing (indent, keys, word motion) |
| `commands.go`, `ui.go`, `highlight.go` | `:commands`, terminal output, syntax highlighting |
| `export.go` | `:show` / `:save` |
| `third_party/readline` | patched line editor, see `PATCHES.md` there |

Before sending a pull request, run `gofmt`, `go vet ./...` and `go test ./...`,
and add a test for behaviour you change where practical. Interactive
rendering is hard to unit-test; please describe how you checked it in a real
terminal.

By contributing you agree that your contributions are licensed under the MIT
License.
