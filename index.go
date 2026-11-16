package errpath

import (
	"bufio"
	"go/ast"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Syntax-only name resolution. Calls resolve through the file's imports, the
// package's own declarations, and the declared types of locals, params and
// struct fields. Anything else (external types, interface values from other
// modules, function values) stays unresolved rather than guessed.

var errorHintRe = regexp.MustCompile(`errpath:\s*error\s+(\S+(?:\s*\|\s*\S+)*)`)

type world struct {
	pkgs    map[string]*pkgInfo // by slash dir relative to the root
	modules map[string]string   // module root dir -> module path
	methods map[string][]*fnSig // method name -> every indexed method
	records map[recordKey]bool  // what a function does with an error it's given (trace.go)
	typing  map[string]bool     // package values whose type is being worked out, against cycles
}

type pkgInfo struct {
	dir   string
	name  string
	id    string // function id prefix
	funcs map[string]*fnSig
	types map[string]*typeInfo
	// values are package-level vars and consts, for their types and aliases.
	values map[string]pkgValue
}

type pkgValue struct {
	file       *parsedFile
	typ, value ast.Expr // either may be nil
	isConst    bool
}

type typeInfo struct {
	file     *parsedFile
	fields   map[string]typeRef
	embedded []typeRef
	methods  map[string]*fnSig // interface methods
}

// typeRef is a named type. pkg is nil for types outside the analysed tree.
type typeRef struct {
	pkg  *pkgInfo
	name string
	path string // import path, for a type outside the analysed tree
}

func (t typeRef) known() bool { return t.pkg != nil && t.name != "" }

type fnSig struct {
	pkg       *pkgInfo
	recv      string
	name      string
	results   []ast.Expr // one entry per result value
	errIdx    int        // position of the error result, or -1
	errorType string     // hinted or inferred; "" when unknown
	decl      *ast.FuncDecl
	file      *parsedFile
}

type parsedFile struct {
	rel     string
	fset    *token.FileSet
	file    *ast.File
	pkg     *pkgInfo
	imports map[string]importRef
}

type importRef struct {
	path string
	pkg  *pkgInfo // nil when outside the analysed tree
}

func newWorld() *world {
	return &world{pkgs: map[string]*pkgInfo{}, modules: map[string]string{}, methods: map[string][]*fnSig{}, records: map[recordKey]bool{}, typing: map[string]bool{}}
}

func (w *world) pkgFor(dir, name string) *pkgInfo {
	if p, ok := w.pkgs[dir]; ok {
		return p
	}
	id := dir
	if dir == "." {
		id = name
	}
	p := &pkgInfo{dir: dir, name: name, id: id, funcs: map[string]*fnSig{}, types: map[string]*typeInfo{}, values: map[string]pkgValue{}}
	w.pkgs[dir] = p
	return p
}

// readModule returns the module path declared in a go.mod, or "".
func readModule(goMod string) string {
	f, err := os.Open(goMod)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }() // read-only: nothing to flush
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if rest, ok := strings.CutPrefix(line, "module"); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}

