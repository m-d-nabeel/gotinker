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
