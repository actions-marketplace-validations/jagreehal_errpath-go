package errpath

import (
	"fmt"
	"go/ast"
	"go/token"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// no-trace: an error handled where it happens, rather than returned, leaves no
// trace unless the handler records it. A handler passes when every path through
// it records the error, hands an error on (returns, stores or sends it), or
// stops the program.
//
// Recording means something OpenTelemetry can see:
//   - a span method that records: RecordError, SetStatus, AddEvent
//   - log/slog, at any level, through the package or a *slog.Logger
//   - anything in autotel (github.com/jagreehal/autotel-go)
//   - a function in this module that does one of those
//
// The standard log package and fmt don't count: nothing carries them to a
// trace. Calls in function literals and go statements don't either, because
// nothing says they run.

var spanRecorders = map[string]bool{"RecordError": true, "SetStatus": true, "AddEvent": true}

func isSlogLevel(name string) bool {
	switch strings.TrimSuffix(name, "Context") {
	case "Debug", "Info", "Warn", "Error", "Log":
		return true
	}
	return name == "LogAttrs"
}

func isAutotel(path string) bool { return strings.HasPrefix(path, "github.com/jagreehal/autotel-go") }

var (
	slogLogger = typeRef{name: "Logger", path: "log/slog"}
	otelSpan   = typeRef{name: "Span", path: "go.opentelemetry.io/otel/trace"}
	otelTracer = typeRef{name: "Tracer", path: "go.opentelemetry.io/otel/trace"}
)

// isSpan: an OpenTelemetry or autotel span.
func isSpan(t typeRef) bool {
	return t.name == "Span" && (strings.HasPrefix(t.path, "go.opentelemetry.io/otel") || isAutotel(t.path))
}

var externalResults = map[string]typeRef{
	"log/slog.New":     slogLogger,
	"log/slog.Default": slogLogger,
	"log/slog.With":    slogLogger,
	"go.opentelemetry.io/otel/trace.SpanFromContext": otelSpan,
	"go.opentelemetry.io/otel.Tracer":                otelTracer,
}

// externalType: calls outside the tree whose result type errpath knows:
// loggers (`slog.Default()`, `autotel.Logger("app")`), tracers and spans.
func (c *fnCtx) externalType(call *ast.CallExpr) typeRef {
	sel, ok := unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return typeRef{}
	}
	if id, isID := sel.X.(*ast.Ident); isID && !c.isLocal(id.Name) {
		if imp, isImport := c.pf.imports[id.Name]; isImport {
			if isAutotel(imp.path) && sel.Sel.Name == "Logger" {
				return slogLogger
			}
			return externalResults[imp.path+"."+sel.Sel.Name]
		}
	}
	if (sel.Sel.Name == "With" || sel.Sel.Name == "WithGroup") && c.typeOf(sel.X) == slogLogger {
		return slogLogger
	}
	return typeRef{}
}

// startedSpan: the span `ctx, span := tracer.Start(ctx, "name")` or
// `autotel.Start(ctx, "name")` gives. A Start on a receiver of unknown type,
// such as a package-level tracer, counts when its name is a string literal,
// as a span name is.
func (c *fnCtx) startedSpan(call *ast.CallExpr) typeRef {
	sel, ok := unparen(call.Fun).(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Start" || len(call.Args) < 2 {
		return typeRef{}
	}
	if id, isID := sel.X.(*ast.Ident); isID && !c.isLocal(id.Name) {
		if imp, isImport := c.pf.imports[id.Name]; isImport {
			if isAutotel(imp.path) {
				return typeRef{name: "Span", path: imp.path}
			}
			return typeRef{}
		}
	}
	switch t := c.typeOf(sel.X); {
	case t == otelTracer:
		return otelSpan
	case t.name == "":
		if lit, isLit := call.Args[1].(*ast.BasicLit); isLit && lit.Kind == token.STRING {
			return otelSpan
		}
	}
	return typeRef{}
}

