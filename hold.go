package errpath

import (
	"go/ast"
	"go/token"
	"maps"
	"strings"
)

// Does a module function hand back the error it's given? holdsParam walks
// the function forwards, keeping the variables that hold the parameter's
// original error at each point, in order: `saved = err` makes saved a holder
// from there on, `err = nil` ends err's. Where paths meet, a variable holds
// only if it does on all of them. A return passes when its error holds a
// holder; a path passes too once it records or stops.

// hstate is what's known at one point on the paths reaching it.
type hstate struct {
	live bool            // some path reaches here
	done bool            // every path here recorded or stopped
	held map[string]bool // variables holding the original error on every path
}

// meet joins two sets of paths.
func meet(a, b hstate) hstate {
	switch {
	case !a.live:
		return b
	case !b.live:
		return a
	case a.done:
		return b
	case b.done:
		return a
	}
	both := map[string]bool{}
	for k := range a.held {
		if b.held[k] {
			both[k] = true
		}
	}
	return hstate{live: true, held: both}
}

func sameState(a, b hstate) bool {
	return a.live == b.live && a.done == b.done && maps.Equal(a.held, b.held)
}

// target is where break and continue land, collecting the states they bring.
type target struct {
	label     string
	brk, cont hstate
	loop      bool
}

type holdWalk struct {
	c       *fnCtx
	set     bool // the parameter is known to hold an error, so holders aren't nil
	ok      bool
	targets []*target
	label   string            // a label waiting for the statement it names
	fall    hstate            // what a fallthrough carries to the next clause
	escaped map[string]bool   // given out by address, so never a holder
	shares  map[string]string // variables that may share storage, by group
	plain   string            // the parameter, when a plain error no write can reach
}

// holdsParam: does every path through a module function return an error
// holding the parameter, record, or stop? set says the caller passes an error
// known to be set, so a nil check on a holder takes one arm.
func (w *world) holdsParam(sig *fnSig, param string, set bool) bool {
	key := recordKey{sig.decl, "holds " + param}
	if set {
		key.ask += " set"
	}
	if done, ok := w.records[key]; ok || sig.decl.Body == nil || param == "_" {
		return done
	}
	w.records[key] = false
	c := newFnCtx(w, sig.file, nil, sig, sig.decl.Recv, sig.decl.Type, sig.decl.Body)
	c.byScope = true
	h := &holdWalk{c: c, set: set, ok: !c.deferClobbers(sig.decl.Body), escaped: addressed(sig.decl.Body), shares: sharing(sig.decl.Body)}
	maps.Copy(h.escaped, c.closureWrites(sig.decl.Body, h.shares))
	if p, ok := paramNamed(sig.decl.Type, param); ok && isErrorType(p.typ) {
		h.plain = param
	}
	if h.ok && !h.escaped[param] {
		h.list(sig.decl.Body.List, hstate{live: true, held: map[string]bool{param: true}})
	} else {
		h.ok = false
	}
	w.records[key] = h.ok
	return h.ok
}

// deferClobbers: can something running after the return change the named
// error result? A deferred call registered by this function (not one inside
// a closure) that sets it to something not holding it (`defer func() { out
// = nil }()`) does, after every return. So may anything given its address
// (`defer reset(&out)`). Wrapping it (`out = fmt.Errorf("...: %w", out)`)
// keeps it; a deferred closure's own `out :=` is another variable.
func (c *fnCtx) deferClobbers(body *ast.BlockStmt) bool {
	result := c.resultObj()
	if result == nil {
		return false
	}
	same := func(e ast.Expr) bool {
		id, ok := unparen(e).(*ast.Ident)
		return ok && id.Name == result.Name && (id.Obj == nil || id.Obj == result.Obj)
	}
	clobbers := false
	ast.Inspect(body, func(n ast.Node) bool {
		if u, ok := n.(*ast.UnaryExpr); ok && u.Op == token.AND && same(u.X) {
			clobbers = true
		}
		return !clobbers
	})
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false // a defer in here runs when the closure returns, if ever
		case *ast.DeferStmt:
			ast.Inspect(x.Call, func(n ast.Node) bool {
				st, _ := n.(ast.Stmt)
				lhs, rhs := assignParts(st)
				for i, l := range lhs {
					if same(l) {
						keeps := len(lhs) == len(rhs) && c.retains(rhs[i], map[string]bool{result.Name: true})
						clobbers = clobbers || !keeps
					}
				}
				return !clobbers
			})
			return false
		}
		return !clobbers
	})
	return clobbers
}

