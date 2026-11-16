package review_test

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jagreehal/errpath-go/review"
)

// Mutation test for the review: every edit to a failure path must be
// reported, and every edit that leaves failure paths alone must stay silent.

type edit struct {
	start, end int
	text       string
}

type mutant struct {
	op    string
	want  bool // reported
	line  int
	edits []edit
}

func TestMutations(t *testing.T) {
	path := filepath.Join("..", "testdata", "corpus", "control_flow.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	baseDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseDir, "c.go"), src, 0o600); err != nil {
		t.Fatal(err)
	}

	g := &gen{fset: fset, src: src, parents: parents(f)}
	g.file(f)
	tried := map[string]int{}
	for _, m := range g.out {
		tried[m.op]++
	}
	if g.sameTarget == 0 || g.sameSwap == 0 {
		t.Fatalf("the corpus lost its jumps that land in the same place either way (%d, %d)", g.sameTarget, g.sameSwap)
	}
	if len(g.out) < 300 {
		t.Fatalf("only %d mutants; the generator has gone quiet: %v", len(g.out), tried)
	}

	wrong := map[string]int{}
	headDir := t.TempDir()
	for _, m := range g.out {
		if err := os.WriteFile(filepath.Join(headDir, "c.go"), apply(src, m.edits), 0o600); err != nil {
			t.Fatal(err)
		}
		rev, err := review.ComparePaths(baseDir, headDir, "collapsed")
		if err != nil {
			t.Fatal(err)
		}
		if rev.Changed == m.want {
			continue
		}
		wrong[m.op]++
		what := "missed"
		if !m.want {
			what = "noise"
		}
		first, _, _ := strings.Cut(strings.TrimSpace(string(src[m.edits[0].start:m.edits[0].end])), "\n")
		t.Errorf("%s %s at line %d `%s`\n%s", what, m.op, m.line, first, describe(rev))
	}
	for _, op := range slices.Sorted(maps.Keys(tried)) {
		t.Logf("%-22s %4d tried %3d wrong", op, tried[op], wrong[op])
	}
}

func apply(src []byte, edits []edit) []byte {
	sorted := slices.Clone(edits)
	slices.SortFunc(sorted, func(a, b edit) int { return cmp.Compare(b.start, a.start) })
	out := slices.Clone(src)
	for _, e := range sorted {
		out = slices.Concat(out[:e.start], []byte(e.text), out[e.end:])
	}
	return out
}

type gen struct {
	fset    *token.FileSet
	src     []byte
	out     []mutant
	parents map[ast.Node]ast.Node
	// sameTarget counts labelled jumps whose label changes nothing, and
	// sameSwap break/continue swaps that land in the same place.
	sameTarget, sameSwap int
}

func parents(f *ast.File) map[ast.Node]ast.Node {
	m := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) > 0 {
			m[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		return true
	})
	return m
}

// destination is where a break or continue sends control, worked out from
// the source the way a reader would: the frame it leaves, then whatever runs
// next. Statements off failure paths don't count as running next.
func (g *gen) destination(b *ast.BranchStmt, tok token.Token, label *ast.Ident) string {
	for n := g.parents[b]; n != nil; n = g.parents[n] {
		switch n.(type) {
		case *ast.FuncLit, *ast.FuncDecl:
			return "nowhere"
		case *ast.ForStmt, *ast.RangeStmt:
		case *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
			if tok == token.CONTINUE {
				continue
			}
		default:
			continue
		}
		named, _ := g.parents[n].(*ast.LabeledStmt)
		if label != nil && (named == nil || named.Label.Name != label.Name) {
			continue
		}
		if tok == token.CONTINUE {
			return fmt.Sprintf("next %p", n)
		}
		return g.after(n)
	}
	return "nowhere"
}