// errorStatus: SetStatus(codes.Error, ...) records, also through a constant
// (`const failure = codes.Error`); codes.Ok and Unset don't.
func (c *fnCtx) errorStatus(call *ast.CallExpr) bool {
	if len(call.Args) == 0 {
		return false
	}
	sel, ok := c.constValue(call.Args[0]).(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Error"
}

// bindConsts records a function's own constants.
func (c *fnCtx) bindConsts(d *ast.GenDecl) {
	if c.consts == nil {
		c.consts = map[string]ast.Expr{}
	}
	for _, spec := range d.Specs {
		vs := spec.(*ast.ValueSpec)
		if len(vs.Values) == len(vs.Names) {
			for i, n := range vs.Names {
				c.consts[n.Name] = vs.Values[i]
			}
		}
	}
}

// constValue follows a constant to the expression it names: a function's
// own, then the package's.
func (c *fnCtx) constValue(e ast.Expr) ast.Expr {
	for range 8 { // an alias of an alias; a cycle won't compile anyway
		id, ok := unparen(e).(*ast.Ident)
		if !ok {
			return e
		}
		if v, local := c.consts[id.Name]; local {
			e = v
			continue
		}
		if c.isLocal(id.Name) {
			return e
		}
		pv, found := c.pf.pkg.values[id.Name]
		if !found || !pv.isConst || pv.value == nil {
			return e
		}
		e = pv.value
	}
	return e
}

// valueType is the type of a package-level var: declared, or what its
// initialiser gives (`var tracer = otel.Tracer("app")`).
func (w *world) valueType(pkg *pkgInfo, name string) typeRef {
	v, ok := pkg.values[name]
	key := pkg.dir + "." + name
	if !ok || w.typing[key] {
		return typeRef{}
	}
	if v.typ != nil {
		return typeRefOf(v.file, v.typ)
	}
	if v.value == nil {
		return typeRef{}
	}
	w.typing[key] = true
	defer delete(w.typing, key)
	c := &fnCtx{w: w, pf: v.file, locals: map[string]typeRef{}, vars: map[string]varState{}}
	return c.typeOf(v.value)
}

// recordCall: does the call record? maybe is a logging method on a value of
// unknown type, which may or may not be a *slog.Logger.
func (c *fnCtx) recordCall(call *ast.CallExpr) (yes, maybe bool) {
	fun := unparen(call.Fun)
	if ix, ok := fun.(*ast.IndexExpr); ok {
		fun = ix.X
	}
	if sel, ok := fun.(*ast.SelectorExpr); ok {
		if yes, maybe, done := c.recordMethod(call, sel); done {
			return yes, maybe
		}
	}
	if sig, _, conf := c.resolveCall(call); sig != nil && conf == ConfidenceCertain && sig.decl != nil {
		return c.w.recorder(sig, c.setParams(sig, call)), false
	}
	return false, false
}

type param struct {
	name string // "_" when unnamed
	typ  ast.Expr
}

// paramFor is the parameter the call's i'th argument binds: every argument
// from the last parameter on goes to it when it's variadic (`errs ...error`).
func paramFor(ft *ast.FuncType, i int) (param, bool) {
	var params []param
	for _, f := range ft.Params.List {
		if len(f.Names) == 0 {
			params = append(params, param{"_", f.Type})
		}
		for _, n := range f.Names {
			params = append(params, param{n.Name, f.Type})
		}
	}
	if len(params) == 0 {
		return param{}, false
	}
	if last := params[len(params)-1]; i >= len(params)-1 {
		if _, variadic := last.typ.(*ast.Ellipsis); variadic {
			return last, true
		}
	}
	if i >= len(params) {
		return param{}, false
	}
	return params[i], true
}

// setParams: the callee's parameters that receive an error known to be set
// here, such as the handled error in report(err, nil). Sorted, as a cache key.
func (c *fnCtx) setParams(sig *fnSig, call *ast.CallExpr) []string {
	var set []string
	for i, a := range call.Args {
		p, ok := paramFor(sig.decl.Type, i)
		if !ok || p.name == "_" || !isErrorType(p.typ) || slices.Contains(set, p.name) {
			continue // a variadic ...error is a slice, never nil-checked as an error
		}
		if id, isID := unparen(a).(*ast.Ident); isID && c.nonNil[c.varKey(id)] {
			set = append(set, p.name)
		}
	}
	slices.Sort(set)
	return set
}

// stable keeps the names nothing in body assigns or takes the address of: a
// fact about a variable lasts only until it changes (`err = nil`, `reset(&err)`).
func stable(body []ast.Stmt, names ...string) map[string]bool {
	out := map[string]bool{}
	nodes := make([]ast.Node, len(body))
	for i, st := range body {
		nodes[i] = st
	}
	escaped := addressed(nodes...)
	for _, n := range names {
		if !escaped[n] && !slices.ContainsFunc(body, func(st ast.Stmt) bool { return assigns(st, n) }) {
			out[n] = true
		}
	}
	return out
}

// recordMethod judges pkg.F and x.M calls; done is false when only following
// the callee into this module can tell.
func (c *fnCtx) recordMethod(call *ast.CallExpr, sel *ast.SelectorExpr) (yes, maybe, done bool) {
	if id, isID := sel.X.(*ast.Ident); isID && !c.isLocal(id.Name) {
		if imp, isImport := c.pf.imports[id.Name]; isImport {
			if isAutotel(imp.path) || (imp.path == "log/slog" && isSlogLevel(sel.Sel.Name)) {
				return true, false, true
			}
			return false, false, imp.pkg == nil
		}
	}
	if spanRecorders[sel.Sel.Name] {
		if sel.Sel.Name == "SetStatus" && !c.errorStatus(call) {
			return false, false, true
		}
		return c.byReceiver(call, sel, isSpan) // maybe a span, maybe a Response
	}
	// A message comes first, so err.Error() isn't a log call.
	if !isSlogLevel(sel.Sel.Name) || len(call.Args) == 0 {
		return false, false, false
	}
	return c.byReceiver(call, sel, func(t typeRef) bool { return t == slogLogger })
}

// byReceiver: the method records when its receiver is the right type, may
// when errpath can't see the type, and otherwise only following it can tell.
func (c *fnCtx) byReceiver(call *ast.CallExpr, sel *ast.SelectorExpr, right func(typeRef) bool) (yes, maybe, done bool) {
	switch t := c.typeOf(sel.X); {
	case right(t):
		return true, false, true
	case t.name == "":
		if sig, _, conf := c.resolveCall(call); sig == nil || conf != ConfidenceCertain {
			return false, true, true
		}
	}
	return false, false, false
}

// recordKey caches what a function does for one question and one set of
// parameters known to hold an error.
type recordKey struct {
	decl *ast.FuncDecl
	ask  string
}

// recorder: does every path through the function record, or stop? set are
// the parameters given an error known to be set. Helpers
// (`s.fail(w, err)`) are common enough to follow; cycles count as no. A
// helper returning or storing an error doesn't count: its caller may drop it.
func (w *world) recorder(sig *fnSig, set []string) bool {
	key := recordKey{sig.decl, "records " + strings.Join(set, ",")}
	if done, ok := w.records[key]; ok || sig.decl.Body == nil { // no body: assembly
		return done
	}
	w.records[key] = false
	c := newFnCtx(w, sig.file, nil, sig, sig.decl.Recv, sig.decl.Type, sig.decl.Body)
	c.nonNil = stable(sig.decl.Body.List, set...)
	yes := c.tracedPaths(sig.decl.Body.List, pathRule{nonNil: c.nonNil})
	w.records[key] = yes
	return yes
}

// recordsIn: does n certainly record (yes), or record only if an unknown
// receiver is a logger (maybe)? Only the left of && and || always runs.
func (c *fnCtx) recordsIn(n ast.Node) (yes, maybe bool) {
	if n == nil {
		return false, false
	}
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit, *ast.GoStmt:
			return false
		case *ast.BinaryExpr:
			if x.Op == token.LAND || x.Op == token.LOR {
				y, m := c.recordsIn(x.X)
				yes, maybe = yes || y, maybe || m
				return false
			}
		case *ast.CallExpr:
			y, m := c.recordCall(x)
			yes, maybe = yes || y, maybe || m
		}
		return !yes
	})
	return yes, maybe
}