// findModuleAbove finds the go.mod at or above dir and returns the import path of dir.
func findModuleAbove(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for d, rel := abs, ""; ; {
		if mod := readModule(filepath.Join(d, "go.mod")); mod != "" {
			return path.Join(mod, rel)
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		rel = path.Join(filepath.Base(d), rel)
		d = parent
	}
}

// importPath of an analysed package dir, or "" when no module is known.
func (w *world) importPath(dir string) string {
	best, bestMod := "", ""
	for root, mod := range w.modules {
		if (root == "." || dir == root || strings.HasPrefix(dir, root+"/")) && len(root) >= len(best) {
			best, bestMod = root, mod
		}
	}
	if bestMod == "" {
		return ""
	}
	if best == "." {
		if dir == "." {
			return bestMod
		}
		return bestMod + "/" + dir
	}
	return path.Join(bestMod, strings.TrimPrefix(dir, best))
}

func (w *world) resolveImport(importPath string) *pkgInfo {
	var suffixHit *pkgInfo
	hits := 0
	for dir, p := range w.pkgs {
		if ip := w.importPath(dir); ip != "" {
			if ip == importPath {
				return p
			}
			continue
		}
		if dir != "." && strings.HasSuffix(importPath, "/"+dir) {
			suffixHit, hits = p, hits+1
		}
	}
	if hits == 1 {
		return suffixHit
	}
	return nil
}

// defaultImportName guesses the package name of an external import path.
func defaultImportName(p string) string {
	parts := strings.Split(p, "/")
	last := parts[len(parts)-1]
	if len(parts) > 1 && len(last) > 1 && last[0] == 'v' && strings.Trim(last[1:], "0123456789") == "" {
		last = parts[len(parts)-2]
	}
	// As goimports guesses: drop a go- prefix, then stop at the first character
	// a name can't hold (autotel-go → autotel, yaml.v3 → yaml).
	last = strings.TrimPrefix(last, "go-")
	if i := strings.IndexAny(last, "-."); i > 0 {
		last = last[:i]
	}
	return last
}

func (w *world) bindImports(pf *parsedFile) {
	pf.imports = map[string]importRef{}
	for _, spec := range pf.file.Imports {
		p := strings.Trim(spec.Path.Value, "`\"")
		ref := importRef{path: p, pkg: w.resolveImport(p)}
		name := defaultImportName(p)
		if ref.pkg != nil {
			name = ref.pkg.name
		}
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "_" || name == "." {
			continue
		}
		pf.imports[name] = ref
	}
}

// indexDecls records types and function signatures of one file.
func (w *world) indexDecls(pf *parsedFile) {
	for _, decl := range pf.file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name == nil || isErrorMethod(d) {
				continue
			}
			recv := ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recv = recvTypeName(d.Recv.List[0].Type)
			}
			sig := newSig(pf.pkg, recv, d.Name.Name, d.Type, pf)
			sig.decl = d
			key := d.Name.Name
			if recv != "" {
				key = recv + "." + key
				w.methods[d.Name.Name] = append(w.methods[d.Name.Name], sig)
			}
			if _, dup := pf.pkg.funcs[key]; !dup {
				pf.pkg.funcs[key] = sig
			}
		case *ast.GenDecl:
			if d.Tok == token.VAR || d.Tok == token.CONST {
				indexValues(pf, d)
				continue
			}
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				ts := spec.(*ast.TypeSpec)
				pf.pkg.types[ts.Name.Name] = w.typeInfoOf(pf, ts)
			}
		}
	}
}

func indexValues(pf *parsedFile, d *ast.GenDecl) {
	for _, spec := range d.Specs {
		vs := spec.(*ast.ValueSpec)
		for i, n := range vs.Names {
			v := pkgValue{file: pf, typ: vs.Type, isConst: d.Tok == token.CONST}
			if len(vs.Values) == len(vs.Names) {
				v.value = vs.Values[i]
			}
			if _, dup := pf.pkg.values[n.Name]; !dup {
				pf.pkg.values[n.Name] = v
			}
		}
	}
}

func newSig(pkg *pkgInfo, recv, name string, ft *ast.FuncType, pf *parsedFile) *fnSig {
	sig := &fnSig{pkg: pkg, recv: recv, name: name, errIdx: -1, file: pf}
	if ft.Results != nil {
		for _, f := range ft.Results.List {
			n := max(len(f.Names), 1)
			for range n {
				if isErrorType(f.Type) {
					sig.errIdx = len(sig.results)
				}
				sig.results = append(sig.results, f.Type)
			}
		}
	}
	return sig
}

func (w *world) typeInfoOf(pf *parsedFile, ts *ast.TypeSpec) *typeInfo {
	ti := &typeInfo{file: pf, fields: map[string]typeRef{}, methods: map[string]*fnSig{}}
	switch t := ts.Type.(type) {
	case *ast.StructType:
		for _, f := range t.Fields.List {
			ref := typeRefOf(pf, f.Type)
			if len(f.Names) == 0 {
				ti.embedded = append(ti.embedded, ref)
				ti.fields[ref.name] = ref
				continue
			}
			for _, n := range f.Names {
				ti.fields[n.Name] = ref
			}
		}
	case *ast.InterfaceType:
		for _, m := range t.Methods.List {
			ft, ok := m.Type.(*ast.FuncType)
			if !ok {
				ti.embedded = append(ti.embedded, typeRefOf(pf, m.Type))
				continue
			}
			for _, n := range m.Names {
				sig := newSig(pf.pkg, ts.Name.Name, n.Name, ft, pf)
				ti.methods[n.Name] = sig
				w.methods[n.Name] = append(w.methods[n.Name], sig)
			}
		}
	}
	return ti
}

