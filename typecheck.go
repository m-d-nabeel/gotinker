package main

import (
	"bufio"
	"bytes"
	"fmt"
	"go/importer"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// exportImporter type-checks against compiler export data, which `go list
// -export` produces (and the build cache keeps), so imports are resolved
// exactly as the real build will resolve them, third-party modules included.
type exportImporter struct {
	dir  string // module directory that `go list` runs in
	fset *token.FileSet

	mu    sync.Mutex
	files map[string]string // import path -> export data file
	imp   types.ImporterFrom
}

func newExportImporter(dir string, fset *token.FileSet) *exportImporter {
	e := &exportImporter{dir: dir, fset: fset, files: map[string]string{}}
	e.reset()
	return e
}

// reset drops cached packages, needed after go.mod changes.
func (e *exportImporter) reset() {
	e.imp = importer.ForCompiler(e.fset, "gc", e.lookup).(types.ImporterFrom)
}

func (e *exportImporter) lookup(path string) (io.ReadCloser, error) {
	e.mu.Lock()
	file, ok := e.files[path]
	e.mu.Unlock()
	if !ok {
		if err := e.load(path); err != nil {
			return nil, err
		}
		e.mu.Lock()
		file, ok = e.files[path]
		e.mu.Unlock()
		if !ok || file == "" {
			return nil, fmt.Errorf("no export data for %q", path)
		}
	}
	return os.Open(file)
}

// load records export data files for paths and all their dependencies,
// compiling them into the build cache if needed.
func (e *exportImporter) load(paths ...string) error {
	args := append([]string{"list", "-e", "-export", "-deps", "-f", "{{.ImportPath}}\t{{.Export}}\t{{if .Error}}{{.Error.Err}}{{end}}", "--"}, paths...)
	cmd := exec.Command("go", args...)
	cmd.Dir = e.dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("go list: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var firstErr error
	e.mu.Lock()
	defer e.mu.Unlock()
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 3)
		if len(parts) < 2 {
			continue
		}
		if len(parts) == 3 && parts[2] != "" && firstErr == nil {
			firstErr = fmt.Errorf("%s", parts[2])
		}
		if parts[1] != "" {
			e.files[parts[0]] = parts[1]
		}
	}
	return firstErr
}

func (e *exportImporter) Import(path string) (*types.Package, error) {
	return e.imp.ImportFrom(path, e.dir, 0)
}

func (e *exportImporter) ImportFrom(path, dir string, mode types.ImportMode) (*types.Package, error) {
	return e.imp.ImportFrom(path, e.dir, mode)
}