// resultObj is the name of the named error result, if any; its parser
// object tells it from a shadowing local of the same name.
func (c *fnCtx) resultObj() *ast.Ident {
	if c.namedErr == "" || c.sig.decl == nil || c.sig.decl.Type.Results == nil {
		return nil
	}
	for _, f := range c.sig.decl.Type.Results.List {
		for _, n := range f.Names {
			if n.Name == c.namedErr {
				return n
			}
		}
	}
	return nil
}

// addressed: the names whose address is taken anywhere in the nodes,
// closures included. A write through the pointer (`*p = nil`) can't be
// followed, so nothing is known about them.
func addressed(nodes ...ast.Node) map[string]bool {
	out := map[string]bool{}
	for _, n := range nodes {
		ast.Inspect(n, func(n ast.Node) bool {
			if u, ok := n.(*ast.UnaryExpr); ok && u.Op == token.AND {
				if id, isID := unparen(u.X).(*ast.Ident); isID {
					out[id.Name] = true
				}
			}
			return true
		})
	}
	return out
}

// facts: what a call made from here knows is set. When the parameter is,
// so is every holder, which a nested helper's nil guard needs.
func (h *holdWalk) facts(in hstate) {
	h.c.nonNil = nil
	if h.set {
		h.c.nonNil = in.held
	}
}

func (h *holdWalk) list(list []ast.Stmt, in hstate) hstate {
	for _, st := range list {
		in = h.stmt(st, in)
	}
	return in
}

func (h *holdWalk) records(in hstate, nodes ...ast.Node) bool {
	h.facts(in)
	for _, n := range nodes {
		if yes, _ := h.c.recordsIn(n); yes {
			return true
		}
	}
	return false
}

var dead = hstate{}

func (h *holdWalk) stmt(st ast.Stmt, in hstate) hstate { //nolint:gocyclo // one case per statement kind; splitting would only scatter them
	if !in.live || in.done {
		return in
	}
	label := h.label
	h.label = ""
	switch s := st.(type) {
	case *ast.ReturnStmt:
		in = h.mutations(s, in) // shortcut: as if every write ran before the result is read
		if !h.records(in, s) && !h.returnHolds(s, in.held) {
			h.ok = false
		}
		return dead
	case *ast.BranchStmt:
		h.jump(s, in)
		return dead
	case *ast.BlockStmt:
		return h.list(s.List, in)
	case *ast.LabeledStmt:
		h.label = s.Label.Name
		return h.stmt(s.Stmt, in)
	case *ast.IfStmt:
		in = h.simple(s.Init, in)
		if h.records(in, s.Cond) {
			return hstate{live: true, done: true}
		}
		in = h.mutations(s.Cond, in)
		known := ""
		if h.set {
			known = h.c.knownNil(s.Cond, in.held)
		}
		yes, no := dead, in
		if known != "nil" {
			yes = h.list(s.Body.List, in)
		}
		if s.Else != nil && known != "set" {
			no = h.stmt(s.Else, in)
		} else if known == "set" {
			no = dead
		}
		return meet(yes, no)
	case *ast.SwitchStmt:
		in = h.simple(s.Init, in)
		if h.records(in, s.Tag) {
			return hstate{live: true, done: true}
		}
		in = h.mutations(s.Tag, in)
		return h.clauses(label, s.Body.List, in, false)
	case *ast.TypeSwitchStmt:
		in = h.simple(s.Init, in)
		in = h.simple(s.Assign, in)
		return h.clauses(label, s.Body.List, in, false)
	case *ast.SelectStmt:
		if h.records(in, selectOperands(s)...) {
			return hstate{live: true, done: true}
		}
		return h.clauses(label, s.Body.List, in, true)
	case *ast.ForStmt:
		return h.loop(label, s.Init, s.Cond, s.Post, s.Body, in, s.Cond == nil)
	case *ast.RangeStmt:
		if h.records(in, s.X) {
			return hstate{live: true, done: true}
		}
		in = h.mutations(s.X, in)
		in = h.assign([]ast.Expr{s.Key, s.Value}, nil, in)
		return h.loop(label, nil, nil, nil, s.Body, in, false)
	case *ast.ExprStmt:
		if call, ok := unparen(s.X).(*ast.CallExpr); ok {
			if n, isPanic := h.c.panicCall(call, true, s); isPanic && n.Terminal {
				return hstate{live: true, done: true}
			}
		}
	}
	return h.simple(st, in)
}

