package errpath

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

var allowStartRe = regexp.MustCompile(`errpath:\s*allow\(`)

// AnalysePath analyses a Go file or a directory tree of Go sources.
// _test.go files, testdata, vendor and hidden directories are skipped.
func AnalysePath(root string) (Report, error) {
	info, err := os.Stat(root)
	if err != nil {
		return Report{}, err
	}
	l := &loader{report: NewReport(), w: newWorld()}
	if info.IsDir() {
		err = l.walk(root)
	} else {
		err = l.single(root)
	}
	if err != nil {
		return Report{}, err
	}

	w, files := l.w, l.files
	for _, pf := range files {
		w.indexDecls(pf)
	}
	for _, pf := range files {
		w.bindImports(pf)
	}
	w.fixRefs()
	w.inferErrorTypes(files)

	ids := map[string]int{}
	for _, pf := range files {
		analyseFile(w, pf, &l.report, ids)
	}
	finishCoverage(&l.report)
	return l.report, nil
}

// loader finds and parses the files to analyse.
type loader struct {
	report Report
	w      *world
	files  []*parsedFile
}

func (l *loader) parse(path, rel, dir string) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		l.report.ParseFailures = append(l.report.ParseFailures, ParseFailure{File: rel, Message: err.Error()})
		return
	}
	l.files = append(l.files, &parsedFile{rel: rel, fset: fset, file: f, pkg: l.w.pkgFor(dir, f.Name.Name)})
}

func (l *loader) single(path string) error {
	if !strings.HasSuffix(path, ".go") {
		return fmt.Errorf("%s: not a Go file", path)
	}
	if ip := findModuleAbove(filepath.Dir(path)); ip != "" {
		l.w.modules["."] = ip
	}
	l.parse(path, filepath.Base(path), ".")
	return nil
}

func (l *loader) walk(root string) error {
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(r)
		if d.IsDir() {
			if rel != "." && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		l.visitFile(path, rel, d.Name())
		return nil
	})
	if err != nil {
		return err
	}
	if _, ok := l.w.modules["."]; !ok {
		if ip := findModuleAbove(root); ip != "" {
			l.w.modules["."] = ip
		}
	}
	return nil
}

// skipDir: directories the go tool ignores too.
func skipDir(name string) bool {
	return name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

func (l *loader) visitFile(path, rel, name string) {
	dir := filepath.ToSlash(filepath.Dir(rel))
	switch {
	case name == "go.mod":
		if mod := readModule(path); mod != "" {
			l.w.modules[dir] = mod
		}
	case strings.HasSuffix(name, "_test.go"):
		l.report.SupportFilesSkipped++
	case strings.HasSuffix(name, ".go"):
		l.parse(path, rel, dir)
	}
}

// inferErrorTypes fills fnSig.errorType from hints, then from what bodies return.
func (w *world) inferErrorTypes(files []*parsedFile) {
	for _, pf := range files {
		for _, decl := range pf.file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			sig := w.sigOf(pf, fd)
			if sig == nil || sig.errIdx < 0 {
				continue
			}
			if hint := errorHint(fd); hint != "" {
				sig.errorType = hint
				continue
			}
			c := newFnCtx(w, pf, nil, sig, fd.Recv, fd.Type, fd.Body)
			sig.errorType = c.inferOwnError(fd.Body)
		}
	}
}

func (w *world) sigOf(pf *parsedFile, fd *ast.FuncDecl) *fnSig {
	key := fd.Name.Name
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		key = recvTypeName(fd.Recv.List[0].Type) + "." + key
	}
	if sig := pf.pkg.funcs[key]; sig != nil && sig.decl == fd {
		return sig
	}
	return nil
}

func analyseFile(w *world, pf *parsedFile, report *Report, ids map[string]int) {
	allows, bad := parseAllows(pf)
	for _, b := range bad {
		report.Findings = append(report.Findings, Finding{
			Rule: BadAllow, File: pf.rel, Line: b.line, Message: b.reason, Confidence: ConfidenceCertain,
		})
	}
	for _, decl := range pf.file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil || isErrorMethod(fd) {
			continue
		}
		sig := w.sigOf(pf, fd)
		if sig == nil { // a duplicate declaration (build-tag twins): analyse it on its own
			recv := ""
			if fd.Recv != nil && len(fd.Recv.List) > 0 {
				recv = recvTypeName(fd.Recv.List[0].Type)
			}
			sig = newSig(pf.pkg, recv, fd.Name.Name, fd.Type, pf)
		}
		name := fd.Name.Name
		id := pf.pkg.id + "."
		if sig.recv != "" {
			id += sig.recv + "."
		}
		id += name
		c := newFnCtx(w, pf, allows, sig, fd.Recv, fd.Type, fd.Body)
		c.report, c.ids = report, ids
		c.analyse(id, name, pf.fset.Position(fd.Pos()).Line, fd.Body)
	}
}

type allow struct {
	rule   string
	reason string
	line   int
}

func parseAllows(pf *parsedFile) (good, bad []allow) {
	for _, cg := range pf.file.Comments {
		for _, cm := range cg.List {
			loc := allowStartRe.FindStringIndex(cm.Text)
			if loc == nil {
				continue
			}
			line := pf.fset.Position(cm.Pos()).Line
			rule, reason, ok := strings.Cut(cm.Text[loc[1]:], ")")
			reason = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(reason), "*/"))
			switch {
			case !ok:
				bad = append(bad, allow{line: line, reason: "unclosed allow comment"})
			case rule != NoPanic && rule != NoSwallow && rule != NoTrace:
				bad = append(bad, allow{line: line, reason: fmt.Sprintf("unknown rule `%s` in allow comment", rule)})
			case reason == "":
				bad = append(bad, allow{line: line, reason: fmt.Sprintf("allow(%s) needs a reason, so it was ignored", rule)})
			default:
				good = append(good, allow{rule: rule, reason: reason, line: line})
			}
		}
	}
	return good, bad
}

// fnCtx walks one function or function literal.
type fnCtx struct {
	w         *world
	pf        *parsedFile
	allows    []allow
	sig       *fnSig
	enclosing string
	locals    map[string]typeRef
	namedErr  string // the named error result, which a bare return returns
	vars      map[string]varState
	depth     int
	declared  []string // local names in declaration order
	consts    map[string]ast.Expr
	nonNil    map[string]bool // error variables known to be set while judging a handler
	byScope   bool            // holder sets tell a local from one it shadows (see varKey)
	id        string
	report    *Report
	ids       map[string]int
}