// after is what runs once statement s finishes.
func (g *gen) after(s ast.Node) string {
	if l, ok := g.parents[s].(*ast.LabeledStmt); ok {
		s = l
	}
	parent := g.parents[s]
	list := stmtList(parent)
	if i := slices.IndexFunc(list, func(st ast.Stmt) bool { return st == s }); i >= 0 {
		for _, st := range list[i+1:] {
			if onFailurePath(st, false, nil) {
				return fmt.Sprintf("after %p", s)
			}
		}
	}
	switch p := parent.(type) {
	case *ast.ForStmt, *ast.RangeStmt:
		return fmt.Sprintf("next %p", p) // s is the loop body
	case *ast.FuncDecl, *ast.FuncLit:
		return "end"
	case *ast.BlockStmt:
		if loop, ok := g.parents[p].(*ast.ForStmt); ok && loop.Body == p {
			return fmt.Sprintf("next %p", loop)
		}
		if loop, ok := g.parents[p].(*ast.RangeStmt); ok && loop.Body == p {
			return fmt.Sprintf("next %p", loop)
		}
		return g.after(p)
	case nil:
		return "end"
	}
	return g.after(parent) // a clause, if or labelled statement
}

func (g *gen) off(p token.Pos) int  { return g.fset.Position(p).Offset }
func (g *gen) line(p token.Pos) int { return g.fset.Position(p).Line }
func (g *gen) text(n ast.Node) string {
	return string(g.src[g.off(n.Pos()):g.off(n.End())])
}

func (g *gen) add(op string, want bool, at token.Pos, edits ...edit) {
	g.out = append(g.out, mutant{op: op, want: want, line: g.line(at), edits: edits})
}

// replace is an edit swapping a node's text for another.
func (g *gen) replace(n ast.Node, text string) edit {
	return edit{g.off(n.Pos()), g.off(n.End()), text}
}

func (g *gen) file(f *ast.File) {
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
			g.function(fd.Type, fd.Body, fd.Recv)
		}
	}
}

// function generates mutants for one function or closure body.
func (g *gen) function(ft *ast.FuncType, body *ast.BlockStmt, recv *ast.FieldList) {
	g.renameLocals(ft, body, recv)
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			g.function(x.Type, x.Body, nil)
			return false
		case *ast.BlockStmt:
			g.quietEdits(x)
		case *ast.IfStmt:
			g.ifStmt(x)
		case *ast.ForStmt:
			if x.Cond != nil {
				// The condition's own panics sit inside the loop too.
				g.condition(x.Cond, onFailurePath(x.Body, false, nil) || onFailurePath(&ast.ExprStmt{X: x.Cond}, false, nil))
			}
		case *ast.BranchStmt:
			g.jump(x)
		case *ast.ReturnStmt:
			g.returnValue(x, ft)
		case *ast.CallExpr:
			g.reflow(x)
		}
		for _, st := range stmtList(n) {
			tail := n == body && st == body.List[len(body.List)-1]
			want := onFailurePath(st, tail, ft) && !g.equivalent(st)
			g.add("delete-statement", want, st.Pos(), g.replace(st, ""))
		}
		g.swaps(stmtList(n))
		return true
	})
}

// swaps swaps neighbours. Order only matters between two statements that are
// both on a failure path; any return counts, since it hides what follows.
func (g *gen) swaps(list []ast.Stmt) {
	for i := 0; i+1 < len(list); i++ {
		a, b := list[i], list[i+1]
		if g.text(a) == g.text(b) {
			continue
		}
		want := onFailurePath(a, false, nil) && onFailurePath(b, false, nil)
		g.add("swap-statements", want, a.Pos(), g.replace(a, g.text(b)), g.replace(b, g.text(a)))
	}
}

func (g *gen) ifStmt(s *ast.IfStmt) {
	matters := onFailurePath(s.Body, false, nil) || (s.Else != nil && onFailurePath(s.Else, false, nil))
	g.condition(s.Cond, matters)
	els, ok := s.Else.(*ast.BlockStmt)
	if !ok {
		return
	}
	g.add("delete-else", onFailurePath(els, false, nil), s.Else.Pos(), edit{g.off(s.Body.End()), g.off(els.End()), ""})
	if g.text(s.Body) != g.text(els) {
		g.add("swap-arms", matters, s.Body.Pos(), g.replace(s.Body, g.text(els)), g.replace(els, g.text(s.Body)))
	}
}