// simple: an assignment or any other statement that runs straight through.
func (h *holdWalk) simple(st ast.Stmt, in hstate) hstate {
	if st == nil || !in.live || in.done {
		return in
	}
	if h.records(in, st) {
		return hstate{live: true, done: true}
	}
	in = h.mutations(st, in)
	switch s := st.(type) {
	case *ast.AssignStmt:
		return h.assign(s.Lhs, s.Rhs, in)
	case *ast.DeclStmt:
		lhs, rhs := assignParts(s)
		return h.assign(lhs, rhs, in)
	}
	return in
}

// assign: each variable on the left holds the error from here on when its
// value does, judged by what held before the statement. Writing a holding
// value into a field or element makes the container hold it; writing anything
// else there may clear it. A value errpath
// can't pair with a variable (one call, several results) holds nothing.
func (h *holdWalk) assign(lhs, rhs []ast.Expr, in hstate) hstate {
	h.facts(in)
	out := in
	out.held = maps.Clone(in.held)
	for i, l := range lhs {
		id, whole := l.(*ast.Ident)
		if !whole {
			id = rootVar(l)
		}
		if id == nil || id.Name == "_" {
			continue
		}
		holds := len(lhs) == len(rhs) && !h.escaped[id.Name] && h.c.retains(rhs[i], in.held)
		if !whole && !holds {
			if h.basicField(l, rhs, i) && in.held[h.c.varKey(id)] {
				holds = true // `out.Op = "load"`: a string field can't hold the error
			} else {
				h.clobber(out.held, id.Name)
			}
		}
		if holds {
			out.held[h.c.varKey(id)] = true
		} else {
			delete(out.held, h.c.varKey(id))
		}
	}
	return out
}

// mutations: a call that may write through what it's given clears what
// that held, and what shares its storage (see mutatedArgs).
func (h *holdWalk) mutations(n ast.Node, in hstate) hstate {
	if n == nil || !in.live || in.done {
		return in
	}
	var hits []mutation
	ast.Inspect(n, func(n ast.Node) bool {
		if _, lit := n.(*ast.FuncLit); lit {
			return false // its writes are in escaped
		}
		if call, ok := n.(*ast.CallExpr); ok {
			hits = append(hits, h.c.mutatedArgs(call)...)
		}
		return true
	})
	if len(hits) == 0 {
		return in
	}
	out := in
	out.held = maps.Clone(in.held)
	for _, m := range hits {
		keep := out.held[h.c.varKey(m.v)]
		h.clobber(out.held, m.v.Name)
		if !m.self && keep {
			out.held[h.c.varKey(m.v)] = true
		}
	}
	return out
}

// mutation: a call may write into v's storage, or with self false only
// into storage beyond v's length that a variable sharing it may see
// (`append(errs, x)` writes past len(errs)).
type mutation struct {
	v    *ast.Ident
	self bool
}