// pathRule says what lets a path through. errName is the handled error;
// handOn counts returning, storing or sending it; lenient counts
// maybe-records. nonNil are error variables known to hold an error here: the
// handled one, or a helper's error parameters, so `if err == nil { return }`
// takes only the other arm.
type pathRule struct {
	errName         string
	handOn, lenient bool
	nonNil          map[string]bool
}

// flow is where jumps inside the handler land: brk and cont for an
// unlabelled break and continue (nil when they would leave the handler),
// labels and conts for labelled ones, fall for fallthrough. Each holds whether every path from there on is traced.
type flow struct {
	brk, cont, fall *bool
	labels, conts   map[string]bool // where break L and continue L land
}

// tracedPaths: does every path through list record, hand the error on (when
// the rule allows), or stop? It walks backwards, so each statement is judged
// once from what follows it: linear, however many branches there are.
func (c *fnCtx) tracedPaths(list []ast.Stmt, r pathRule) bool {
	return c.seq(list, false, r, flow{})
}

// seq: is every path through list traced, given next says whether falling
// off its end is?
func (c *fnCtx) seq(list []ast.Stmt, next bool, r pathRule, f flow) bool {
	for i := len(list) - 1; i >= 0; i-- {
		next = c.stepTraced(list[i], next, r, f)
	}
	return next
}

