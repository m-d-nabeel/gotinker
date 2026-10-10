# Changes to github.com/reeflective/readline

This directory is a modified copy of
[github.com/reeflective/readline](https://github.com/reeflective/readline)
v1.3.0, distributed under the Apache License 2.0 (see `LICENSE`). It is
vendored so `go install github.com/m-d-nabeel/gotinker@latest` works without a
`replace` directive.

Modifications made for gotinker:

1. **Import paths** — every file: `github.com/reeflective/readline/...`
   imports were rewritten to `github.com/m-d-nabeel/gotinker/third_party/readline/...`;
   `go.mod`, `go.sum` and the test files were removed.

2. **Completion menu sizing** — `internal/display/engine.go`,
   `AvailableHelperLines` (marked `gotinker patch`). gotinker runs readline with
   `cursor-position-probe` off, because terminal position replies interleaved
   with typed keys misrender the prompt. Without the probe `startRows` is -1
   and menus were sized one row taller than the terminal, pushing the input
   line off-screen and leaving debris when the menu closed. The patch assumes
   the input is on the last row in that case.

3. **Newline text hook** — `shell.go`, `history.go` (`acceptLineWith`): a new
   `Shell.NewlineText` field lets the caller choose the text inserted when
   Enter does not accept the line, and where the cursor lands within it, so
   gotinker can auto-indent and split `{}` / `()` / `[]` onto separate lines.

4. **Unbound escape sequences are dropped** — `emacs.go` (`selfInsert`,
   `dropEscapeSequence`, `dropUntilFinal`) and
   `internal/keymap/dispatch.go` (`dispatchKeys`, `isCSIFinal`): keys such as
   F5 (`ESC [15~`) or Alt sent as an ESC prefix used to be inserted as literal
   text (`^[[D`, `~`). The rest of an unmatched CSI/SS3 sequence is now
   discarded instead.

5. **Enter in the completion menu** — `completion.go` (`acceptCompletion`)
   and `internal/keymap/completion.go` (`menuselectKeys`): Enter while the
   menu is open used to fall through to `accept-line` and run the input; it
   now keeps the selected candidate and closes the menu.

6. **Bottom-of-window space without the probe** —
   `internal/display/refresh.go` (`ensureInputSpace`): with the probe off the
   function returned early, so when a multi-line input grew on the last rows
   of the window each new line was drawn one row too high, over earlier
   output. It now reserves the rows by printing newlines and moving back up,
   which scrolls only when needed.

7. **Pair deletion on its own** — `emacs.go` (`backwardDeleteChar`): a new
   `autopairs-delete` option makes Backspace inside an empty pair (`(|)`,
   `"|"`) delete both characters, without enabling `autopairs` insertion
   (which also pairs `<` and so breaks `x < y` in Go). gotinker inserts pairs
   itself.

8. **^C echoed at the end of the input** — `emacs.go` (`abort`): Ctrl-C
   echoed `^C` at the cursor, overwriting the text after it; the cursor now
   moves to the end of the input first.