func newFnCtx(w *world, pf *parsedFile, allows []allow, sig *fnSig, recv *ast.FieldList, ft *ast.FuncType, body *ast.BlockStmt) *fnCtx {
	c := &fnCtx{w: w, pf: pf, allows: allows, sig: sig, enclosing: sig.errorType, locals: map[string]typeRef{}, vars: map[string]varState{}}
	c.bindLocals(recv, ft, body)
	return c
}

func (c *fnCtx) child(lit *ast.FuncLit) *fnCtx {
	sig := newSig(c.pf.pkg, "", "", lit.Type, c.pf)
	n := &fnCtx{w: c.w, pf: c.pf, allows: c.allows, sig: sig, locals: map[string]typeRef{}, vars: map[string]varState{}, report: c.report, ids: c.ids}
	for k, v := range c.locals {
		n.locals[k] = v
	}
	n.bindLocals(nil, lit.Type, lit.Body)
	if sig.errIdx >= 0 {
		n.enclosing = n.inferOwnError(lit.Body)
	}
	return n
}

// bindLocals records every name declared in the function, with its type when
// the declaration makes it plain. Shadowing is ignored: one name, one type.
func (c *fnCtx) bindLocals(recv *ast.FieldList, ft *ast.FuncType, body *ast.BlockStmt) { //nolint:gocyclo // one case per node kind; splitting would only scatter them
	params := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			for _, n := range f.Names {
				c.bind(n.Name, typeRefOf(c.pf, f.Type))
				if !isErrorType(f.Type) {
					continue
				}
				// Params and results share the body's block. A named result starts nil.
				st := errState{}
				if fl == ft.Results {
					c.namedErr = n.Name
					st = errState{kind: stNil}
				}
				c.vars[n.Name] = varState{st: st, depth: 1}
			}
		}
	}
	params(recv)
	params(ft.Params)
	params(ft.Results)
	if body == nil {
		return
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.GenDecl:
			if s.Tok == token.CONST {
				c.bindConsts(s)
			}
		case *ast.AssignStmt:
			if s.Tok == token.DEFINE {
				c.bindAssign(s.Lhs, s.Rhs)
			}
		case *ast.ValueSpec:
			names := make([]ast.Expr, len(s.Names))
			for i, n := range s.Names {
				names[i] = n
			}
			if s.Type != nil {
				for _, n := range s.Names {
					c.bind(n.Name, typeRefOf(c.pf, s.Type))
				}
			} else {
				c.bindAssign(names, s.Values)
			}
		case *ast.RangeStmt:
			for _, e := range []ast.Expr{s.Key, s.Value} {
				if id, ok := e.(*ast.Ident); ok && s.Tok == token.DEFINE {
					c.bind(id.Name, typeRef{})
				}
			}
		case *ast.TypeSwitchStmt:
			if a, ok := s.Assign.(*ast.AssignStmt); ok {
				for _, l := range a.Lhs {
					c.bind(l.(*ast.Ident).Name, typeRef{})
				}
			}
		}
		return true
	})
}

// bind records a declared local, its type, and its place in declaration order.
func (c *fnCtx) bind(name string, t typeRef) {
	c.locals[name] = t
	if name != "_" && !contains(c.declared, name) {
		c.declared = append(c.declared, name)
	}
}

func (c *fnCtx) bindAssign(lhs, rhs []ast.Expr) {
	var types []typeRef
	switch {
	case len(lhs) == len(rhs):
		for _, r := range rhs {
			types = append(types, c.typeOf(r))
		}
	case len(rhs) == 1:
		if call, ok := unparen(rhs[0]).(*ast.CallExpr); ok {
			if sig, _, _ := c.resolveCall(call); sig != nil && len(sig.results) == len(lhs) {
				for _, r := range sig.results {
					types = append(types, typeRefOf(sig.file, r))
				}
			} else if span := c.startedSpan(call); span.name != "" && len(lhs) == 2 {
				types = []typeRef{{}, span}
			}
		}
	}
	for i, l := range lhs {
		id, ok := l.(*ast.Ident)
		if !ok || id.Name == "_" {
			continue
		}
		if i < len(types) {
			c.bind(id.Name, types[i])
		} else if _, seen := c.locals[id.Name]; !seen {
			c.bind(id.Name, typeRef{})
		}
	}
}

func (c *fnCtx) isLocal(name string) bool {
	_, ok := c.locals[name]
	return ok
}

// typeOf is the named type of an expression, when the source makes it plain.
func (c *fnCtx) typeOf(e ast.Expr) typeRef {
	switch x := unparen(e).(type) {
	case *ast.Ident:
		if t, ok := c.locals[x.Name]; ok {
			return t
		}
		return c.w.valueType(c.pf.pkg, x.Name)
	case *ast.CompositeLit:
		return typeRefOf(c.pf, x.Type)
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			return c.typeOf(x.X)
		}
	case *ast.StarExpr:
		return c.typeOf(x.X)
	case *ast.SelectorExpr:
		return c.w.field(c.typeOf(x.X), x.Sel.Name)
	case *ast.CallExpr:
		if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 {
			return typeRefOf(c.pf, x.Args[0])
		}
		if sig, _, conf := c.resolveCall(x); sig != nil && conf == ConfidenceCertain && len(sig.results) > 0 {
			return typeRefOf(sig.file, sig.results[0])
		}
		return c.externalType(x)
	}
	return typeRef{}
}

// resolveCall finds the declaration a call reaches. A method on a value of
// unknown type resolves with possible confidence when every indexed method of
// that name returns an error in the same position.
func (c *fnCtx) resolveCall(call *ast.CallExpr) (*fnSig, string, Confidence) {
	fun := unparen(call.Fun)
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun = f.X
	case *ast.IndexListExpr:
		fun = f.X
	}
	switch f := fun.(type) {
	case *ast.Ident:
		if c.isLocal(f.Name) {
			return nil, f.Name, ""
		}
		if sig := c.pf.pkg.funcs[f.Name]; sig != nil {
			return sig, f.Name, ConfidenceCertain
		}
		return nil, f.Name, ""
	case *ast.SelectorExpr:
		if sig, display, ok := c.resolvePkgFunc(f); ok {
			if sig == nil {
				return nil, display, ""
			}
			return sig, display, ConfidenceCertain
		}
		return c.resolveMethod(f)
	}
	return nil, "func", ""
}

// resolvePkgFunc resolves pkg.Func through the file's imports. ok is false
// when the selector isn't on an import.
func (c *fnCtx) resolvePkgFunc(f *ast.SelectorExpr) (sig *fnSig, display string, ok bool) {
	id, isID := f.X.(*ast.Ident)
	if !isID || c.isLocal(id.Name) {
		return nil, "", false
	}
	imp, isImport := c.pf.imports[id.Name]
	if !isImport {
		return nil, "", false
	}
	display = id.Name + "." + f.Sel.Name
	if imp.pkg == nil {
		return nil, display, true
	}
	return imp.pkg.funcs[f.Sel.Name], display, true
}