// condition mutates a condition that decides whether failure paths run.
func (g *gen) condition(c ast.Expr, matters bool) {
	g.add("negate-condition", matters, c.Pos(), g.replace(c, "!("+g.text(c)+")"))
	g.add("comment-in-condition", false, c.Pos(), edit{g.off(c.Pos()), g.off(c.Pos()), "/* why */ "})
	bin, ok := c.(*ast.BinaryExpr)
	if !ok {
		return
	}
	swapped := map[token.Token]string{
		token.LSS: "<=", token.GTR: ">=", token.LEQ: "<", token.GEQ: ">",
		token.EQL: "!=", token.NEQ: "==", token.LAND: "||", token.LOR: "&&",
	}
	if to, ok := swapped[bin.Op]; ok {
		at := g.off(bin.OpPos)
		g.add("swap-operator", matters, bin.OpPos, edit{at, at + len(bin.Op.String()), to})
	}
}

func (g *gen) jump(b *ast.BranchStmt) {
	if b.Tok != token.BREAK && b.Tok != token.CONTINUE {
		return
	}
	other := map[token.Token]token.Token{token.BREAK: token.CONTINUE, token.CONTINUE: token.BREAK}[b.Tok]
	here := g.destination(b, b.Tok, b.Label)
	at := g.off(b.TokPos)
	// A jump spelled differently that lands in the same place is the same jump.
	if g.destination(b, other, b.Label) == here {
		g.sameSwap++
	}
	g.add("swap-jump", g.destination(b, other, b.Label) != here, b.Pos(), edit{at, at + len(b.Tok.String()), other.String()})
	if b.Label != nil {
		if here == g.destination(b, b.Tok, nil) {
			g.sameTarget++
		}
		g.add("retarget-jump", g.destination(b, b.Tok, nil) != here, b.Pos(), g.replace(b, b.Tok.String()))
	}
}

// returnValue flips whether a return fails.
func (g *gen) returnValue(r *ast.ReturnStmt, ft *ast.FuncType) {
	if ft.Results == nil || len(r.Results) == 0 || len(r.Results) != resultCount(ft) || !lastIsError(ft) {
		return
	}
	last := r.Results[len(r.Results)-1]
	if id, ok := last.(*ast.Ident); ok && id.Name == "nil" {
		g.add("nil-to-error", true, r.Pos(), g.replace(last, "ErrMutant"))
	} else {
		g.add("error-to-nil", true, r.Pos(), g.replace(last, "nil"))
	}
}

// quietEdits are edits a reviewer shouldn't hear about.
func (g *gen) quietEdits(b *ast.BlockStmt) {
	at := g.off(b.Lbrace) + 1
	g.add("add-comment", false, b.Pos(), edit{at, at, "\n// a note\n"})
	if len(b.List) > 0 && stmtList(b.List[0]) != nil {
		return // a switch or select body holds cases, not statements
	}
	g.add("add-plain-statement", false, b.Pos(), edit{at, at, "\n_ = 1 + 1\n"})
}

// reflow splits a call's arguments over lines, as a formatter might.
func (g *gen) reflow(c *ast.CallExpr) {
	if len(c.Args) < 2 {
		return
	}
	var edits []edit
	for _, a := range c.Args[1:] {
		at := g.off(a.Pos())
		edits = append(edits, edit{at, at, "\n\t\t"})
	}
	g.add("reflow-lines", false, c.Pos(), edits...)
}

// renameLocals renames each declared local everywhere in the function.
// A name keeps its prefix, so err stays recognisably an error.
func (g *gen) renameLocals(ft *ast.FuncType, body *ast.BlockStmt, recv *ast.FieldList) {
	roots := []ast.Node{ft, body}
	if recv != nil {
		roots = append(roots, recv)
	}
	for _, decl := range declaredLocals(ft, body, recv) {
		var edits []edit
		for _, root := range roots {
			for _, id := range uses(root, decl.Name) {
				edits = append(edits, g.replace(id, decl.Name+"Renamed"))
			}
		}
		g.add("rename-local", false, decl.Pos(), edits...)
	}
}

// declaredLocals: params, results and names declared in the body, in order.
func declaredLocals(ft *ast.FuncType, body *ast.BlockStmt, recv *ast.FieldList) []*ast.Ident {
	var order []*ast.Ident
	seen := map[string]bool{}
	declare := func(ids ...ast.Expr) {
		for _, e := range ids {
			if id, ok := e.(*ast.Ident); ok && !seen[id.Name] && id.Name != "_" {
				seen[id.Name] = true
				order = append(order, id)
			}
		}
	}
	declare(fieldNames(recv, ft.Params, ft.Results)...)
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				declare(x.Lhs...)
			}
		case *ast.ValueSpec:
			for _, id := range x.Names {
				declare(id)
			}
		case *ast.RangeStmt:
			if x.Tok == token.DEFINE {
				declare(x.Key, x.Value)
			}
		}
		return true
	})
	return order
}