// Standard packages whose functions only read what they're given, but for
// the ones named in stdWriters.
var stdReaders = map[string]bool{
	"errors": true, "fmt": true, "log": true, "log/slog": true, "strings": true,
	"strconv": true, "slices": true, "maps": true, "sort": true, "context": true,
	"time": true, "math": true, "unicode": true, "unicode/utf8": true,
}

var stdWriters = map[string]bool{
	"slices.Delete": true, "slices.DeleteFunc": true, "slices.Compact": true,
	"slices.CompactFunc": true, "slices.Replace": true, "maps.Copy": true,
	"maps.DeleteFunc": true, "fmt.Sscan": true, "fmt.Sscanf": true,
	"fmt.Sscanln": true, "fmt.Scan": true, "fmt.Scanf": true, "fmt.Scanln": true,
	"fmt.Fscan": true, "fmt.Fscanf": true, "fmt.Fscanln": true,
}

// mutatedArgs: the variables a call may write through. Builtins are known
// (clear, copy, delete, append); a function in this module is followed;
// a reading standard package writes nothing; anything else, an unknown
// method included, may write through every argument and its receiver.
func (c *fnCtx) mutatedArgs(call *ast.CallExpr) []mutation { //nolint:gocyclo // one case per kind of callee
	all := func(self bool, args ...ast.Expr) []mutation {
		var out []mutation
		for _, a := range args {
			for _, id := range identsIn(a) {
				out = append(out, mutation{id, self})
			}
		}
		return out
	}
	var recv ast.Expr
	fun := unparen(call.Fun)
	switch g := fun.(type) { // slices.Delete[[]error](...)
	case *ast.IndexExpr:
		fun = g.X
	case *ast.IndexListExpr:
		fun = g.X
	}
	switch f := fun.(type) {
	case *ast.Ident:
		if c.isLocal(f.Name) {
			return all(true, call.Args...)
		}
		if c.pf.pkg.types[f.Name] != nil || basicTypes[f.Name] || f.Name == "error" || f.Name == "any" {
			return nil // a conversion
		}
		if c.pf.pkg.funcs[f.Name] == nil {
			switch {
			case len(call.Args) == 0:
				return nil
			case f.Name == "clear" || f.Name == "copy" || f.Name == "delete":
				return all(true, call.Args[0])
			case f.Name == "append":
				_, sliced := unparen(call.Args[0]).(*ast.SliceExpr)
				return all(sliced, call.Args[0])
			}
			return nil // len, cap, min, max, print, panic, close
		}
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok && !c.isLocal(id.Name) {
			if imp, isImport := c.pf.imports[id.Name]; isImport && imp.pkg == nil {
				switch {
				case stdWriters[imp.path+"."+f.Sel.Name]:
					return all(true, call.Args...)
				case stdReaders[imp.path]:
					return nil
				}
				return all(true, call.Args...)
			}
		}
		recv = f.X
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType, *ast.StructType:
		return nil // a conversion
	}
	sig, _, conf := c.resolveCall(call)
	if sig == nil || conf != ConfidenceCertain || sig.decl == nil || sig.decl.Body == nil {
		if recv != nil {
			return all(true, append([]ast.Expr{recv}, call.Args...)...)
		}
		return all(true, call.Args...)
	}
	var out []mutation
	for i, a := range call.Args {
		if p, ok := paramFor(sig.decl.Type, i); ok && c.w.mutatesParam(sig, p.name) {
			out = append(out, all(true, a)...)
		}
	}
	if recv != nil && sig.decl.Recv != nil && len(sig.decl.Recv.List[0].Names) > 0 {
		if c.w.mutatesParam(sig, sig.decl.Recv.List[0].Names[0].Name) {
			out = append(out, all(true, recv)...)
		}
	}
	return out
}

// identsIn: the variables an expression names, as namesIn, as identifiers.
func identsIn(e ast.Expr) []*ast.Ident {
	var out []*ast.Ident
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			for _, a := range x.Args {
				out = append(out, identsIn(a)...)
			}
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				out = append(out, identsIn(sel.X)...)
			}
			return false
		case *ast.SelectorExpr:
			out = append(out, identsIn(x.X)...)
			return false
		case *ast.KeyValueExpr:
			out = append(out, identsIn(x.Value)...)
			return false
		case *ast.Ident:
			if x.Name != "nil" && x.Name != "_" {
				out = append(out, x)
			}
		}
		return true
	})
	return out
}