func (c *fnCtx) resolveMethod(f *ast.SelectorExpr) (*fnSig, string, Confidence) {
	display := shortName(f.X) + "." + f.Sel.Name
	t := c.typeOf(f.X)
	if t.known() {
		if sig := c.w.method(t, f.Sel.Name, 0); sig != nil {
			return sig, display, ConfidenceCertain
		}
		if t.pkg.types[t.name] != nil {
			return nil, display, "" // a project type without that method in this tree
		}
	} else if t.name != "" {
		return nil, display, "" // an external type
	}
	if sig := c.w.agreeingMethod(f.Sel.Name); sig != nil {
		return sig, display, ConfidencePossible
	}
	return nil, display, ""
}

func (c *fnCtx) allowed(rule string, n ast.Node) *string {
	first := c.pf.fset.Position(n.Pos()).Line
	last := c.pf.fset.Position(n.End()).Line
	for _, a := range c.allows {
		if a.rule == rule && a.line+1 >= first && a.line <= last {
			r := a.reason
			return &r
		}
	}
	return nil
}

func (c *fnCtx) line(n ast.Node) int { return c.pf.fset.Position(n.Pos()).Line }

// analyse builds the railway, records the function, then its closures.
func (c *fnCtx) analyse(id, name string, line int, body *ast.BlockStmt) {
	c.ids[id]++
	if n := c.ids[id]; n > 1 {
		id = fmt.Sprintf("%s#%d", id, n)
	}
	c.id = id
	nodes := c.block(body.List)
	if !endsWithExit(nodes) {
		nodes = append(nodes, Succeed())
	}
	ret := Returns{Kind: "other"}
	if c.sig.errIdx >= 0 {
		ret = Returns{Kind: "result", Error: c.enclosing}
		if ret.Error == "" {
			ret.Error = "error"
		}
	}
	fn := Function{ID: id, Name: name, File: c.pf.rel, Line: line, Returns: ret, Nodes: nodes, Locals: c.declared}
	c.report.Functions = append(c.report.Functions, fn)
	c.report.Findings = append(c.report.Findings, findingsOf(fn)...)

	for _, cl := range closures(body) {
		suffix := "." + cl.name
		c.child(cl.lit).analyse(c.id+suffix, name+suffix, c.line(cl.lit), cl.lit.Body)
	}
}

type closure struct {
	lit  *ast.FuncLit
	name string
}

// closures names the function literals directly in body: by the variable
// they're assigned to, else go1, defer1 or func1 counted per kind, so adding
// or removing one closure doesn't rename the others.
func closures(body *ast.BlockStmt) []closure { //nolint:gocyclo // one case per node kind; splitting would only scatter them
	named := map[*ast.FuncLit]string{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.AssignStmt:
			if len(x.Lhs) == len(x.Rhs) {
				for i, r := range x.Rhs {
					lit, isLit := unparen(r).(*ast.FuncLit)
					if id, isID := x.Lhs[i].(*ast.Ident); isLit && isID && id.Name != "_" {
						named[lit] = id.Name
					}
				}
			}
		case *ast.ValueSpec:
			if len(x.Names) == len(x.Values) {
				for i, v := range x.Values {
					if lit, isLit := unparen(v).(*ast.FuncLit); isLit {
						named[lit] = x.Names[i].Name
					}
				}
			}
		case *ast.GoStmt:
			if lit, isLit := unparen(x.Call.Fun).(*ast.FuncLit); isLit {
				named[lit] = "go"
			}
		case *ast.DeferStmt:
			if lit, isLit := unparen(x.Call.Fun).(*ast.FuncLit); isLit {
				named[lit] = "defer"
			}
		}
		return true
	})
	counts := map[string]int{}
	var out []closure
	ast.Inspect(body, func(n ast.Node) bool {
		lit, ok := n.(*ast.FuncLit)
		if !ok {
			return true
		}
		name := named[lit]
		if name == "" || name == "go" || name == "defer" {
			kind := or(name, "func")
			counts[kind]++
			name = fmt.Sprintf("%s%d", kind, counts[kind])
		}
		out = append(out, closure{lit, name})
		return false
	})
	return out
}

func (c *fnCtx) stmts(list []ast.Stmt) []Node {
	var nodes []Node
	for i := 0; i < len(list); i++ {
		if chk, ok := c.errAssign(list[i]); ok {
			if j := findCheck(list, i, chk.errName); j > 0 {
				// What runs between the call and its check runs either way.
				nodes = append(nodes, c.panicsIn(chk.call.Args...)...)
				c.setVar(chk.errName, errState{kind: stCallee, call: chk.call}, isDefine(list[i]))
				nodes = append(nodes, c.stmts(list[i+1:j])...)
				nodes = append(nodes, c.errCheck(chk, list[j].(*ast.IfStmt))...)
				i = j
				continue
			}
		}
		nodes = append(nodes, c.stmt(list[i])...)
	}
	return nodes
}

func isDefine(st ast.Stmt) bool {
	a, ok := st.(*ast.AssignStmt)
	return !ok || a.Tok == token.DEFINE
}

func (c *fnCtx) stmt(st ast.Stmt) []Node { //nolint:gocyclo // one case per node kind; splitting would only scatter them
	switch s := st.(type) {
	case *ast.IfStmt:
		return c.ifStmt(s)
	case *ast.SwitchStmt:
		return c.switchStmt(s, "")
	case *ast.TypeSwitchStmt:
		return c.typeSwitch(s, "")
	case *ast.SelectStmt:
		return c.selectStmt(s, "")
	case *ast.ForStmt:
		return c.forStmt(s, "")
	case *ast.RangeStmt:
		return c.rangeStmt(s, "")
	case *ast.LabeledStmt:
		return c.labeled(s)
	case *ast.ReturnStmt:
		return c.returnStmt(s)
	case *ast.ExprStmt:
		return c.exprStmt(s)
	case *ast.AssignStmt:
		return c.assign(s, s.Lhs, s.Rhs)
	case *ast.DeclStmt:
		return c.declStmt(s)
	case *ast.DeferStmt:
		return c.deferStmt(s)
	case *ast.GoStmt:
		return c.panicsIn(s.Call.Args...)
	case *ast.BlockStmt:
		return c.block(s.List)
	case *ast.BranchStmt:
		name := s.Tok.String()
		if s.Label != nil {
			name += " " + s.Label.Name
		}
		return []Node{Jump(name, c.line(s))}
	case *ast.IncDecStmt:
		return c.panicsIn(s.X)
	case *ast.SendStmt:
		return c.panicsIn(s.Chan, s.Value)
	}
	return nil
}