func (c *fnCtx) records(r pathRule, nodes ...ast.Node) bool {
	for _, n := range nodes {
		if y, m := c.recordsIn(n); y || (r.lenient && m) {
			return true
		}
	}
	return false
}

func (c *fnCtx) stepTraced(st ast.Stmt, next bool, r pathRule, f flow) bool { //nolint:gocyclo // one case per statement kind; splitting would only scatter them
	switch s := st.(type) {
	case *ast.ReturnStmt:
		return c.records(r, s) || (r.handOn && c.returnsError(s))
	case *ast.BranchStmt:
		return jumpTraced(s, f)
	case *ast.BlockStmt:
		return c.seq(s.List, next, r, f)
	case *ast.LabeledStmt:
		inner := f
		inner.labels = maps.Clone(f.labels)
		if inner.labels == nil {
			inner.labels = map[string]bool{}
		}
		inner.labels[s.Label.Name] = next
		switch loop := s.Stmt.(type) {
		case *ast.ForStmt, *ast.RangeStmt:
			return c.loopTraced(loop, s.Label.Name, next, r, inner)
		}
		return c.stepTraced(s.Stmt, next, r, inner)
	case *ast.IfStmt:
		if c.records(r, s.Init, s.Cond) {
			return true
		}
		yes := c.seq(s.Body.List, next, r, f)
		no := next
		if s.Else != nil {
			no = c.stepTraced(s.Else, next, r, f)
		}
		switch c.knownNil(s.Cond, r.nonNil) {
		case "nil": // the condition is false here
			return no
		case "set":
			return yes
		}
		return yes && no
	case *ast.SwitchStmt:
		return c.records(r, s.Init, s.Tag) || c.clausesTraced(s.Body.List, next, r, f, false)
	case *ast.TypeSwitchStmt:
		return c.records(r, s.Init, s.Assign) || c.clausesTraced(s.Body.List, next, r, f, false)
	case *ast.SelectStmt:
		return c.records(r, selectOperands(s)...) || c.clausesTraced(s.Body.List, next, r, f, true)
	case *ast.ForStmt, *ast.RangeStmt:
		return c.loopTraced(s, "", next, r, f)
	case *ast.AssignStmt, *ast.DeclStmt, *ast.SendStmt:
		if r.handOn && c.keeps(st, r.errName) {
			return true
		}
	case *ast.ExprStmt:
		if call, ok := unparen(s.X).(*ast.CallExpr); ok {
			if n, isPanic := c.panicCall(call, true, s); isPanic && n.Terminal {
				return true
			}
		}
	}
	return c.records(r, st) || next
}

// loopTraced: a loop is traced when its head records, or when what follows
// it is and no way out of its body (a return, a jump past the loop) drops the
// error. break lands after the loop; continue and the end of the body go
// round again. A loop with a condition may not run at all; `for {}` leaves
// only through its body, and going round forever drops nothing.
func (c *fnCtx) loopTraced(st ast.Stmt, label string, next bool, r pathRule, f flow) bool {
	var head []ast.Node
	var body *ast.BlockStmt
	forever := false
	switch s := st.(type) {
	case *ast.ForStmt:
		head, body, forever = []ast.Node{s.Init, s.Cond}, s.Body, s.Cond == nil
	case *ast.RangeStmt:
		head, body = []ast.Node{s.X}, s.Body
	}
	if c.records(r, head...) {
		return true
	}
	again := next // round again, then out through the condition
	if forever {
		again = true
	}
	inner := flow{brk: &next, cont: &again, labels: f.labels, conts: maps.Clone(f.conts)}
	if label != "" {
		if inner.conts == nil {
			inner.conts = map[string]bool{}
		}
		inner.conts[label] = again
	}
	through := c.seq(body.List, again, r, inner)
	if forever {
		return through
	}
	return next && through
}