// mutatesParam: may a module function write into what its parameter (or
// receiver) refers to, so its caller sees it? A field or element write to
// it or a local sharing its storage, or a call that may write through one,
// anywhere, closures included. A cycle counts as yes.
func (w *world) mutatesParam(sig *fnSig, name string) bool {
	key := recordKey{sig.decl, "mutates " + name}
	if done, ok := w.records[key]; ok || name == "_" {
		return done
	}
	w.records[key] = true
	c := newFnCtx(w, sig.file, nil, sig, sig.decl.Recv, sig.decl.Type, sig.decl.Body)
	group := sharing(sig.decl.Body)
	member := func(n string) bool { return n == name || (group[n] != "" && group[n] == group[name]) }
	yes := false
	ast.Inspect(sig.decl.Body, func(n ast.Node) bool {
		for _, v := range c.storageWrites(n) {
			if member(v.v.Name) && (v.self || v.v.Name != name) {
				yes = true
			}
		}
		return !yes
	})
	w.records[key] = yes
	return yes
}

// storageWrites: the variables one node writes into: a field or element
// assignment (`p.Err = nil`, `errs[1] = nil`, `p[0]++`) or a call that may
// write through its arguments.
func (c *fnCtx) storageWrites(n ast.Node) []mutation {
	var lhs []ast.Expr
	switch x := n.(type) {
	case *ast.AssignStmt:
		lhs = x.Lhs
	case *ast.IncDecStmt:
		lhs = []ast.Expr{x.X}
	case *ast.CallExpr:
		return c.mutatedArgs(x)
	}
	var out []mutation
	for _, l := range lhs {
		if _, whole := l.(*ast.Ident); !whole {
			if root := rootVar(l); root != nil {
				out = append(out, mutation{root, true})
			}
		}
	}
	return out
}

// closureWrites: the variables a function literal in body may change, by
// assigning them or writing into their storage: a closure may run at any
// point (`sort.Slice(errs, func(i, j int) bool { errs[i] = nil ... })`),
// so nothing is known about them. A storage write reaches every variable
// sharing that storage.
func (c *fnCtx) closureWrites(body *ast.BlockStmt, group map[string]string) map[string]bool {
	out := map[string]bool{}
	var stored []string
	ast.Inspect(body, func(n ast.Node) bool {
		lit, ok := n.(*ast.FuncLit)
		if !ok {
			return true
		}
		ast.Inspect(lit.Body, func(n ast.Node) bool {
			if a, isAssign := n.(*ast.AssignStmt); isAssign {
				for _, l := range a.Lhs {
					if id, whole := l.(*ast.Ident); whole {
						out[id.Name] = true
					}
				}
			}
			for _, m := range c.storageWrites(n) {
				stored = append(stored, m.v.Name)
			}
			return true
		})
		return false
	})
	for _, s := range stored {
		out[s] = true
		for n, g := range group {
			if g == group[s] && group[s] != "" {
				out[n] = true
			}
		}
	}
	return out
}

// basicField: is lhs = rhs[i] a literal written to a field of a basic type
// (`out.Op = "load"`)? Not an `any` field, which a literal can replace an
// error in, nor an element.
func (h *holdWalk) basicField(lhs ast.Expr, rhs []ast.Expr, i int) bool {
	sel, ok := lhs.(*ast.SelectorExpr)
	if !ok || i >= len(rhs) {
		return false
	}
	if _, lit := unparen(rhs[i]).(*ast.BasicLit); !lit {
		return false
	}
	return basicTypes[h.c.w.field(h.c.typeOf(sel.X), sel.Sel.Name).name]
}

