package main

import (
	"errors"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"
)

// exprType finds the type of the final expression of plan in a checked
// program, if the final statement is an expression.
func exprType(c *checked, plan *stmtPlan) (types.TypeAndValue, token.Pos, bool) {
	expr, start, _ := plan.lastExpr()
	if expr == nil || c.file == nil || c.prog.cellStart < 0 {
		return types.TypeAndValue{}, token.NoPos, false
	}
	pos := c.fileStart() + token.Pos(c.prog.cellStart+start+guardShift(plan.guards, start))
	var tv types.TypeAndValue
	found := false
	ast.Inspect(c.file, func(n ast.Node) bool {
		if found {
			return false
		}
		if e, ok := n.(ast.Expr); ok && e.Pos() == pos {
			if t, ok := c.info.Types[e]; ok {
				tv, found = t, true
			}
		}
		return true
	})
	return tv, pos, found
}

// notUsed ignores the "is not used" error of a bare final expression,
// which is expected until the expression is wrapped for display.
func notUsed(plan *stmtPlan) func(*checked, types.Error) bool {
	return func(c *checked, e types.Error) bool {
		_, pos, ok := exprType(c, plan)
		if !ok {
			expr, start, _ := plan.lastExpr()
			if expr == nil || c.prog.cellStart < 0 {
				return false
			}
			pos = c.fileStart() + token.Pos(c.prog.cellStart+start+guardShift(plan.guards, start))
		}
		return e.Pos == pos && (strings.HasSuffix(e.Msg, "is not used") || strings.HasSuffix(e.Msg, "is not an expression"))
	}
}

// TypeOf type-checks expr in the session's scope without running anything.
func (s *Session) TypeOf(expr string) (types.TypeAndValue, error) {
	plan, err := planStmts(s.nextID, expr)
	if err != nil {
		return types.TypeAndValue{}, err
	}
	if _, ok := plan.last.(*ast.ExprStmt); !ok || len(plan.stmts) != 1 {
		return types.TypeAndValue{}, errors.New("not an expression")
	}
	cur := &cell{id: plan.id, kind: stmtCell, src: plan.src, raw: plan.src}
	c := s.check(genInput{decls: s.decls, replay: s.stmts, imports: s.imports, current: cur}, notUsed(plan))
	if len(c.errs) > 0 {
		e := c.errs[0]
		pos := e.Fset.Position(e.Pos)
		if _, line, col := locate(pos, s.cellByID(nil, cur)); line > 0 {
			return types.TypeAndValue{}, &posError{line: line, col: col, msg: e.Msg}
		}
		return types.TypeAndValue{}, errors.New(e.Msg)
	}
	tv, _, ok := exprType(c, plan)
	if !ok {
		return types.TypeAndValue{}, errors.New("cannot determine type")
	}
	return tv, nil
}

type varInfo struct {
	name, typ string
	pos       token.Pos
}

// Vars lists the variables visible to the next input: locals defined by
// statement cells (innermost first wins) and package-level variables.
func (s *Session) Vars() []varInfo {
	cur := &cell{id: s.nextID, kind: stmtCell, src: ""}
	c := s.check(genInput{decls: s.decls, replay: s.stmts, imports: s.imports, current: cur}, nil)
	if c.pkg == nil || c.prog.cellStart < 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []varInfo
	pos := c.fileStart() + token.Pos(c.prog.cellStart)
	for sc := c.pkg.Scope().Innermost(pos); sc != nil && sc != types.Universe; sc = sc.Parent() {
		for _, name := range sc.Names() {
			if seen[name] || name == "_" || strings.HasPrefix(name, "__") {
				continue
			}
			if v, ok := sc.Lookup(name).(*types.Var); ok {
				seen[name] = true
				out = append(out, varInfo{name: name, typ: typeString(v.Type()), pos: v.Pos()})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pos < out[j].pos })
	return out
}

func (s *Session) usedImports() []importSpec {
	return s.check(genInput{decls: s.decls, replay: s.stmts, imports: s.imports}, nil).imports
}

// cells returns all committed cells in input order.
func (s *Session) cells() []*cell {
	all := append(append([]*cell(nil), s.decls...), s.stmts...)
	sort.Slice(all, func(i, j int) bool { return all[i].id < all[j].id })
	return all
}

func (s *Session) drop(id int) bool {
	for i, c := range s.decls {
		if c.id == id {
			s.decls = append(s.decls[:i:i], s.decls[i+1:]...)
			s.warnBroken()
			return true
		}
	}
	for i, c := range s.stmts {
		if c.id == id {
			s.stmts = append(s.stmts[:i:i], s.stmts[i+1:]...)
			s.warnBroken()
			return true
		}
	}
	return false
}

// warnBroken reports cells that no longer compile after a removal.
func (s *Session) warnBroken() {
	c := s.check(genInput{decls: s.decls, replay: s.stmts, imports: s.imports}, nil)
	if len(c.errs) > 0 {
		s.ui.note("warning: the session no longer compiles; :drop the cells below or redefine what they need")
		s.ui.typeErrors(c, s.cellByID(nil, nil))
	}
}

type snapshot struct {
	decls, stmts []*cell
	imports      []importSpec
}

func (s *Session) snapshot() snapshot {
	return snapshot{append([]*cell(nil), s.decls...), append([]*cell(nil), s.stmts...), append([]importSpec(nil), s.imports...)}
}

func (s *Session) restore(snap snapshot) {
	s.decls, s.stmts, s.imports = snap.decls, snap.stmts, snap.imports
}

func (s *Session) reset() {
	s.decls, s.stmts, s.imports = nil, nil, nil
}