// labeled marks where a goto can land, then the statement, which keeps the
// label when break or continue can name it.
func (c *fnCtx) labeled(s *ast.LabeledStmt) []Node {
	label := s.Label.Name + ": "
	nodes := []Node{LabelNode(s.Label.Name, c.line(s))}
	switch inner := s.Stmt.(type) {
	case *ast.ForStmt:
		return append(nodes, c.forStmt(inner, label)...)
	case *ast.RangeStmt:
		return append(nodes, c.rangeStmt(inner, label)...)
	case *ast.SwitchStmt:
		return append(nodes, c.switchStmt(inner, label)...)
	case *ast.TypeSwitchStmt:
		return append(nodes, c.typeSwitch(inner, label)...)
	case *ast.SelectStmt:
		return append(nodes, c.selectStmt(inner, label)...)
	}
	return append(nodes, c.stmt(s.Stmt)...)
}

// errCall is `v, err := call()` with the error in a named variable.
type errCall struct {
	call    *ast.CallExpr
	sig     *fnSig
	conf    Confidence
	display string
	errName string
	line    int
}

func (c *fnCtx) errAssign(st ast.Stmt) (errCall, bool) {
	lhs, rhs := assignParts(st)
	if len(rhs) != 1 || len(lhs) == 0 {
		return errCall{}, false
	}
	call, ok := unparen(rhs[0]).(*ast.CallExpr)
	if !ok {
		return errCall{}, false
	}
	sig, display, conf := c.resolveCall(call)
	pos := len(lhs) - 1 // by convention, when the callee is unknown
	if sig != nil && sig.errIdx >= 0 && len(sig.results) == len(lhs) {
		pos = sig.errIdx
	}
	id, ok := lhs[pos].(*ast.Ident)
	if !ok || id.Name == "_" {
		return errCall{}, false
	}
	if (sig == nil || sig.errIdx < 0) && !isErrName(id.Name) {
		return errCall{}, false
	}
	return errCall{call: call, sig: sig, conf: conf, display: display, errName: id.Name, line: c.line(st)}, true
}

// assignParts splits `a, b := x` or `var a, b = x` into its two sides.
func assignParts(st ast.Stmt) (lhs, rhs []ast.Expr) {
	switch s := st.(type) {
	case *ast.AssignStmt:
		return s.Lhs, s.Rhs
	case *ast.DeclStmt:
		gd, ok := s.Decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR || len(gd.Specs) != 1 {
			return nil, nil
		}
		vs := gd.Specs[0].(*ast.ValueSpec)
		for _, n := range vs.Names {
			lhs = append(lhs, n)
		}
		return lhs, vs.Values
	}
	return nil, nil
}

// isErrName: err, errX, fooErr.
func isErrName(n string) bool {
	if n == "err" || strings.HasSuffix(n, "Err") {
		return true
	}
	return len(n) > 3 && strings.HasPrefix(n, "err") && (unicode.IsUpper(rune(n[3])) || unicode.IsDigit(rune(n[3])))
}

func isErrNilCheck(cond ast.Expr, errName string) bool {
	bin, ok := unparen(cond).(*ast.BinaryExpr)
	if !ok || bin.Op != token.NEQ {
		return false
	}
	is := func(e ast.Expr, name string) bool {
		id, ok := unparen(e).(*ast.Ident)
		return ok && id.Name == name
	}
	return (is(bin.X, errName) && is(bin.Y, "nil")) || (is(bin.Y, errName) && is(bin.X, "nil"))
}

// errCheck turns a call and its `if err != nil` into a step (the error leaves
// the function) or a recovery (the error is handled and the function goes on).
func (c *fnCtx) errCheck(chk errCall, ifs *ast.IfStmt) []Node {
	body := ifs.Body.List
	var ret *ast.ReturnStmt
	if len(body) > 0 {
		ret, _ = body[len(body)-1].(*ast.ReturnStmt)
	}
	setErr := func() { c.setVar(chk.errName, errState{kind: stNonNil}, false) }
	elseArm := func() []Node {
		c.setVar(chk.errName, errState{kind: stNil}, false)
		if ifs.Else == nil {
			return nil
		}
		return c.elseNodes(ifs.Else)
	}
	// The error path ends the function, so what it does to variables stays there.
	exits := func(f func() []Node) []Node {
		saved := maps.Clone(c.vars)
		setErr()
		var nodes []Node
		c.scoped(func() { nodes = f() })
		c.vars = saved
		return nodes
	}
	setErr()
	failing := ret != nil && c.returnsError(ret)
	panics := !failing && c.endsInPanic(body)
	if failing || panics {
		step := c.makeStep(chk)
		if failing {
			step.Cleanup = exits(func() []Node { return c.stmts(body[:len(body)-1]) })
			if conv := c.conversion(ret, chk.errName); conv != "" {
				step.Conversions = []string{conv}
			}
		} else {
			step.Cleanup = exits(func() []Node { return c.stmts(body) })
		}
		return append([]Node{step}, elseArm()...)
	}
	rec := RecoverNode(chk.display, "if "+chk.errName+" != nil", chk.line)
	if rec.Untraced, rec.Confidence = c.traced(body, chk.errName); rec.Untraced {
		rec.Allowed = c.allowed(NoTrace, ifs)
	}
	nodes := []Node{rec}
	arms := c.arms(
		func() []Node { setErr(); return c.block(body) },
		elseArm,
	)
	if len(arms[0]) > 0 || len(arms[1]) > 0 {
		nodes = append(nodes, BranchNode("if "+chk.errName+" != nil", []Arm{{Label: "yes", Body: arms[0]}, {Label: "no", Body: arms[1]}}))
	}
	return nodes
}

func (c *fnCtx) makeStep(chk errCall) Node {
	switch {
	case chk.sig != nil && chk.conf == ConfidenceCertain && chk.sig.errorType != "":
		return Step(chk.display, StrPtr(chk.sig.errorType), ErrorSourceCallee, chk.line)
	case c.enclosing != "":
		return Step(chk.display, StrPtr(c.enclosing), ErrorSourceEnclosing, chk.line)
	}
	return Step(chk.display, StrPtr("error"), ErrorSourceUnknown, chk.line)
}

func (c *fnCtx) endsInPanic(body []ast.Stmt) bool {
	if len(body) == 0 {
		return false
	}
	es, ok := body[len(body)-1].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := unparen(es.X).(*ast.CallExpr)
	if !ok {
		return false
	}
	n, ok := c.panicCall(call, true, es)
	return ok && n.Terminal
}