// jumpTraced: a break, continue or fallthrough lands somewhere in the
// handler, and is traced when that place is. Anything else leaves it.
func jumpTraced(s *ast.BranchStmt, f flow) bool {
	labelled := func(m map[string]bool) bool { return m[s.Label.Name] }
	switch {
	case s.Tok == token.BREAK && s.Label != nil:
		return labelled(f.labels)
	case s.Tok == token.BREAK && f.brk != nil:
		return *f.brk
	case s.Tok == token.CONTINUE && s.Label != nil:
		return labelled(f.conts)
	case s.Tok == token.CONTINUE && f.cont != nil:
		return *f.cont
	case s.Tok == token.FALLTHROUGH && f.fall != nil:
		return *f.fall
	}
	return false
}

// knownNil: "nil" when cond is `x == nil` and "set" when `x != nil`, for an
// x known to hold an error.
func (c *fnCtx) knownNil(cond ast.Expr, nonNil map[string]bool) string {
	bin, ok := unparen(cond).(*ast.BinaryExpr)
	if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
		return ""
	}
	x, y := unparen(bin.X), unparen(bin.Y)
	if id, isID := x.(*ast.Ident); isID && id.Name == "nil" {
		x, y = y, x
	}
	v, isV := x.(*ast.Ident)
	n, isN := y.(*ast.Ident)
	if !isV || !isN || n.Name != "nil" || !nonNil[c.varKey(v)] {
		return ""
	}
	if bin.Op == token.EQL {
		return "nil"
	}
	return "set"
}

// clausesTraced: every clause is traced, with what follows the statement as
// where its body ends and where a break lands. A fallthrough lands in the
// next clause. Without a default, skipping every clause is a path too; a
// select always runs one.
func (c *fnCtx) clausesTraced(clauses []ast.Stmt, next bool, r pathRule, f flow, hasDefault bool) bool {
	inner := flow{brk: &next, cont: f.cont, labels: f.labels, conts: f.conts}
	following, all := false, true
	for i := len(clauses) - 1; i >= 0; i-- {
		fall := following
		inner.fall = &fall
		var ok bool
		switch x := clauses[i].(type) {
		case *ast.CaseClause:
			hasDefault = hasDefault || x.List == nil
			ok = c.seq(x.Body, next, r, inner)
		case *ast.CommClause:
			ok = c.records(r, x.Comm) || (r.handOn && x.Comm != nil && c.keeps(x.Comm, r.errName)) || c.seq(x.Body, next, r, inner)
		}
		following, all = ok, all && ok
	}
	return all && (hasDefault || next)
}

// keeps: does the statement hand the error on to code that runs later, by
// storing it somewhere other than _ (`err = fmt.Errorf("...: %w", err)`,
// `errs = append(errs, err)`) or sending it?
func (c *fnCtx) keeps(st ast.Stmt, errName string) bool {
	held := map[string]bool{errName: true}
	if send, ok := st.(*ast.SendStmt); ok {
		return c.retains(send.Value, held)
	}
	lhs, values := assignParts(st)
	blank := func(e ast.Expr) bool { id, ok := e.(*ast.Ident); return ok && id.Name == "_" }
	if len(lhs) == len(values) {
		for i, v := range values {
			if !blank(lhs[i]) && c.retains(v, held) {
				return true
			}
		}
		return false
	}
	return len(values) == 1 && !slices.ContainsFunc(lhs, blank) && c.retains(values[0], held)
}

// varKey names a variable in a set of facts. Scoped, a local declared in an
// inner block (`saved := err`) is told apart from the one it shadows, by the
// parser's object for it; parameters and results keep their names.
func (c *fnCtx) varKey(id *ast.Ident) string {
	if !c.byScope || id.Obj == nil {
		return id.Name
	}
	if _, param := id.Obj.Decl.(*ast.Field); param {
		return id.Name
	}
	return fmt.Sprintf("%s@%p", id.Name, id.Obj)
}