var basicTypes = map[string]bool{
	"bool": true, "string": true, "byte": true, "rune": true, "uintptr": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"float32": true, "float64": true, "complex64": true, "complex128": true,
}

// clobber: a write into name's storage may have cleared the error from it
// and from every variable that may share that storage (`alias := errs`).
// The parameter itself survives when it's a plain error: an interface value
// is copied, not shared.
func (h *holdWalk) clobber(held map[string]bool, name string) {
	for k := range held {
		n, _, _ := strings.Cut(k, "@")
		if n != h.plain && (n == name || (h.shares[n] != "" && h.shares[n] == h.shares[name])) {
			delete(held, k)
		}
	}
}

// sharing groups the variables that may share storage: every variable
// assigned, anywhere in the body, from an expression naming another
// (`alias := errs`, `p := out`, `ws := &W{Errs: errs}`). By name, ignoring
// order and scope; cautious, so a write may clear more than it does.
func sharing(body *ast.BlockStmt) map[string]string {
	rep := map[string]string{}
	var find func(string) string
	find = func(n string) string {
		if r, ok := rep[n]; ok && r != n {
			r = find(r)
			rep[n] = r
			return r
		}
		rep[n] = n
		return n
	}
	ast.Inspect(body, func(n ast.Node) bool {
		st, ok := n.(ast.Stmt)
		if !ok {
			return true
		}
		lhs, rhs := assignParts(st)
		for _, l := range lhs {
			root := rootVar(l)
			if root == nil || root.Name == "_" {
				continue
			}
			for _, r := range rhs {
				for _, name := range namesIn(r) {
					rep[find(name)] = find(root.Name)
				}
			}
		}
		return true
	})
	for n := range rep {
		rep[n] = find(n)
	}
	return rep
}

// namesIn: the variables an expression names, not functions it calls or
// fields it selects.
func namesIn(e ast.Expr) []string {
	var out []string
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			for _, a := range x.Args {
				out = append(out, namesIn(a)...)
			}
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				out = append(out, namesIn(sel.X)...) // a method may return its receiver
			}
			return false
		case *ast.SelectorExpr:
			out = append(out, namesIn(x.X)...)
			return false
		case *ast.KeyValueExpr:
			out = append(out, namesIn(x.Value)...)
			return false
		case *ast.Ident:
			if x.Name != "nil" && x.Name != "_" {
				out = append(out, x.Name)
			}
		}
		return true
	})
	return out
}

func paramNamed(ft *ast.FuncType, name string) (param, bool) {
	for _, f := range ft.Params.List {
		for _, n := range f.Names {
			if n.Name == name {
				return param{n.Name, f.Type}, true
			}
		}
	}
	return param{}, false
}

// rootVar is the variable a field, element or pointer write changes:
// errs for errs[1], out for out.Err or *out. Nil for anything else.
func rootVar(e ast.Expr) *ast.Ident {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			return x
		case *ast.ParenExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.SliceExpr:
			e = x.X
		case *ast.UnaryExpr:
			e = x.X
		case *ast.SelectorExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.TypeAssertExpr:
			e = x.X // out.(*Wrapped).Err: the dynamic value is shared
		default:
			return nil
		}
	}
}

// loop runs the body until what reaches its top stops changing. Each round
// can only lose holders, so it settles. A loop with a condition may not run
// at all; `for {}` leaves only by break.
func (h *holdWalk) loop(label string, init, cond ast.Node, post ast.Stmt, body *ast.BlockStmt, in hstate, forever bool) hstate {
	if initSt, ok := init.(ast.Stmt); ok {
		in = h.simple(initSt, in)
	}
	if cond != nil && h.records(in, cond) {
		return hstate{live: true, done: true}
	}
	t := &target{label: label, loop: true}
	h.targets = append(h.targets, t)
	defer func() { h.targets = h.targets[:len(h.targets)-1] }()
	top := in
	for {
		t.brk, t.cont = dead, dead
		entry := h.mutations(cond, top) // the condition runs before every round
		end := h.list(body.List, entry)
		end = h.simple(post, meet(end, t.cont))
		next := meet(in, end)
		if sameState(next, top) {
			break
		}
		top = next
	}
	if forever {
		return t.brk
	}
	return meet(h.mutations(cond, top), t.brk)
}