// typeRefOf names the type a type expression refers to. pkg is filled in
// later for selectors, since imports bind after all packages are known.
func typeRefOf(pf *parsedFile, expr ast.Expr) typeRef {
	switch t := expr.(type) {
	case *ast.Ident:
		return typeRef{pkg: pf.pkg, name: t.Name}
	case *ast.StarExpr:
		return typeRefOf(pf, t.X)
	case *ast.ParenExpr:
		return typeRefOf(pf, t.X)
	case *ast.IndexExpr:
		return typeRefOf(pf, t.X)
	case *ast.IndexListExpr:
		return typeRefOf(pf, t.X)
	case *ast.SelectorExpr:
		if id, ok := t.X.(*ast.Ident); ok && pf.imports != nil {
			if imp, ok := pf.imports[id.Name]; ok {
				return typeRef{pkg: imp.pkg, name: t.Sel.Name, path: imp.path}
			}
		}
		// Import aliases are unknown while indexing: remember the alias.
		if id, ok := t.X.(*ast.Ident); ok {
			return typeRef{name: id.Name + "." + t.Sel.Name}
		}
	}
	return typeRef{}
}

// fixRefs resolves "alias.Type" refs recorded before imports were bound.
func (w *world) fixRefs() {
	fix := func(pf *parsedFile, r typeRef) typeRef {
		alias, name, ok := strings.Cut(r.name, ".")
		if r.pkg != nil || !ok {
			return r
		}
		if imp, ok := pf.imports[alias]; ok {
			return typeRef{pkg: imp.pkg, name: name, path: imp.path}
		}
		return r
	}
	for _, p := range w.pkgs {
		for _, ti := range p.types {
			for k, r := range ti.fields {
				ti.fields[k] = fix(ti.file, r)
			}
			for i, r := range ti.embedded {
				ti.embedded[i] = fix(ti.file, r)
			}
		}
	}
}

// method finds a method on a named type, following embedded fields.
func (w *world) method(t typeRef, name string, depth int) *fnSig {
	if !t.known() || depth > 4 {
		return nil
	}
	if sig := t.pkg.funcs[t.name+"."+name]; sig != nil {
		return sig
	}
	ti := t.pkg.types[t.name]
	if ti == nil {
		return nil
	}
	if sig := ti.methods[name]; sig != nil {
		return sig
	}
	for _, e := range ti.embedded {
		if sig := w.method(e, name, depth+1); sig != nil {
			return sig
		}
	}
	return nil
}

func (w *world) field(t typeRef, name string) typeRef {
	if !t.known() {
		return typeRef{}
	}
	if ti := t.pkg.types[t.name]; ti != nil {
		if r, ok := ti.fields[name]; ok {
			return r
		}
		for _, e := range ti.embedded {
			if r := w.field(e, name); r.name != "" {
				return r
			}
		}
	}
	return typeRef{}
}

// agreeingMethod is any method of that name when every indexed method of the
// name returns an error in the same position, so the guess can't be wrong
// about there being an error.
func (w *world) agreeingMethod(name string) *fnSig {
	cands := w.methods[name]
	if len(cands) == 0 {
		return nil
	}
	for _, s := range cands {
		if s.errIdx < 0 || s.errIdx != cands[0].errIdx || len(s.results) != len(cands[0].results) {
			return nil
		}
	}
	return cands[0]
}

// isErrorMethod is the error interface method; not an error railway.
func isErrorMethod(fd *ast.FuncDecl) bool {
	if fd.Recv == nil || fd.Name.Name != "Error" || fieldCount(fd.Type.Params) > 0 {
		return false
	}
	if fd.Type.Results == nil || fieldCount(fd.Type.Results) != 1 {
		return false
	}
	id, ok := fd.Type.Results.List[0].Type.(*ast.Ident)
	return ok && id.Name == "string"
}

func fieldCount(fl *ast.FieldList) int {
	if fl == nil {
		return 0
	}
	n := 0
	for _, f := range fl.List {
		n += max(len(f.Names), 1)
	}
	return n
}

func isErrorType(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "error"
}

// recvTypeName is the receiver's type name, without pointer or type parameters.
func recvTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.ParenExpr:
		return recvTypeName(t.X)
	case *ast.IndexExpr:
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	}
	return ""
}

func errorHint(fd *ast.FuncDecl) string {
	if fd.Doc == nil {
		return ""
	}
	for _, c := range fd.Doc.List {
		if m := errorHintRe.FindStringSubmatch(c.Text); m != nil {
			return m[1]
		}
	}
	return ""
}
