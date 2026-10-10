package main

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// stdIndex maps package names (as used in code, e.g. "rand") to the standard
// library import paths that declare them.
type stdIndex struct {
	paths  []string
	byName map[string][]string
}

// preferred lists, for ambiguous package names, the order in which candidate
// paths are tried when auto-importing. The first one that exports the used
// symbol wins.
var preferred = map[string][]string{
	"rand":     {"math/rand/v2", "math/rand", "crypto/rand"},
	"template": {"text/template", "html/template"},
	"scanner":  {"text/scanner", "go/scanner"},
	"pprof":    {"runtime/pprof", "net/http/pprof"},
	"trace":    {"runtime/trace"},
	"constant": {"go/constant"},
	"build":    {"go/build"},
	"format":   {"go/format"},
	"parser":   {"go/parser"},
	"printer":  {"go/printer"},
	"types":    {"go/types"},
	"doc":      {"go/doc"},
	"ast":      {"go/ast"},
	"token":    {"go/token"},
	"elf":      {"debug/elf"},
	"pe":       {"debug/pe"},
}

var versionElem = regexp.MustCompile(`^v[0-9]+$`)

// pkgName returns the name a package is conventionally referred to by.
func pkgName(importPath string) string {
	base := path.Base(importPath)
	if versionElem.MatchString(base) {
		base = path.Base(path.Dir(importPath))
	}
	return base
}

// loadStdIndex lists the standard library, caching the result per Go version
// since `go list std` takes a noticeable fraction of a second.
func loadStdIndex(cacheDir, goVersion string) (*stdIndex, error) {
	cache := filepath.Join(cacheDir, "std-"+goVersion+".txt")
	data, err := os.ReadFile(cache)
	if err != nil {
		out, err := goCmd(context.Background(), os.TempDir(), "list", "std").Output()
		if err != nil {
			return nil, err
		}
		data = out
		_ = os.WriteFile(cache, data, 0o644)
	}
	idx := &stdIndex{byName: map[string][]string{}}
	for _, p := range strings.Fields(string(data)) {
		if strings.Contains(p, "internal") || strings.HasPrefix(p, "vendor/") || strings.HasPrefix(p, "cmd/") {
			continue
		}
		idx.paths = append(idx.paths, p)
		n := pkgName(p)
		idx.byName[n] = append(idx.byName[n], p)
	}
	sort.Strings(idx.paths)
	for n, ps := range idx.byName {
		order := preferred[n]
		rank := func(p string) int {
			for i, q := range order {
				if q == p {
					return i
				}
			}
			return len(order) + len(p) // otherwise shortest path first
		}
		sort.SliceStable(ps, func(i, j int) bool { return rank(ps[i]) < rank(ps[j]) })
		idx.byName[n] = ps
	}
	return idx, nil
}

func (x *stdIndex) isStd(importPath string) bool {
	if x == nil {
		return !strings.Contains(strings.SplitN(importPath, "/", 2)[0], ".")
	}
	i := sort.SearchStrings(x.paths, importPath)
	return i < len(x.paths) && x.paths[i] == importPath
}

// names returns every package name, sorted.
func (x *stdIndex) names() []string {
	out := make([]string, 0, len(x.byName))
	for n := range x.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