// errExpr is the expression a return statement gives for the error result.
// forwarded is set for `return f()` passing every result through.
func (c *fnCtx) errExpr(ret *ast.ReturnStmt) (e ast.Expr, forwarded *ast.CallExpr) {
	if c.sig.errIdx < 0 || len(ret.Results) == 0 {
		return nil, nil
	}
	if len(ret.Results) == len(c.sig.results) {
		return ret.Results[c.sig.errIdx], nil
	}
	if len(ret.Results) == 1 {
		if call, ok := unparen(ret.Results[0]).(*ast.CallExpr); ok {
			return nil, call
		}
	}
	return nil, nil
}

func (c *fnCtx) returnsError(ret *ast.ReturnStmt) bool {
	if len(ret.Results) == 0 {
		if c.namedErr == "" {
			return false
		}
		return c.vars[c.namedErr].st.kind != stNil
	}
	e, fwd := c.errExpr(ret)
	if fwd != nil {
		return true
	}
	if e == nil {
		return false
	}
	if _, st, ok := c.tracked(e); ok {
		return st.kind != stNil
	}
	return c.classify(e, "").kind != errNil
}

type errKind int

const (
	errNil       errKind = iota
	errPropagate         // a variable holding some error
	errNew               // a fresh, named error
	errWrap              // the error, wrapped
	errCallee            // a call whose error result is passed on; may be nil
)

type errClass struct {
	kind errKind
	name string
	call *ast.CallExpr
}

// classify says what an expression in the error result position is.
func (c *fnCtx) classify(e ast.Expr, errVar string) errClass {
	switch x := unparen(e).(type) {
	case *ast.Ident:
		switch {
		case x.Name == "nil":
			return errClass{kind: errNil}
		case x.Name == errVar || c.isLocal(x.Name):
			return errClass{kind: errPropagate}
		}
		return errClass{kind: errNew, name: x.Name} // a package-level sentinel
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && !c.isLocal(id.Name) {
			if _, imp := c.pf.imports[id.Name]; imp {
				return errClass{kind: errNew, name: id.Name + "." + x.Sel.Name}
			}
		}
		return errClass{kind: errPropagate}
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			return c.classify(x.X, errVar)
		}
	case *ast.CompositeLit:
		if n := typeRefOf(c.pf, x.Type).name; n != "" {
			return errClass{kind: errNew, name: n}
		}
	case *ast.CallExpr:
		return c.classifyCall(x, errVar)
	}
	return errClass{kind: errPropagate}
}

func (c *fnCtx) classifyCall(x *ast.CallExpr, errVar string) errClass {
	sig, display, _ := c.resolveCall(x)
	switch display {
	case "fmt.Errorf":
		if wrapsWithW(x) {
			return errClass{kind: errWrap}
		}
		return errClass{kind: errNew, name: "error"}
	case "errors.New":
		return errClass{kind: errNew, name: "error"}
	case "errors.Join":
		return errClass{kind: errWrap}
	}
	name := ""
	if sig != nil {
		name = sig.errorType
	}
	if mentionsErr(x.Args, errVar) {
		return errClass{kind: errWrap, name: name}
	}
	if !isConstructor(display[strings.LastIndex(display, ".")+1:]) {
		return errClass{kind: errCallee, call: x}
	}
	if name == "" && sig != nil && len(sig.results) > 0 && !isErrorType(sig.results[0]) {
		name = typeRefOf(sig.file, sig.results[0]).name
	}
	return errClass{kind: errNew, name: or(name, "error")}
}

func wrapsWithW(call *ast.CallExpr) bool {
	for _, a := range call.Args {
		if lit, ok := a.(*ast.BasicLit); ok && strings.Contains(lit.Value, "%w") {
			return true
		}
	}
	return false
}

// mentionsErr: do the arguments use an error variable, so the call wraps it?
func mentionsErr(args []ast.Expr, errVar string) bool {
	found := false
	for _, a := range args {
		ast.Inspect(a, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && (id.Name == errVar || isErrName(id.Name)) {
				found = true
			}
			return !found
		})
	}
	return found
}

// isConstructor: a call that makes a fresh error, by its name.
func isConstructor(name string) bool {
	return strings.HasPrefix(name, "New") || strings.HasPrefix(name, "new") ||
		strings.HasSuffix(name, "Error") || strings.HasSuffix(name, "Err") || strings.HasSuffix(name, "Errorf")
}

// conversion describes how the error of a step was changed before return:
// "" when returned as is, "wrap" when wrapped with %w, else the new error.
func (c *fnCtx) conversion(ret *ast.ReturnStmt, errVar string) string {
	e, fwd := c.errExpr(ret)
	if len(ret.Results) == 0 && c.namedErr != "" && c.namedErr != errVar {
		e = ast.NewIdent(c.namedErr)
	}
	if fwd != nil || e == nil {
		return ""
	}
	if name, st, ok := c.tracked(e); ok && name != errVar && st.kind == stNew {
		return st.name
	}
	cl := c.classify(e, errVar)
	switch cl.kind {
	case errWrap:
		if cl.name != "" {
			return cl.name
		}
		return "wrap"
	case errNew:
		return cl.name
	case errCallee:
		return "error"
	}
	if id, ok := unparen(e).(*ast.Ident); ok && id.Name != errVar {
		return "error" // a different error variable
	}
	return ""
}

// inferOwnError names the function's error when every failing return names it.
func (c *fnCtx) inferOwnError(body *ast.BlockStmt) string {
	var names []string
	known := true
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			e, fwd := c.errExpr(s)
			if fwd != nil || (e == nil && len(s.Results) == 0 && c.namedErr != "") {
				known = false
				return false
			}
			if e == nil {
				return false
			}
			cl := c.classify(e, "")
			switch {
			case cl.kind == errNil:
			case cl.kind == errNew && cl.name != "error":
				if !contains(names, cl.name) {
					names = append(names, cl.name)
				}
			default:
				known = false
			}
		}
		return true
	})
	if !known || len(names) == 0 {
		return ""
	}
	return strings.Join(names, " | ")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (c *fnCtx) ifStmt(s *ast.IfStmt) []Node {
	var nodes []Node
	c.scoped(func() {
		if s.Init != nil {
			if chk, ok := c.errAssign(s.Init); ok && isErrNilCheck(s.Cond, chk.errName) {
				c.setVar(chk.errName, errState{kind: stCallee, call: chk.call}, isDefine(s.Init))
				nodes = append(c.panicsIn(chk.call.Args...), c.errCheck(chk, s)...)
				return
			}
			nodes = c.stmt(s.Init)
		}
		nodes = append(nodes, c.panicsIn(s.Cond)...)
		if c.deadErrorTest(s.Cond) {
			if s.Else != nil {
				nodes = append(nodes, c.elseNodes(s.Else)...)
			}
			return
		}
		name, yes, no, refined := c.refine(s.Cond)
		arms := c.arms(
			func() []Node {
				if refined {
					c.setVar(name, yes, false)
				}
				return c.block(s.Body.List)
			},
			func() []Node {
				if refined {
					c.setVar(name, no, false)
				}
				if s.Else == nil {
					return nil
				}
				return c.elseNodes(s.Else)
			},
		)
		nodes = append(nodes, branch("if "+exprString(s.Cond), []Arm{{Label: "yes", Body: arms[0]}, {Label: "no", Body: arms[1]}})...)
	})
	return nodes
}