// clauses: case expressions run in order until one matches, so a clause
// starts after its own and every earlier one, and a default after all of
// them; a select evaluates every case on entry. A fallthrough brings the
// clause before; break and the end of a clause land after the statement.
// Without a default, no clause may run.
func (h *holdWalk) clauses(label string, list []ast.Stmt, in hstate, hasDefault bool) hstate {
	t := &target{label: label, brk: dead}
	h.targets = append(h.targets, t)
	defer func() { h.targets = h.targets[:len(h.targets)-1] }()
	evaluated := in
	entry := make([]hstate, len(list))
	for i, cl := range list {
		switch x := cl.(type) {
		case *ast.CaseClause:
			for _, e := range x.List {
				evaluated = h.mutations(e, evaluated)
			}
		case *ast.CommClause:
			for _, e := range commOperands(x.Comm) {
				evaluated = h.mutations(e, evaluated)
			}
		}
		entry[i] = evaluated
	}
	out := dead
	h.fall = dead
	for i, cl := range list {
		start := entry[i]
		var body []ast.Stmt
		switch x := cl.(type) {
		case *ast.CaseClause:
			hasDefault = hasDefault || x.List == nil
			if x.List == nil {
				start = evaluated
			}
			body = x.Body
		case *ast.CommClause:
			start = h.simple(x.Comm, evaluated)
			body = x.Body
		}
		start = meet(start, h.fall)
		h.fall = dead
		out = meet(out, h.list(body, start))
	}
	out = meet(out, t.brk)
	if !hasDefault {
		out = meet(out, evaluated)
	}
	return out
}

// commOperands: what a select evaluates for a case on entry, the channel
// and any value sent. A receive's assignment targets (`slot[i] = <-ch`) are
// evaluated only in the case selected.
func commOperands(comm ast.Stmt) []ast.Expr {
	switch c := comm.(type) {
	case *ast.SendStmt:
		return []ast.Expr{c.Chan, c.Value}
	case *ast.ExprStmt:
		return []ast.Expr{c.X}
	case *ast.AssignStmt:
		return c.Rhs
	}
	return nil
}

// selectOperands: what a select evaluates on entry, whichever case wins.
func selectOperands(s *ast.SelectStmt) []ast.Node {
	var out []ast.Node
	for _, cl := range s.Body.List {
		if cc, ok := cl.(*ast.CommClause); ok {
			for _, e := range commOperands(cc.Comm) {
				out = append(out, e)
			}
		}
	}
	return out
}

// jump sends the state to where a break, continue or fallthrough lands. A
// goto, or a jump errpath can't place, ends the path without handing back.
func (h *holdWalk) jump(s *ast.BranchStmt, in hstate) {
	if s.Tok == token.FALLTHROUGH {
		h.fall = in
		return
	}
	for i := len(h.targets) - 1; i >= 0; i-- {
		t := h.targets[i]
		named := s.Label != nil && t.label == s.Label.Name
		switch {
		case s.Tok == token.BREAK && (named || (s.Label == nil)):
			t.brk = meet(t.brk, in)
			return
		case s.Tok == token.CONTINUE && t.loop && (named || s.Label == nil):
			t.cont = meet(t.cont, in)
			return
		}
	}
	h.ok = false
}

// returnHolds: does the return's error result hold a holder? A bare return
// returns the named error result.
func (h *holdWalk) returnHolds(ret *ast.ReturnStmt, held map[string]bool) bool {
	c := h.c
	h.facts(hstate{held: held})
	if len(ret.Results) == 0 {
		return c.namedErr != "" && held[c.namedErr]
	}
	e, fwd := c.errExpr(ret)
	if fwd != nil {
		return c.callRetains(fwd, held)
	}
	return e != nil && c.retains(e, held)
}