func fieldNames(lists ...*ast.FieldList) []ast.Expr {
	var out []ast.Expr
	for _, fl := range lists {
		if fl == nil {
			continue
		}
		for _, f := range fl.List {
			for _, n := range f.Names {
				out = append(out, n)
			}
		}
	}
	return out
}

// uses finds identifiers named name, except a field or method after a dot.
func uses(root ast.Node, name string) []*ast.Ident {
	var out []*ast.Ident
	ast.Inspect(root, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			out = append(out, uses(x.X, name)...)
			return false
		case *ast.Ident:
			if x.Name == name {
				out = append(out, x)
			}
		}
		return true
	})
	return out
}

func resultCount(ft *ast.FuncType) int {
	n := 0
	for _, f := range ft.Results.List {
		n += max(len(f.Names), 1)
	}
	return n
}

func lastIsError(ft *ast.FuncType) bool {
	l := ft.Results.List[len(ft.Results.List)-1]
	id, ok := l.Type.(*ast.Ident)
	return ok && id.Name == "error"
}

func stmtList(n ast.Node) []ast.Stmt {
	switch x := n.(type) {
	case *ast.BlockStmt:
		return x.List
	case *ast.CaseClause:
		return x.Body
	case *ast.CommClause:
		return x.Body
	}
	return nil
}

// equivalent: the corpus marks statements whose deletion can't change a
// failure path, though they look like they would, with a comment above.
func (g *gen) equivalent(st ast.Stmt) bool {
	lines := strings.Split(string(g.src), "\n")
	line := g.line(st.Pos())
	return line >= 2 && strings.Contains(lines[line-2], "mutation: equivalent")
}

// onFailurePath: does the node return, jump, panic, recover, record a handled
// error, or call something that returns an error? A success return at the very end of a
// function is the implicit one, so deleting it changes nothing.
func onFailurePath(n ast.Node, tail bool, ft *ast.FuncType) bool {
	if r, ok := n.(*ast.ReturnStmt); ok && tail {
		return !successReturn(r, ft)
	}
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.DeferStmt:
			if lit, ok := x.Call.Fun.(*ast.FuncLit); ok && slices.Contains(callNames(lit.Body), "recover") {
				found = true
			}
		case *ast.ReturnStmt, *ast.BranchStmt:
			found = true
		case *ast.CallExpr:
			switch callName(x) {
			case "panic", "recover", "Exit", "Fatalf", "MustLoad", "fetch", "store", "Save", "each", "WarnContext":
				found = true
			}
		}
		return !found
	})
	return found
}

func callNames(n ast.Node) []string {
	var names []string
	ast.Inspect(n, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			names = append(names, callName(c))
		}
		return true
	})
	return names
}

func successReturn(r *ast.ReturnStmt, ft *ast.FuncType) bool {
	if ft.Results == nil || !lastIsError(ft) || len(r.Results) == 0 {
		return true
	}
	id, ok := r.Results[len(r.Results)-1].(*ast.Ident)
	return ok && id.Name == "nil"
}

func callName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

func describe(r review.Review) string {
	var b strings.Builder
	for _, f := range r.Functions {
		fmt.Fprintf(&b, "  %s %s reordered=%v returns=%v\n", f.ID, f.Status, f.Reordered, f.Returns != nil)
		for _, i := range f.Added {
			fmt.Fprintf(&b, "    + %s %s [%s]\n", i.Kind, i.Label, i.Context)
		}
		for _, i := range f.Removed {
			fmt.Fprintf(&b, "    - %s %s [%s]\n", i.Kind, i.Label, i.Context)
		}
		for _, c := range f.Changed {
			fmt.Fprintf(&b, "    ~ %s %s [%s] from [%s]\n", c.Kind, c.Label, c.Context, c.From.Context)
		}
	}
	for _, n := range r.NewFindings {
		fmt.Fprintf(&b, "  finding %s %s\n", n.Rule, n.Message)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "  note %s\n", n)
	}
	return b.String()
}