// branch is a branch node, or nothing when no arm does anything on a
// failure path: such a branch can't change where errors go.
func branch(label string, arms []Arm) []Node {
	for _, a := range arms {
		if len(a.Body) > 0 {
			return []Node{BranchNode(label, arms)}
		}
	}
	return nil
}

// loop is a loop node, or nothing when it does nothing on a failure path. A
// loop that never ends stays even when empty: nothing after it runs.
func loop(label string, canSkip bool, head, body, post []Node) []Node {
	n := LoopNode(label, canSkip, body)
	n.Head, n.Post = head, post
	if len(head) == 0 && len(body) == 0 && len(post) == 0 && !Diverges(n) {
		return nil
	}
	return []Node{n}
}

func (c *fnCtx) elseNodes(els ast.Stmt) []Node {
	if b, ok := els.(*ast.BlockStmt); ok {
		return c.block(b.List)
	}
	return c.stmt(els)
}

func (c *fnCtx) caseArms(clauses []ast.Stmt, label func(ast.Stmt) (string, []ast.Stmt), implicitDefault bool) []Arm {
	var labels []string
	var runs []func() []Node
	hasDefault := false
	for _, st := range clauses {
		l, body := label(st)
		hasDefault = hasDefault || l == "default"
		labels = append(labels, l)
		runs = append(runs, func() []Node { return c.block(body) })
	}
	if implicitDefault && !hasDefault {
		labels = append(labels, "default")
		runs = append(runs, func() []Node { return nil })
	}
	bodies := c.arms(runs...)
	arms := make([]Arm, len(labels))
	for i := range labels {
		arms[i] = Arm{Label: labels[i], Body: bodies[i]}
	}
	return arms
}

func caseLabel(st ast.Stmt) (string, []ast.Stmt) {
	cc := st.(*ast.CaseClause)
	if len(cc.List) == 0 {
		return "default", cc.Body
	}
	parts := make([]string, len(cc.List))
	for i, e := range cc.List {
		parts[i] = exprString(e)
	}
	return strings.Join(parts, ", "), cc.Body
}

func (c *fnCtx) switchStmt(s *ast.SwitchStmt, label string) []Node {
	var nodes []Node
	c.scoped(func() {
		if s.Init != nil {
			nodes = c.stmt(s.Init)
		}
		label += "switch"
		if s.Tag != nil {
			nodes = append(nodes, c.panicsIn(s.Tag)...)
			label += " " + exprString(s.Tag)
		}
		// Case expressions are evaluated in order until one matches.
		nodes = append(nodes, c.panicsIn(caseExprs(s.Body.List)...)...)
		nodes = append(nodes, branch(label, c.caseArms(s.Body.List, caseLabel, true))...)
	})
	return nodes
}

func caseExprs(clauses []ast.Stmt) []ast.Expr {
	var out []ast.Expr
	for _, st := range clauses {
		out = append(out, st.(*ast.CaseClause).List...)
	}
	return out
}

func (c *fnCtx) typeSwitch(s *ast.TypeSwitchStmt, label string) []Node {
	var pre []Node
	if s.Init != nil {
		pre = c.stmt(s.Init)
	}
	var x ast.Expr
	switch a := s.Assign.(type) {
	case *ast.AssignStmt:
		x = a.Rhs[0]
	case *ast.ExprStmt:
		x = a.X
	}
	if ta, ok := x.(*ast.TypeAssertExpr); ok {
		x = ta.X
	}
	pre = append(pre, c.panicsIn(x)...)
	label += "switch " + exprString(x) + ".(type)"
	var arms []Arm
	c.scoped(func() { arms = c.caseArms(s.Body.List, caseLabel, true) })
	return append(pre, branch(label, arms)...)
}

func (c *fnCtx) selectStmt(s *ast.SelectStmt, label string) []Node {
	commLabel := func(st ast.Stmt) (string, []ast.Stmt) {
		cc := st.(*ast.CommClause)
		switch comm := cc.Comm.(type) {
		case nil:
			return "default", cc.Body
		case *ast.SendStmt:
			return exprString(comm.Chan) + " <- " + exprString(comm.Value), cc.Body
		case *ast.ExprStmt:
			return exprString(comm.X), cc.Body
		case *ast.AssignStmt:
			return exprString(comm.Rhs[0]), cc.Body
		}
		return "case", cc.Body
	}
	// Every channel and sent value is evaluated once, on entering the select.
	var operands []ast.Expr
	for _, st := range s.Body.List {
		switch comm := st.(*ast.CommClause).Comm.(type) {
		case *ast.SendStmt:
			operands = append(operands, comm.Chan, comm.Value)
		case *ast.ExprStmt:
			operands = append(operands, comm.X)
		case *ast.AssignStmt:
			operands = append(operands, comm.Rhs...)
		}
	}
	return append(c.panicsIn(operands...), branch(label+"select", c.caseArms(s.Body.List, commLabel, false))...)
}

func (c *fnCtx) forStmt(s *ast.ForStmt, label string) []Node {
	var nodes []Node
	c.scoped(func() {
		if s.Init != nil {
			nodes = c.stmt(s.Init)
		}
		label += "for"
		var head []Node
		if s.Cond != nil {
			label += " " + exprString(s.Cond)
			head = c.panicsIn(s.Cond) // evaluated before every iteration
		}
		var post []Node
		body := c.loopBody(func() []Node {
			b := c.block(s.Body.List)
			if s.Post != nil {
				post = c.stmt(s.Post)
			}
			return b
		})
		nodes = append(nodes, loop(label, s.Cond != nil, head, body, post)...)
	})
	return nodes
}

func (c *fnCtx) rangeStmt(s *ast.RangeStmt, label string) []Node {
	pre := c.panicsIn(s.X)
	label += "for range " + exprString(s.X)
	body := c.loopBody(func() []Node { return c.block(s.Body.List) })
	return append(pre, loop(label, true, nil, body, nil)...)
}

