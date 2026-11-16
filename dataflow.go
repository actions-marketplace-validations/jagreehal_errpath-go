package errpath

import (
	"go/ast"
	"go/token"
	"maps"
)

// What each error variable holds as the walker goes, so `return v, err` and a
// bare return with a named error result say whether they fail. Scopes follow
// Go's: `:=` in an inner block shadows, `=` assigns the outer variable. Where
// branches join, a variable keeps its state only if every arm agrees.

type stateKind int

const (
	stUnknown stateKind = iota // may or may not be nil
	stNil
	stNonNil // some error, not named
	stNew    // a named error (sentinel or type)
	stCallee // whatever a call returned
)

type errState struct {
	kind stateKind
	name string        // stNew
	call *ast.CallExpr // stCallee
}

type varState struct {
	st    errState
	depth int // block depth it was declared at
}

// scoped runs f as a new block: names declared inside go away after it.
func (c *fnCtx) scoped(f func()) {
	c.depth++
	outer := maps.Clone(c.vars)
	f()
	for name, v := range c.vars {
		if v.depth == c.depth {
			if old, ok := outer[name]; ok {
				c.vars[name] = old
			} else {
				delete(c.vars, name)
			}
		}
	}
	c.depth--
}

func (c *fnCtx) block(list []ast.Stmt) []Node {
	var nodes []Node
	c.scoped(func() { nodes = c.stmts(list) })
	return nodes
}

// setVar records an assignment. define is `:=` or var: a new variable unless
// one of that name was already declared in this same block.
func (c *fnCtx) setVar(name string, st errState, define bool) {
	if name == "_" || name == "" {
		return
	}
	v, ok := c.vars[name]
	switch {
	case define && (!ok || v.depth != c.depth):
		v.depth = c.depth
	case !ok:
		v.depth = 0 // an outer or package variable
	}
	v.st = st
	c.vars[name] = v
}

func (c *fnCtx) tracked(e ast.Expr) (string, errState, bool) {
	id, ok := unparen(e).(*ast.Ident)
	if !ok {
		return "", errState{}, false
	}
	v, ok := c.vars[id.Name]
	return id.Name, v.st, ok
}

// stateOf is what assigning e to an error variable stores.
func (c *fnCtx) stateOf(e ast.Expr) errState {
	if _, st, ok := c.tracked(e); ok {
		return st
	}
	cl := c.classify(e, "")
	switch cl.kind {
	case errNil:
		return errState{kind: stNil}
	case errNew:
		if cl.name == "error" {
			return errState{kind: stNonNil}
		}
		return errState{kind: stNew, name: cl.name}
	case errWrap:
		return errState{kind: stNonNil}
	case errCallee:
		return errState{kind: stCallee, call: cl.call}
	}
	return errState{}
}

// arms runs each arm from the same state, then joins the arms that fall through.
func (c *fnCtx) arms(fs ...func() []Node) [][]Node {
	start := maps.Clone(c.vars)
	out := make([][]Node, len(fs))
	var ends []map[string]varState
	for i, f := range fs {
		c.vars = maps.Clone(start)
		out[i] = f()
		if !endsWithExit(out[i]) {
			ends = append(ends, c.vars)
		}
	}
	c.vars = start
	if len(ends) == 0 {
		return out
	}
	for name, v := range start {
		st := ends[0][name].st
		for _, e := range ends[1:] {
			if e[name].st != st {
				st = errState{}
				break
			}
		}
		v.st = st
		c.vars[name] = v
	}
	return out
}

// refine is what a condition tells each arm about a variable: in
// `if err != nil` the yes arm knows err is set, and the no arm that it's nil.
func (c *fnCtx) refine(cond ast.Expr) (name string, yes, no errState, ok bool) {
	bin, isBin := unparen(cond).(*ast.BinaryExpr)
	if !isBin || (bin.Op != token.NEQ && bin.Op != token.EQL) {
		return "", errState{}, errState{}, false
	}
	x, y := bin.X, bin.Y
	if id, isID := unparen(x).(*ast.Ident); isID && id.Name == "nil" {
		x, y = y, x
	}
	if id, isID := unparen(y).(*ast.Ident); !isID || id.Name != "nil" {
		return "", errState{}, errState{}, false
	}
	name, st, tracked := c.tracked(x)
	if !tracked {
		return "", errState{}, errState{}, false
	}
	set := st
	if st.kind == stUnknown || st.kind == stNil || st.kind == stCallee {
		set = errState{kind: stNonNil}
	}
	if bin.Op == token.NEQ {
		return name, set, errState{kind: stNil}, true
	}
	return name, errState{kind: stNil}, set, true
}

// fromState is what returning a variable in this state does.
func (c *fnCtx) fromState(name string, st errState, line int) []Node {
	switch st.kind {
	case stNil:
		return []Node{Succeed()}
	case stNew:
		return []Node{Fail(st.name, line)}
	case stNonNil:
		return []Node{Fail(or(c.enclosing, "error"), line)}
	case stCallee:
		return []Node{c.calleeStep(st.call, line), Succeed()}
	}
	return []Node{BranchNode("if "+name+" != nil", []Arm{
		{Label: "yes", Body: []Node{Fail(or(c.enclosing, "error"), line)}},
		{Label: "no", Body: []Node{Succeed()}},
	})}
}

// findCheck finds the `if err != nil` that checks an error assigned at
// list[i]. Nothing between may reassign it, nor leave (return, jump, defer)
// without looking at it: on that path the error would be dropped unchecked.
func findCheck(list []ast.Stmt, i int, errName string) int {
	for j := i + 1; j < len(list); j++ {
		if ifs, ok := list[j].(*ast.IfStmt); ok && ifs.Init == nil && isErrNilCheck(ifs.Cond, errName) {
			return j
		}
		if assigns(list[j], errName) || (leaves(list[j]) && !mentions(list[j], errName)) {
			return -1
		}
	}
	return -1
}

// leaves: can the statement end the function or jump, or defer work to the end?
func leaves(st ast.Stmt) bool {
	found := false
	ast.Inspect(st, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt, *ast.BranchStmt, *ast.DeferStmt:
			found = true
		}
		return !found
	})
	return found
}

// mentions: does the statement read or write name here and now? A closure
// that does runs later, if at all.
func mentions(st ast.Stmt, name string) bool {
	found := false
	ast.Inspect(st, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.Ident:
			found = found || x.Name == name
		}
		return !found
	})
	return found
}

// deadErrorTest: errors.Is or errors.As on a variable known to be nil is
// always false.
func (c *fnCtx) deadErrorTest(cond ast.Expr) bool {
	call, ok := unparen(cond).(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return false
	}
	if _, display, _ := c.resolveCall(call); display != "errors.Is" && display != "errors.As" {
		return false
	}
	_, st, tracked := c.tracked(call.Args[0])
	return tracked && st.kind == stNil
}

// assigns reports whether st assigns name anywhere outside function literals.
func assigns(st ast.Stmt, name string) bool {
	found := false
	ast.Inspect(st, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.AssignStmt:
			for _, l := range x.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == name {
					found = true
				}
			}
		case *ast.ValueSpec:
			for _, id := range x.Names {
				if id.Name == name {
					found = true
				}
			}
		}
		return !found
	})
	return found
}