// retains: does the value hold the error itself, not just its text? The
// error, its address, a composite literal holding it (`&PathError{Err: err}`),
// append, errors.Join, fmt.Errorf with %w, or a function in this module that
// holds on to it. Not fmt.Sprint(err), err.Error() or err != nil.
func (c *fnCtx) retains(e ast.Expr, held map[string]bool) bool {
	switch x := unparen(e).(type) {
	case *ast.Ident:
		return held[c.varKey(x)]
	case *ast.UnaryExpr:
		return x.Op == token.AND && c.retains(x.X, held)
	case *ast.CompositeLit:
		for _, el := range x.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				el = kv.Value
			}
			if c.retains(el, held) {
				return true
			}
		}
	case *ast.CallExpr:
		return c.callRetains(x, held)
	}
	return false
}

// callRetains: does the call's result hold an argument that holds the error?
func (c *fnCtx) callRetains(call *ast.CallExpr, held map[string]bool) bool {
	holds := func(int) bool { return false }
	if id, ok := unparen(call.Fun).(*ast.Ident); ok && id.Name == "append" && !c.isLocal("append") {
		holds = func(int) bool { return true }
	} else {
		switch sig, display, conf := c.resolveCall(call); {
		case display == "errors.Join":
			holds = func(int) bool { return true }
		case display == "fmt.Errorf":
			wrapped := wrappedArgs(call)
			holds = func(i int) bool { return wrapped[i] }
		case sig != nil && conf == ConfidenceCertain && sig.decl != nil && sig.errIdx >= 0:
			holds = func(i int) bool {
				p, ok := paramFor(sig.decl.Type, i)
				_, variadic := p.typ.(*ast.Ellipsis)
				set := !variadic && c.retains(call.Args[i], c.nonNil)
				return ok && c.w.holdsParam(sig, p.name, set)
			}
		}
	}
	for i, a := range call.Args {
		if c.retains(a, held) && holds(i) {
			return true
		}
	}
	return false
}

// traced says whether a handler records its error: untraced when a path
// doesn't, and possible when that rests on a receiver errpath can't see.
func (c *fnCtx) traced(body []ast.Stmt, errName string) (untraced bool, conf Confidence) {
	c.nonNil = stable(body, errName)
	defer func() { c.nonNil = nil }()
	r := pathRule{errName: errName, handOn: true, nonNil: c.nonNil}
	if c.tracedPaths(body, r) {
		return false, ""
	}
	r.lenient = true
	if c.tracedPaths(body, r) {
		return true, ConfidencePossible
	}
	return true, ConfidenceCertain
}

// wrappedArgs: which of fmt.Errorf's arguments a %w verb wraps, matching
// verbs to positions, including explicit ones (`%[2]w`) and * widths.
func wrappedArgs(call *ast.CallExpr) map[int]bool {
	out := map[int]bool{}
	if len(call.Args) == 0 {
		return out
	}
	lit, ok := unparen(call.Args[0]).(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return out
	}
	format, err := strconv.Unquote(lit.Value)
	if err != nil {
		return out
	}
	arg := 1 // call.Args index of the next operand
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		var verb byte
		if i, arg, verb = directive(format, i+1, arg); verb == 0 {
			return out // malformed: wrap nothing rather than guess
		}
		if verb == 'w' {
			out[arg] = true
		}
		if verb != '%' {
			arg++
		}
	}
	return out
}

// directive reads one formatting directive from format[i:], after its %:
// flags, widths (a * takes an operand), an explicit [n] operand, then the
// verb. It returns the verb's index, the operand the verb takes, and the verb.
func directive(format string, i, arg int) (end, operand int, verb byte) {
	for ; i < len(format); i++ {
		switch ch := format[i]; {
		case ch == '[':
			closing := strings.IndexByte(format[i:], ']')
			if closing < 0 {
				return i, arg, 0
			}
			n, err := strconv.Atoi(format[i+1 : i+closing])
			if err != nil {
				return i, arg, 0
			}
			arg = n
			i += closing
		case ch == '*':
			arg++
		case strings.IndexByte("+-# 0.123456789", ch) < 0:
			return i, arg, ch
		}
	}
	return i, arg, 0
}