// loopBody runs a body that may run any number of times, so its effects on
// variables join with not running at all.
func (c *fnCtx) loopBody(f func() []Node) []Node {
	return c.arms(f, func() []Node { return nil })[0]
}

func (c *fnCtx) returnStmt(s *ast.ReturnStmt) []Node {
	nodes := c.panicsIn(s.Results...)
	line := c.line(s)
	if c.sig.errIdx < 0 {
		return append(nodes, Succeed())
	}
	if len(s.Results) == 0 {
		if c.namedErr == "" {
			return append(nodes, Succeed())
		}
		return append(nodes, c.fromState(c.namedErr, c.vars[c.namedErr].st, line)...)
	}
	e, fwd := c.errExpr(s)
	if fwd != nil {
		return append(nodes, c.calleeStep(fwd, line), Succeed())
	}
	if e == nil {
		return append(nodes, Succeed())
	}
	if name, st, ok := c.tracked(e); ok {
		return append(nodes, c.fromState(name, st, line)...)
	}
	cl := c.classify(e, "")
	switch cl.kind {
	case errNil:
		return append(nodes, Succeed())
	case errCallee:
		return append(nodes, c.calleeStep(cl.call, line), Succeed())
	case errNew:
		return append(nodes, Fail(cl.name, line))
	}
	return append(nodes, Fail(or(c.enclosing, "error"), line))
}

// calleeStep is a call whose error the function returns as is: `return f()`.
func (c *fnCtx) calleeStep(call *ast.CallExpr, line int) Node {
	sig, display, conf := c.resolveCall(call)
	return c.makeStep(errCall{call: call, sig: sig, conf: conf, display: display, line: line})
}

func (c *fnCtx) exprStmt(s *ast.ExprStmt) []Node {
	call, ok := unparen(s.X).(*ast.CallExpr)
	if !ok {
		return c.panicsIn(s.X)
	}
	nodes := c.panicsIn(call.Args...)
	if p, ok := c.panicCall(call, true, s); ok {
		return append(nodes, p)
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		nodes = append(nodes, c.panicsIn(sel.X)...)
	}
	if sig, display, conf := c.resolveCall(call); sig != nil && sig.errIdx >= 0 {
		nodes = append(nodes, SwallowNode(display, "discarded return", conf, c.line(s), c.allowed(NoSwallow, s)))
	}
	return nodes
}

func (c *fnCtx) declStmt(s *ast.DeclStmt) []Node {
	gd, ok := s.Decl.(*ast.GenDecl)
	if !ok || gd.Tok != token.VAR {
		return nil
	}
	var nodes []Node
	for _, spec := range gd.Specs {
		vs := spec.(*ast.ValueSpec)
		lhs := make([]ast.Expr, len(vs.Names))
		for i, n := range vs.Names {
			lhs[i] = n
			if len(vs.Values) == 0 && vs.Type != nil && isErrorType(vs.Type) {
				c.setVar(n.Name, errState{kind: stNil}, true)
			}
		}
		nodes = append(nodes, c.assign(s, lhs, vs.Values)...)
	}
	return nodes
}

// assign finds errors dropped with a blank identifier.
func (c *fnCtx) assign(st ast.Node, lhs, rhs []ast.Expr) []Node {
	nodes := c.panicsIn(rhs...)
	blank := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && id.Name == "_"
	}
	check := func(call *ast.CallExpr, targets []ast.Expr) {
		sig, display, conf := c.resolveCall(call)
		if sig == nil || sig.errIdx < 0 || len(sig.results) != len(targets) || !blank(targets[sig.errIdx]) {
			return
		}
		how := "_"
		if len(targets) == 1 {
			how = "_ ="
		}
		nodes = append(nodes, SwallowNode(display, how, conf, c.line(st), c.allowed(NoSwallow, st)))
	}
	c.trackAssign(st, lhs, rhs)
	if len(rhs) == 1 {
		if call, ok := unparen(rhs[0]).(*ast.CallExpr); ok {
			check(call, lhs)
		}
		return nodes
	}
	if len(lhs) == len(rhs) {
		for i, r := range rhs {
			if call, ok := unparen(r).(*ast.CallExpr); ok {
				check(call, lhs[i:i+1])
			}
		}
	}
	return nodes
}

// trackAssign records what assigned error variables now hold.
func (c *fnCtx) trackAssign(st ast.Node, lhs, rhs []ast.Expr) {
	define := true
	if a, ok := st.(*ast.AssignStmt); ok {
		define = a.Tok == token.DEFINE
	}
	if s, ok := st.(ast.Stmt); ok {
		if chk, ok := c.errAssign(s); ok {
			c.setVar(chk.errName, errState{kind: stCallee, call: chk.call}, define)
			return
		}
	}
	for i, l := range lhs {
		id, ok := l.(*ast.Ident)
		if !ok {
			continue
		}
		if _, known := c.vars[id.Name]; !known && !isErrName(id.Name) {
			continue
		}
		st := errState{}
		if len(lhs) == len(rhs) {
			st = c.stateOf(rhs[i])
		}
		c.setVar(id.Name, st, define)
	}
}

// deferStmt: the arguments and receiver are evaluated now; the call itself
// runs as the function returns.
func (c *fnCtx) deferStmt(s *ast.DeferStmt) []Node {
	line := c.line(s)
	nodes := c.panicsIn(s.Call.Args...)
	if lit, ok := unparen(s.Call.Fun).(*ast.FuncLit); ok {
		if containsRecover(lit.Body) {
			nodes = append(nodes, RecoverNode("recover", "defer recover", line))
		}
		return nodes
	}
	if sel, ok := unparen(s.Call.Fun).(*ast.SelectorExpr); ok {
		nodes = append(nodes, c.panicsIn(sel.X)...)
	}
	if p, ok := c.panicCall(s.Call, false, s); ok {
		nodes = append(nodes, p)
	}
	sig, display, conf := c.resolveCall(s.Call)
	if sig != nil && conf == ConfidenceCertain && sig.decl != nil && containsRecover(sig.decl.Body) {
		nodes = append(nodes, RecoverNode(display, "defer recover", line))
	}
	return nodes
}

func containsRecover(body *ast.BlockStmt) bool {
	found := false
	if body == nil {
		return false
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "recover" {
				found = true
			}
		}
		return !found
	})
	return found
}

// panicCall recognises calls that panic or exit. Statement-level calls to
// panic, os.Exit, log.Fatal* and log.Panic* end the path; Must* helpers may
// panic wherever they appear.
func (c *fnCtx) panicCall(call *ast.CallExpr, statement bool, at ast.Node) (Node, bool) {
	_, display, _ := c.resolveCall(call)
	base := display[strings.LastIndex(display, ".")+1:]
	how, terminal, conf := "", false, ConfidenceCertain
	if statement {
		how = c.endsProcess(call, display, base)
		terminal = how != ""
	}
	if how == "" && isMust(base) {
		how, conf = "panic", ConfidencePossible
	}
	if how == "" {
		return Node{}, false
	}
	return PanicNode(display, how, terminal, conf, c.line(call), c.allowed(NoPanic, at)), true
}

// endsProcess says whether a call statement always panics ("panic") or exits
// ("exit"): panic, os.Exit, any *.Fatal*, and package-level *.Panic*.
func (c *fnCtx) endsProcess(call *ast.CallExpr, display, base string) string {
	pkgPath := ""
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if id, ok := sel.X.(*ast.Ident); ok && !c.isLocal(id.Name) {
			pkgPath = c.pf.imports[id.Name].path
		}
	}
	switch {
	case display == "panic" && !c.isLocal("panic"):
		return "panic"
	case pkgPath == "os" && base == "Exit", base == "Fatal" || base == "Fatalf" || base == "Fatalln":
		return "exit"
	case pkgPath != "" && (base == "Panic" || base == "Panicf" || base == "Panicln"):
		return "panic"
	}
	return ""
}

// isMust: Must, MustX; not Mustard.
func isMust(name string) bool {
	return name == "Must" || (len(name) > 4 && strings.HasPrefix(name, "Must") && unicode.IsUpper(rune(name[4])))
}

// panicsIn finds Must* calls inside expressions, outside function literals.
func (c *fnCtx) panicsIn(exprs ...ast.Expr) []Node {
	var nodes []Node
	for _, e := range exprs {
		if e == nil {
			continue
		}
		ast.Inspect(e, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncLit:
				return false
			case *ast.CallExpr:
				if p, ok := c.panicCall(x, false, x); ok {
					nodes = append(nodes, p)
				}
			}
			return true
		})
	}
	return nodes
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// shortName is the last name in a receiver expression: s.repo -> repo.
func shortName(e ast.Expr) string {
	switch x := unparen(e).(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.CallExpr:
		return shortName(x.Fun) + "()"
	case *ast.IndexExpr:
		return shortName(x.X) + "[]"
	case *ast.StarExpr:
		return shortName(x.X)
	}
	return "expr"
}

const maxLabel = 60

func exprString(e ast.Expr) string {
	s := exprText(e)
	if len(s) > maxLabel {
		s = s[:maxLabel-1] + "…"
	}
	return s
}

func exprText(e ast.Expr) string { //nolint:gocyclo // one case per node kind; splitting would only scatter them
	switch x := e.(type) {
	case nil:
		return ""
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprText(x.X) + "." + x.Sel.Name
	case *ast.BasicLit:
		return x.Value
	case *ast.BinaryExpr:
		return exprText(x.X) + " " + x.Op.String() + " " + exprText(x.Y)
	case *ast.UnaryExpr:
		return x.Op.String() + exprText(x.X)
	case *ast.StarExpr:
		return "*" + exprText(x.X)
	case *ast.CallExpr:
		return exprText(x.Fun) + "()"
	case *ast.ParenExpr:
		return "(" + exprText(x.X) + ")"
	case *ast.IndexExpr:
		return exprText(x.X) + "[" + exprText(x.Index) + "]"
	case *ast.TypeAssertExpr:
		return exprText(x.X) + ".(" + exprText(x.Type) + ")"
	case *ast.CompositeLit:
		return exprText(x.Type) + "{}"
	case *ast.ArrayType:
		return "[]" + exprText(x.Elt)
	case *ast.MapType:
		return "map[" + exprText(x.Key) + "]" + exprText(x.Value)
	case *ast.FuncLit:
		return "func"
	case *ast.SliceExpr:
		return exprText(x.X) + "[:]"
	}
	return "expr"
}

// endsWithExit: the path always leaves here, so nothing after it runs.
func endsWithExit(nodes []Node) bool {
	if len(nodes) == 0 {
		return false
	}
	last := nodes[len(nodes)-1]
	switch last.Kind {
	case "succeed", "fail":
		return true
	case "panic":
		return last.Terminal
	case "loop":
		return Diverges(last)
	case "branch":
		for _, a := range last.Arms {
			if !endsWithExit(a.Body) {
				return false
			}
		}
		return len(last.Arms) > 0
	}
	return false
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// walkNodes visits every node, including arms, loop bodies and step cleanup.
func walkNodes(nodes []Node, visit func(Node)) {
	for _, n := range nodes {
		visit(n)
		walkNodes(n.Cleanup, visit)
		walkNodes(n.Head, visit)
		walkNodes(n.Body, visit)
		walkNodes(n.Post, visit)
		for _, a := range n.Arms {
			walkNodes(a.Body, visit)
		}
	}
}

// NodeFinding is the finding a panic, swallow or untraced recover node raises in fn.
func NodeFinding(fn Function, n Node) (Finding, bool) {
	id := fn.ID
	f := Finding{File: fn.File, Line: n.Line, Confidence: n.Confidence, Function: &id, Allowed: n.Allowed}
	switch n.Kind {
	case "panic":
		f.Rule = NoPanic
		f.Message = fmt.Sprintf("`%s` can panic", n.Name)
		if n.How == "exit" {
			f.Message = fmt.Sprintf("`%s` exits the process", n.Name)
		}
	case "swallow":
		f.Rule = NoSwallow
		f.Message = fmt.Sprintf("`%s` drops the error from `%s`", n.How, n.Name)
	case "recover":
		if !n.Untraced {
			return Finding{}, false
		}
		f.Rule = NoTrace
		f.Message = fmt.Sprintf("the error from `%s` is handled here, and nothing records it", n.Name)
	default:
		return Finding{}, false
	}
	return f, true
}

func findingsOf(fn Function) []Finding {
	var out []Finding
	walkNodes(fn.Nodes, func(n Node) {
		if f, ok := NodeFinding(fn, n); ok {
			out = append(out, f)
		}
	})
	return out
}

func finishCoverage(report *Report) {
	report.Coverage.Functions = len(report.Functions)
	report.Coverage.Unanalysed = len(report.Unanalysed)
	for _, fn := range report.Functions {
		walkNodes(fn.Nodes, func(n Node) {
			if n.Kind != "step" {
				return
			}
			report.Coverage.Steps++
			switch n.Source {
			case ErrorSourceCallee:
				report.Coverage.ErrorFromCallee++
			case ErrorSourceEnclosing:
				report.Coverage.ErrorFromEnclosing++
			default:
				report.Coverage.ErrorUnknown++
			}
		})
	}
}
