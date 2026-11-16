package errpath_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jagreehal/errpath-go"
)

// dump renders nodes compactly so a case can assert the whole railway.
func dump(nodes []errpath.Node) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		parts = append(parts, dumpNode(n))
	}
	return strings.Join(parts, " ; ")
}

func dumpNode(n errpath.Node) string {
	switch n.Kind {
	case "step":
		s := fmt.Sprintf("step %s<%s/%s>", n.Name, errpath.ErrorString(n.Error), n.Source)
		if len(n.Conversions) > 0 {
			s += "[" + strings.Join(n.Conversions, ",") + "]"
		}
		if len(n.Cleanup) > 0 {
			s += "{" + dump(n.Cleanup) + "}"
		}
		return s
	case "fail":
		return "fail " + n.FailError
	case "succeed":
		return "ok"
	case "panic":
		s := n.How + " " + n.Name
		if n.Terminal {
			s += "!"
		}
		return s
	case "swallow":
		return fmt.Sprintf("swallow %s %s", n.How, n.Name)
	case "recover":
		return "recover " + n.Name
	case "jump":
		return n.Name
	case "label":
		return "label " + n.Name
	case "branch", "loop":
		return dumpCompound(n)
	}
	return n.Kind
}

func dumpCompound(n errpath.Node) string {
	var arms []string
	for _, a := range n.Arms {
		arms = append(arms, a.Label+": "+dump(a.Body))
	}
	if n.Kind == "loop" {
		if len(n.Head) > 0 {
			arms = append(arms, "head: "+dump(n.Head))
		}
		arms = append(arms, dump(n.Body))
	}
	return n.Kind + " " + n.Label + " {" + strings.Join(arms, " | ") + "}"
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func analyse(t *testing.T, files map[string]string) errpath.Report {
	t.Helper()
	r, err := errpath.AnalysePath(writeTree(t, files))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func byID(t *testing.T, r errpath.Report, id string) errpath.Function {
	t.Helper()
	for _, f := range r.Functions {
		if f.ID == id {
			return f
		}
	}
	var ids []string
	for _, f := range r.Functions {
		ids = append(ids, f.ID)
	}
	t.Fatalf("no function %s; have %v", id, ids)
	return errpath.Function{}
}

func findingsIn(r errpath.Report, fnID string) []string {
	var out []string
	for _, f := range r.Findings {
		if f.Function != nil && *f.Function == fnID {
			s := f.Rule + " " + f.Message + " " + string(f.Confidence)
			if f.Allowed != nil {
				s += " allowed"
			}
			out = append(out, s)
		}
	}
	return out
}

func TestRailways(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		id    string
		want  string
		error string // returns.error, when set
	}{
		{
			name: "plain value return is success",
			src:  "func Count() int { total := 0\n return total }",
			id:   "p.Count", want: "ok",
		},
		{
			name: "own error ignores non-error results",
			src: `type NotFound struct{}
func (NotFound) Error() string { return "" }
func Load(k string) (int, error) {
	if k == "" { return 0, NotFound{} }
	n := 3
	return n, nil
}`,
			id: "p.Load", want: "branch if k == \"\" {yes: fail NotFound | no: } ; ok", error: "NotFound",
		},
		{
			name: "own error unknown when anything propagates",
			src: `func Inner() error { return nil }
func Outer() error {
	if err := Inner(); err != nil { return err }
	return ErrBad
}
var ErrBad = error(nil)`,
			id: "p.Outer", want: "step Inner<error/unknown> ; fail ErrBad", error: "error",
		},
		{
			name: "checked error that is handled is a recovery",
			src: `func Load() (int, error) { return 0, nil }
func Use() {
	_, err := Load()
	if err != nil {
		println("logged")
	}
}`,
			id: "p.Use", want: "recover Load ; ok",
		},
		{
			name: "returning nil on error is a recovery",
			src: `func Load() (int, error) { return 0, nil }
func Use() error {
	n, err := Load()
	if err != nil {
		return nil
	}
	_ = n
	return nil
}`,
			id: "p.Use", want: "recover Load ; branch if err != nil {yes: ok | no: } ; ok",
		},
		{
			name: "labeled loop keeps its body",
			src: `func F() {
outer:
	for i := 0; i < 3; i++ {
		panic("boom")
		break outer
	}
}`,
			id: "p.F", want: "label outer ; loop outer: for i < 3 {panic panic! ; break outer} ; ok",
		},
		{
			name: "non-error nil check is an ordinary branch",
			src: `func find() *int { return nil }
func F() *int {
	if user := find(); user != nil {
		return user
	}
	return nil
}`,
			id: "p.F", want: "branch if user != nil {yes: ok | no: } ; ok",
		},
		{
			name: "generic receiver",
			src:  "type List[T any] struct{}\nfunc (l *List[T]) Push() error { return nil }",
			id:   "p.List.Push", want: "ok",
		},
		{
			name: "Must in an assignment may panic",
			src:  "func MustParse(s string) int { return 0 }\nfunc F() { x := MustParse(\"1\")\n _ = x }",
			id:   "p.F", want: "panic MustParse ; ok",
		},
		{
			name: "Mustard is not a Must helper",
			src:  "func Mustard() int { return 0 }\nfunc F() { x := Mustard()\n _ = x }",
			id:   "p.F", want: "ok",
		},
		{
			name: "unnamed closures are counted per kind",
			src:  "func F() {\n\tgo func() { panic(1) }()\n\tdefer func() {}()\n\tfunc() { panic(2) }()\n}",
			id:   "p.F.func1", want: "panic panic!",
		},
		{
			name: "closure is its own function",
			src:  "func F() { f := func() { panic(\"x\") }\n f() }",
			id:   "p.F.f", want: "panic panic!",
		},
		{
			name: "wrap, convert and cleanup on the error path",
			src: `import "fmt"
var ErrDB = fmt.Errorf("db")
func A() error { return nil }
func B() error { return nil }
func Log() error { return nil }
func F() error {
	if err := A(); err != nil {
		return fmt.Errorf("a: %w", err)
	}
	if err := B(); err != nil {
		Log()
		return ErrDB
	}
	return nil
}`,
			id: "p.F", want: "step A<error/unknown>[wrap] ; step B<error/unknown>[ErrDB]{swallow discarded return Log} ; ok",
		},
		{
			name: "error path that panics",
			src: `import "log"
func A() error { return nil }
func F() {
	if err := A(); err != nil {
		log.Fatal(err)
	}
}`,
			id: "p.F", want: "step A<error/unknown>{exit log.Fatal!} ; ok",
		},
		{
			name: "forwarded call is a step",
			src:  "func A() (int, error) { return 0, nil }\nfunc F() (int, error) { return A() }",
			id:   "p.F", want: "step A<error/unknown> ; ok",
		},
		{
			name: "returned call in error position is a step",
			src:  "func check() error { return nil }\nfunc F() (int, error) { return 0, check() }",
			id:   "p.F", want: "step check<error/unknown> ; ok",
		},
		{
			name: "constructor in error position is a failure",
			src: `type NotFound struct{}
func (*NotFound) Error() string { return "" }
func NewNotFound() *NotFound { return &NotFound{} }
func F() error { return NewNotFound() }`,
			id: "p.F", want: "fail NotFound",
		},
		{
			name: "switch without default can match nothing",
			src:  "func F(n int) error {\n switch n {\n case 1:\n  return ErrX\n }\n return nil }\nvar ErrX error",
			id:   "p.F", want: "branch switch n {1: fail ErrX | default: } ; ok",
		},
		{
			name: "type switch and select are branches",
			src: `func F(v any, ch chan int) {
	switch v.(type) {
	case int:
		panic("int")
	default:
	}
	select {
	case <-ch:
	default:
		panic("busy")
	}
	if v == nil {
		v = 1
	}
}`,
			id: "p.F", want: "branch switch v.(type) {int: panic panic! | default: } ; branch select {<-ch:  | default: panic panic!} ; ok",
		},
		{
			name: "defer recover",
			src:  "func F() { defer func() { recover() }() }",
			id:   "p.F", want: "recover recover ; ok",
		},
		{
			name: "defer of a named recovering helper",
			src:  "func guard() { recover() }\nfunc F() { defer guard() }",
			id:   "p.F", want: "recover guard ; ok",
		},
		{
			name: "os.Exit ends the path",
			src:  "import \"os\"\nfunc F() { os.Exit(1) }",
			id:   "p.F", want: "exit os.Exit!",
		},
		{
			name: "var declaration step",
			src:  "func A() (int, error) { return 0, nil }\nfunc F() error {\n var n, err = A()\n if err != nil { return err }\n _ = n\n return nil }",
			id:   "p.F", want: "step A<error/unknown> ; ok",
		},
	}
	cases = append(cases, flowCases...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := analyse(t, map[string]string{"p.go": "package p\n\n" + tc.src + "\n"})
			if len(r.ParseFailures) > 0 {
				t.Fatalf("parse: %+v", r.ParseFailures)
			}
			fn := byID(t, r, tc.id)
			if got := dump(fn.Nodes); got != tc.want {
				t.Fatalf("railway\n got: %s\nwant: %s", got, tc.want)
			}
			if tc.error != "" && fn.Returns.Error != tc.error {
				t.Fatalf("returns.error = %q, want %q", fn.Returns.Error, tc.error)
			}
		})
	}
}

func TestFindings(t *testing.T) {
	cases := []struct {
		name string
		src  string
		id   string
		want []string
	}{
		{
			name: "_, err checked is not a dropped error",
			src:  "import \"log/slog\"\nfunc Load() (int, error) { return 0, nil }\nfunc F() { _, err := Load()\n if err != nil { slog.Error(\"load\", \"error\", err) } }",
			id:   "p.F", want: nil,
		},
		{
			name: "handled error that only prints is untraced",
			src:  "func Load() error { return nil }\nfunc F() {\n if err := Load(); err != nil {\n  println(err)\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "std log does not reach a trace",
			src:  "import \"log\"\nfunc Load() error { return nil }\nfunc F() {\n if err := Load(); err != nil {\n  log.Printf(\"%v\", err)\n  return\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "span.RecordError records",
			src:  "import \"go.opentelemetry.io/otel/trace\"\nfunc Load() error { return nil }\nfunc F(span trace.Span) {\n if err := Load(); err != nil {\n  span.RecordError(err)\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "slog logger field records",
			src:  "import \"log/slog\"\ntype S struct{ log *slog.Logger }\nfunc Load() error { return nil }\nfunc (s *S) F() {\n if err := Load(); err != nil {\n  s.log.Warn(\"load\", \"error\", err)\n }\n}",
			id:   "p.S.F", want: nil,
		},
		{
			name: "autotel records",
			src:  "import \"github.com/jagreehal/autotel-go/v2\"\nfunc Load() error { return nil }\nfunc F(ctx context.Context) {\n if err := Load(); err != nil {\n  autotel.RecordError(ctx, err, nil)\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "helper that logs records",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc fail(err error) { slog.Error(\"x\", \"error\", err) }\nfunc F() {\n if err := Load(); err != nil {\n  fail(err)\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "every path must record",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc F(retry bool) {\n if err := Load(); err != nil {\n  if retry {\n   return\n  }\n  slog.Error(\"x\")\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "handing an error on counts",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc F(fatal bool) error {\n if err := Load(); err != nil {\n  if fatal {\n   return err\n  }\n  slog.Error(\"x\")\n }\n return nil\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "logging in a closure does not count",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc F() {\n if err := Load(); err != nil {\n  go func() { slog.Error(\"x\") }()\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "logger of unknown type is possible",
			src:  "func Load() error { return nil }\nfunc F() {\n logger := newLogger()\n if err := Load(); err != nil {\n  logger.Error(\"x\", \"error\", err)\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it possible"},
		},
		{
			name: "slog.Default logger records",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc F() {\n logger := slog.Default().With(\"a\", 1)\n if err := Load(); err != nil {\n  logger.ErrorContext(nil, \"x\", \"error\", err)\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "allow no-trace",
			src:  "func Load() error { return nil }\nfunc F() {\n // errpath: allow(no-trace) a cache miss is expected\n if err := Load(); err != nil {\n  return\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain allowed"},
		},
		{
			name: "wrapping the error to return later hands it on",
			src:  "func Load() error { return nil }\nfunc F() error {\n err := Load()\n if err != nil {\n  err = fmt.Errorf(\"load: %w\", err)\n }\n return err\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "collecting the error hands it on",
			src:  "func Load() error { return nil }\nfunc F(errs []error) {\n if err := Load(); err != nil {\n  errs = append(errs, err)\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "sending the error hands it on",
			src:  "func Load() error { return nil }\nfunc F(ch chan error) {\n if err := Load(); err != nil {\n  ch <- err\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "clearing the error does not",
			src:  "func Load() error { return nil }\nfunc F() error {\n err := Load()\n if err != nil {\n  err = nil\n }\n return err\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "keeping only the message does not",
			src:  "func Load() error { return nil }\nfunc F() string {\n if err := Load(); err != nil {\n  msg := err.Error()\n  return msg\n }\n return \"\"\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "helper that logs only sometimes does not record",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc fail(on bool, err error) {\n if on {\n  slog.Error(\"x\", \"error\", err)\n }\n}\nfunc F(on bool) {\n if err := Load(); err != nil {\n  fail(on, err)\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "helper that returns the error does not record",
			src:  "func Load() error { return nil }\nfunc wrap(err error) error { return err }\nfunc F() {\n if err := Load(); err != nil {\n  println(wrap(err))\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "helper that always exits stops the path",
			src:  "import \"os\"\nfunc Load() error { return nil }\nfunc fail(on bool, err error) {\n println(err)\n os.Exit(1)\n}\nfunc F(on bool) {\n if err := Load(); err != nil {\n  fail(on, err)\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "SetStatus on a project type is not a span",
			src:  "type Response struct{}\nfunc (r *Response) SetStatus(code int) {}\nfunc Load() error { return nil }\nfunc F(r *Response) {\n if err := Load(); err != nil {\n  r.SetStatus(500)\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "SetStatus on an unknown receiver is possible",
			src:  "import (\n\t\"go.opentelemetry.io/otel/codes\"\n\t\"go.opentelemetry.io/otel/trace\"\n)\nfunc Load() error { return nil }\nfunc F() {\n s := get()\n if err := Load(); err != nil {\n  s.SetStatus(codes.Error, \"x\")\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it possible"},
		},
		{
			name: "SetStatus Ok on a span does not record",
			src:  "import (\n\t\"go.opentelemetry.io/otel/codes\"\n\t\"go.opentelemetry.io/otel/trace\"\n)\nfunc Load() error { return nil }\nfunc F(span trace.Span) {\n if err := Load(); err != nil {\n  span.SetStatus(codes.Ok, \"\")\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "SetStatus Error on a span records",
			src:  "import (\n\t\"go.opentelemetry.io/otel/codes\"\n\t\"go.opentelemetry.io/otel/trace\"\n)\nfunc Load() error { return nil }\nfunc F(span trace.Span) {\n if err := Load(); err != nil {\n  span.SetStatus(codes.Error, err.Error())\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "span from a tracer Start records",
			src:  "func Load() error { return nil }\nfunc F(ctx context.Context) {\n ctx, span := tracer.Start(ctx, \"load\")\n defer span.End()\n if err := Load(); err != nil {\n  span.RecordError(err)\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "span from SpanFromContext records",
			src:  "import (\n\t\"go.opentelemetry.io/otel/codes\"\n\t\"go.opentelemetry.io/otel/trace\"\n)\nfunc Load() error { return nil }\nfunc F(ctx context.Context) {\n span := trace.SpanFromContext(ctx)\n if err := Load(); err != nil {\n  span.RecordError(err)\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "assigning the error to blank does not keep it",
			src:  "func Load() error { return nil }\nfunc F() {\n if err := Load(); err != nil {\n  _ = err\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "comparing the error does not keep it",
			src:  "func Load() error { return nil }\nfunc F() bool {\n failed := false\n if err := Load(); err != nil {\n  failed = err != nil\n }\n return failed\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "errors.Is does not keep it",
			src:  "import \"errors\"\nfunc Load() error { return nil }\nvar ErrX = errors.New(\"x\")\nfunc F() bool {\n var miss bool\n if err := Load(); err != nil {\n  miss = errors.Is(err, ErrX)\n }\n return miss\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "converting the error to text does not keep it",
			src:  "import \"fmt\"\nfunc Load() error { return nil }\nfunc F() string {\n if err := Load(); err != nil {\n  msg := fmt.Sprint(err)\n  return msg\n }\n return \"\"\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "fmt.Errorf without %w does not keep it",
			src:  "import \"fmt\"\nfunc Load() error { return nil }\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = fmt.Errorf(\"load: %v\", err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a module function returning an error keeps it",
			src:  "func Load() error { return nil }\ntype E struct{ err error }\nfunc (e *E) Error() string { return \"\" }\nfunc wrap(err error) error { return &E{err} }\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a select clause that returns instead drops it",
			src:  "func Load() error { return nil }\nfunc F(ch chan error, done chan struct{}) {\n if err := Load(); err != nil {\n  select {\n  case ch <- err:\n  case <-done:\n   return\n  }\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "sending in every select case hands it on",
			src:  "func Load() error { return nil }\nfunc F(ch, other chan error) {\n if err := Load(); err != nil {\n  select {\n  case ch <- err:\n  case other <- err:\n  }\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "switch break lands on the log after it",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc F(retry bool) {\n if err := Load(); err != nil {\n  switch {\n  case retry:\n   break\n  }\n  slog.Error(\"x\", \"error\", err)\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "labelled break out of a switch lands after it",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc F(n int) {\n if err := Load(); err != nil {\n sw:\n  switch n {\n  case 1:\n   if n > 0 {\n    break sw\n   }\n  }\n  slog.Error(\"x\", \"error\", err)\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "fallthrough into a clause that logs",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc F(n int) {\n if err := Load(); err != nil {\n  switch n {\n  case 1:\n   fallthrough\n  default:\n   slog.Error(\"x\", \"error\", err)\n  }\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "continue still leaves the handler",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc F(ids []int) {\n for range ids {\n  if err := Load(); err != nil {\n   switch {\n   case true:\n    continue\n   }\n   slog.Error(\"x\")\n  }\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "aliased error status records",
			src:  "import (\n\t\"go.opentelemetry.io/otel/codes\"\n\t\"go.opentelemetry.io/otel/trace\"\n)\nfunc Load() error { return nil }\nconst failure = codes.Error\nfunc F(span trace.Span) {\n if err := Load(); err != nil {\n  span.SetStatus(failure, err.Error())\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "local aliased error status records",
			src:  "import (\n\t\"go.opentelemetry.io/otel/codes\"\n\t\"go.opentelemetry.io/otel/trace\"\n)\nfunc Load() error { return nil }\nfunc F(span trace.Span) {\n const failure = codes.Error\n if err := Load(); err != nil {\n  span.SetStatus(failure, err.Error())\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "aliased ok status does not",
			src:  "import (\n\t\"go.opentelemetry.io/otel/codes\"\n\t\"go.opentelemetry.io/otel/trace\"\n)\nfunc Load() error { return nil }\nconst fine = codes.Ok\nfunc F(span trace.Span) {\n if err := Load(); err != nil {\n  span.SetStatus(fine, \"\")\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "package-level tracer starts a span",
			src:  "import \"go.opentelemetry.io/otel\"\nfunc Load() error { return nil }\nvar tracer = otel.Tracer(\"app\")\nfunc F(ctx context.Context, name string) {\n ctx, span := tracer.Start(ctx, name)\n if err := Load(); err != nil {\n  span.RecordError(err)\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "struct field tracer starts a span",
			src:  "import (\n\t\"go.opentelemetry.io/otel/codes\"\n\t\"go.opentelemetry.io/otel/trace\"\n)\nfunc Load() error { return nil }\ntype S struct{ tracer trace.Tracer }\nfunc (s *S) F(ctx context.Context, name string) {\n ctx, span := s.tracer.Start(ctx, name)\n if err := Load(); err != nil {\n  span.RecordError(err)\n }\n}",
			id:   "p.S.F", want: nil,
		},
		{
			name: "helper with a nil guard records",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc report(err error) {\n if err == nil {\n  return\n }\n slog.Error(\"x\", \"error\", err)\n}\nfunc F() {\n if err := Load(); err != nil {\n  report(err)\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "helper with an inverted nil guard records",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc report(err error) {\n if err != nil {\n  slog.Error(\"x\", \"error\", err)\n }\n}\nfunc F() {\n if err := Load(); err != nil {\n  report(err)\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "helper guarding something else still must record",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc report(on bool, err error) {\n if !on {\n  return\n }\n slog.Error(\"x\", \"error\", err)\n}\nfunc F() {\n if err := Load(); err != nil {\n  report(false, err)\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a return inside a loop skips the log after it",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc F(retry bool) {\n if err := Load(); err != nil {\n  for retry {\n   return\n  }\n  slog.Error(\"x\", \"error\", err)\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a return inside a range loop skips the log",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc F(ids []int) {\n if err := Load(); err != nil {\n  for range ids {\n   return\n  }\n  slog.Error(\"x\", \"error\", err)\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "breaking out of a loop reaches the log",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc F(retry bool) {\n if err := Load(); err != nil {\n  for retry {\n   break\n  }\n  slog.Error(\"x\", \"error\", err)\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a loop that logs before returning is fine",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc F(retry bool) {\n if err := Load(); err != nil {\n  for retry {\n   slog.Error(\"x\")\n   return\n  }\n  slog.Error(\"x\", \"error\", err)\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "an endless loop left only by a logged return",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc F(retry bool) {\n if err := Load(); err != nil {\n  for {\n   if retry {\n    continue\n   }\n   slog.Error(\"x\")\n   return\n  }\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "an endless loop left by an unlogged break",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc F(retry bool) {\n if err := Load(); err != nil {\n  for {\n   if retry {\n    break\n   }\n  }\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "labelled continue to an outer loop in the handler",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc F(rows [][]int) {\n if err := Load(); err != nil {\n outer:\n  for _, r := range rows {\n   for range r {\n    continue outer\n   }\n  }\n  slog.Error(\"x\", \"error\", err)\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "helper fact comes from the argument passed",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc report(err, other error) {\n if other != nil {\n  slog.Error(\"x\", \"error\", other)\n }\n}\nfunc F() {\n if err := Load(); err != nil {\n  report(err, nil)\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "helper fact holds for the argument that is set",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc report(err, other error) {\n if other != nil {\n  slog.Error(\"x\", \"error\", other)\n }\n}\nfunc F() {\n if err := Load(); err != nil {\n  report(nil, err)\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a fact does not survive reassignment",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc F() {\n if err := Load(); err != nil {\n  err = nil\n  if err != nil {\n  slog.Error(\"x\", \"error\", err)\n  }\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a helper fact does not survive reassignment",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc report(err error) {\n err = nil\n if err != nil {\n  slog.Error(\"x\")\n }\n}\nfunc F() {\n if err := Load(); err != nil {\n  report(err)\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a helper returning nil does not keep the error",
			src:  "func Load() error { return nil }\nfunc discard(error) error { return nil }\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = discard(err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a helper returning another error does not keep it",
			src:  "import \"errors\"\nfunc Load() error { return nil }\nvar ErrX = errors.New(\"x\")\nfunc replace(err error) error { return ErrX }\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = replace(err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a nil-guarded wrapping helper keeps it",
			src:  "import \"fmt\"\nfunc Load() error { return nil }\nfunc wrap(err error) error {\n if err == nil {\n  return nil\n }\n return fmt.Errorf(\"wrap: %w\", err)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a helper that reassigns its parameter does not keep it",
			src:  "func Load() error { return nil }\nfunc discard(err error) error {\n err = nil\n return err\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = discard(err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a helper that rewraps its parameter keeps it",
			src:  "import \"fmt\"\nfunc Load() error { return nil }\nfunc wrap(err error) error {\n err = fmt.Errorf(\"wrap: %w\", err)\n return err\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a helper wrapping through a local keeps it",
			src:  "import \"fmt\"\nfunc Load() error { return nil }\nfunc wrap(err error) error {\n wrapped := fmt.Errorf(\"load: %w\", err)\n return wrapped\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a local assigned something else holds nothing",
			src:  "import \"fmt\"\nfunc Load() error { return nil }\nfunc wrap(err error, fresh bool) error {\n wrapped := fmt.Errorf(\"load: %w\", err)\n if fresh {\n  wrapped = fmt.Errorf(\"fresh\")\n }\n return wrapped\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(err, false)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "%w wraps only its own argument",
			src:  "import (\n\t\"errors\"\n\t\"fmt\"\n)\nfunc Load() error { return nil }\nvar ErrX = errors.New(\"x\")\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = fmt.Errorf(\"%w: %v\", ErrX, err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "%w on the handled error keeps it",
			src:  "import (\n\t\"errors\"\n\t\"fmt\"\n)\nfunc Load() error { return nil }\nvar ErrX = errors.New(\"x\")\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = fmt.Errorf(\"%v: %w\", ErrX, err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a later assignment does not justify an earlier return",
			src:  "func Load() error { return nil }\nfunc keep(err, saved error, early bool) error {\n if early {\n  return saved\n }\n saved = err\n return saved\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err, nil, true)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "an assignment before the return justifies it",
			src:  "func Load() error { return nil }\nfunc keep(err, saved error, early bool) error {\n saved = err\n if early {\n  return saved\n }\n return saved\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err, nil, true)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "copying back and forth keeps it",
			src:  "func Load() error { return nil }\nfunc keep(err error) error {\n saved := err\n err = saved\n return err\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "copying back and forth keeps it beside other locals",
			src:  "func Load() error { return nil }\nfunc keep(err error) error {\n a, b, c := 1, 2, 3\n _, _, _ = a, b, c\n saved := err\n err = saved\n return err\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a loop that clears the error loses it",
			src:  "func Load() error { return nil }\nfunc keep(err error, n int) error {\n for i := 0; i < n; i++ {\n  err = nil\n }\n return err\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err, 1)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a loop that leaves the error alone keeps it",
			src:  "func Load() error { return nil }\nfunc keep(err error, n int) error {\n total := 0\n for i := 0; i < n; i++ {\n  total += i\n }\n _ = total\n return err\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err, 1)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a switch break carries what holds",
			src:  "import \"fmt\"\nfunc Load() error { return nil }\nfunc keep(err error, n int) error {\n switch n {\n case 1:\n  err = fmt.Errorf(\"one: %w\", err)\n  break\n case 2:\n  err = nil\n }\n return err\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err, 1)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a bare return with a named result that holds",
			src:  "func Load() error { return nil }\nfunc keep(err error) (out error) {\n out = err\n return\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a variadic wrapper keeps every extra argument",
			src:  "import \"errors\"\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error { return errors.Join(errs...) }\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a variadic wrapper that drops its arguments does not",
			src:  "func Load() error { return nil }\nfunc wrap(errs ...error) error { return nil }\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a variadic helper after a fixed parameter",
			src:  "import \"errors\"\nfunc Load() error { return nil }\nfunc wrap(msg string, errs ...error) error { return errors.Join(errs...) }\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(\"x\", nil, err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a shadowed local does not make the outer one hold",
			src:  "func Load() error { return nil }\nfunc keep(err error) error {\n var saved error\n {\n  saved := err\n  _ = saved\n }\n return saved\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "an inner block assigning the outer local holds",
			src:  "func Load() error { return nil }\nfunc keep(err error) error {\n var saved error\n {\n  saved = err\n }\n return saved\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a deferred write clearing the named result drops it",
			src:  "func Load() error { return nil }\nfunc keep(err error) (out error) {\n out = err\n defer func() { out = nil }()\n return\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a deferred wrap of the named result keeps it",
			src:  "import \"fmt\"\nfunc Load() error { return nil }\nfunc keep(err error) (out error) {\n defer func() { out = fmt.Errorf(\"keep: %w\", out) }()\n return err\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a nested wrapper keeps the nil guard's fact",
			src:  "import \"fmt\"\nfunc Load() error { return nil }\nfunc inner(err error) error {\n if err == nil {\n  return nil\n }\n return fmt.Errorf(\"inner: %w\", err)\n}\nfunc keep(err error) error { return inner(err) }\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a deferred closure's own local leaves the result alone",
			src:  "func Load() error { return nil }\nfunc keep(err error) (out error) {\n defer func() {\n  out := error(nil)\n  _ = out\n }()\n return err\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a defer inside an unused closure is never registered",
			src:  "func Load() error { return nil }\nfunc keep(err error) (out error) {\n _ = func() { defer func() { out = nil }() }\n return err\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a deferred reset through the result's address drops it",
			src:  "func Load() error { return nil }\nfunc reset(p *error) { *p = nil }\nfunc keep(err error) (out error) {\n defer reset(&out)\n return err\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a reset through the parameter's address drops it",
			src:  "func Load() error { return nil }\nfunc reset(p *error) { *p = nil }\nfunc keep(err error) error {\n reset(&err)\n return err\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a reset through the address ends the handler's non-nil fact",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc reset(p *error) { *p = nil }\nfunc F() {\n if err := Load(); err != nil {\n  reset(&err)\n  if err != nil {\n   slog.Error(\"load\", \"error\", err)\n  }\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "clearing an element of a variadic slice drops it",
			src:  "import \"errors\"\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n errs[1] = nil\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "clearing a field of a wrapper drops it",
			src:  "func Load() error { return nil }\ntype Wrapped struct {\n Op  string\n Err error\n}\nfunc (w *Wrapped) Error() string { return w.Op }\nfunc keep(err error) error {\n out := &Wrapped{Err: err}\n out.Err = nil\n return out\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "setting a literal field of a wrapper keeps it",
			src:  "func Load() error { return nil }\ntype Wrapped struct {\n Op  string\n Err error\n}\nfunc (w *Wrapped) Error() string { return w.Op }\nfunc keep(err error) error {\n out := &Wrapped{Err: err}\n out.Op = \"load\"\n return out\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "storing the error in a field makes the wrapper hold it",
			src:  "func Load() error { return nil }\ntype Wrapped struct {\n Op  string\n Err error\n}\nfunc (w *Wrapped) Error() string { return w.Op }\nfunc keep(err error) error {\n out := &Wrapped{}\n out.Err = err\n return out\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "clearing an element through an alias drops it",
			src:  "import \"errors\"\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n alias := errs\n alias[1] = nil\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "clearing a field through an aliased pointer drops it",
			src:  "func Load() error { return nil }\ntype Wrapped struct {\n Op  string\n Err error\n}\nfunc (w *Wrapped) Error() string { return w.Op }\nfunc keep(err error) error {\n out := &Wrapped{Err: err}\n p := out\n p.Err = nil\n return out\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a literal written to an any field replaces the error",
			src:  "func Load() error { return nil }\ntype Any struct {\n Err any\n}\nfunc (a *Any) Error() string { return \"any\" }\nfunc keep(err error) error {\n out := &Any{Err: err}\n out.Err = \"none\"\n return out\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "clearing a wrapper's field leaves the plain parameter",
			src:  "func Load() error { return nil }\ntype Wrapped struct {\n Op  string\n Err error\n}\nfunc (w *Wrapped) Error() string { return w.Op }\nfunc keep(err error) error {\n out := &Wrapped{Err: err}\n out.Err = nil\n _ = out\n return err\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "clear empties a slice that held it",
			src:  "import \"errors\"\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n clear(errs)\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "clear through an alias empties it too",
			src:  "import \"errors\"\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n alias := errs\n clear(alias)\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "copy over a slice that held it drops it",
			src:  "import \"errors\"\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n copy(errs, []error{nil, nil})\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "append into a slice's own storage drops it",
			src:  "import \"errors\"\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n _ = append(errs[:1], nil)\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "slices.Delete on a slice that held it drops it",
			src:  "import (\n \"errors\"\n \"slices\"\n)\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n _ = slices.Delete(errs, 0, 2)\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "copy out of a slice leaves it alone",
			src:  "import \"errors\"\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n dst := make([]error, 2)\n copy(dst, errs)\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "clearing a field through a type assertion drops it",
			src:  "func Load() error { return nil }\ntype Wrapped struct {\n Op  string\n Err error\n}\nfunc (w *Wrapped) Error() string { return w.Op }\nfunc keep(err error) error {\n var out error = &Wrapped{Err: err}\n out.(*Wrapped).Err = nil\n return out\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a module helper that zeroes the slice drops it",
			src:  "import (\n \"errors\"\n)\nfunc Load() error { return nil }\nfunc zero(es []error) {\n for i := range es {\n  es[i] = nil\n }\n}\nfunc wrap(errs ...error) error {\n zero(errs)\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a module helper that only reads the slice keeps it",
			src:  "import (\n \"errors\"\n)\nfunc Load() error { return nil }\nfunc count(es []error) int { return len(es) }\nfunc wrap(errs ...error) error {\n _ = count(errs)\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a helper writing through a local alias of its parameter drops it",
			src:  "import (\n \"errors\"\n)\nfunc Load() error { return nil }\nfunc zero(es []error) {\n a := es\n a[0], a[1] = nil, nil\n}\nfunc wrap(errs ...error) error {\n zero(errs)\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "sort.Slice with a writing callback drops it",
			src:  "import (\n \"errors\"\n \"sort\"\n)\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n sort.Slice(errs, func(i, j int) bool {\n  errs[i] = nil\n  return false\n })\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "sort.Slice with a reading callback keeps it",
			src:  "import (\n \"errors\"\n \"sort\"\n)\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n sort.Slice(errs, func(i, j int) bool { return errs[i] == nil })\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a function from outside the module may write through its argument",
			src:  "import (\n \"errors\"\n \"example.com/ext\"\n)\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n ext.Mangle(errs)\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "append into an alias's storage drops it",
			src:  "import (\n \"errors\"\n)\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n alias := errs[:1]\n alias = append(alias, nil)\n _ = alias\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "collecting with append keeps it",
			src:  "import \"errors\"\nfunc Load() error { return nil }\nfunc keep(err error) error {\n var errs []error\n errs = append(errs, err)\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "copy in a condition drops it",
			src:  "import \"errors\"\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n if copy(errs, []error{nil, nil}) > 0 {\n  return errors.Join(errs...)\n }\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "an explicit generic slices.Delete drops it",
			src:  "import (\n \"errors\"\n \"slices\"\n)\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n _ = slices.Delete[[]error](errs, 0, len(errs))\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a method that clears its receiver drops it",
			src:  "func Load() error { return nil }\ntype Box struct{ Err error }\nfunc (b *Box) Error() string { return \"box\" }\nfunc (b *Box) Reset() { b.Err = nil }\nfunc (b *Box) Name() string { return \"box\" }\nfunc keep(err error) error {\n b := &Box{Err: err}\n b.Reset()\n return b\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a method that only reads its receiver keeps it",
			src:  "func Load() error { return nil }\ntype Box struct{ Err error }\nfunc (b *Box) Error() string { return \"box\" }\nfunc (b *Box) Reset() { b.Err = nil }\nfunc (b *Box) Name() string { return \"box\" }\nfunc keep(err error) error {\n b := &Box{Err: err}\n _ = b.Name()\n return b\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "an explicit generic call to a reading function keeps it",
			src:  "import (\n \"errors\"\n \"slices\"\n)\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n _ = slices.Contains[[]error](errs, nil)\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(nil, err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a case expression's write reaches the default",
			src:  "import (\n \"errors\"\n \"log/slog\"\n)\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n switch 0 {\n case copy(errs, []error{nil}):\n  slog.Error(\"matched\")\n  return nil\n default:\n  return errors.Join(errs...)\n }\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a case expression's write reaches later cases",
			src:  "import (\n \"errors\"\n \"log/slog\"\n)\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n switch 0 {\n case copy(errs, []error{nil}):\n  slog.Error(\"matched\")\n  return nil\n case 1:\n  return errors.Join(errs...)\n }\n return errors.Join(errs...)\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a later case expression's write reaches the default before it",
			src:  "import (\n \"errors\"\n \"log/slog\"\n)\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n switch 0 {\n default:\n  return errors.Join(errs...)\n case copy(errs, []error{nil}):\n  slog.Error(\"matched\")\n  return nil\n }\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a later case expression's write doesn't reach an earlier case",
			src:  "import (\n \"errors\"\n \"log/slog\"\n)\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n switch 0 {\n case 0:\n  return errors.Join(errs...)\n case copy(errs, []error{nil}):\n  slog.Error(\"matched\")\n  return nil\n default:\n  slog.Error(\"none\")\n  return nil\n }\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a receive's assignment target runs only in its own case",
			src:  "import (\n \"errors\"\n \"log/slog\"\n)\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n ch := make(chan int)\n slot := map[int]int{}\n select {\n case slot[copy(errs, []error{nil})] = <-ch:\n  slog.Error(\"received\")\n  return nil\n default:\n  return errors.Join(errs...)\n }\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(err)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a receive's assignment target runs in its own case",
			src:  "import (\n \"errors\"\n \"log/slog\"\n)\nfunc Load() error { return nil }\nfunc wrap(errs ...error) error {\n ch := make(chan int)\n slot := map[int]int{}\n select {\n case slot[copy(errs, []error{nil})] = <-ch:\n  return errors.Join(errs...)\n default:\n  slog.Error(\"none\")\n  return nil\n }\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "a select evaluates every case's channel on entry",
			src:  "import (\n \"errors\"\n \"log/slog\"\n)\nfunc Load() error { return nil }\nfunc pick(ch chan int, n int) chan int { return ch }\nfunc wrap(errs ...error) error {\n ch := make(chan int)\n slot := map[int]int{}\n select {\n case <-pick(ch, copy(errs, []error{nil})):\n  slog.Error(\"received\")\n  return nil\n default:\n  return errors.Join(errs...)\n }\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = wrap(err)\n }\n return out\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "recording in a select's send value counts on every path",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc record(err error) error {\n slog.Error(\"load\", \"error\", err)\n return err\n}\nfunc F(ch chan error) {\n if err := Load(); err != nil {\n  select {\n  case ch <- record(err):\n  default:\n  }\n }\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "a helper recording in a select's send value counts on every path",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc record(err error) error {\n slog.Error(\"load\", \"error\", err)\n return err\n}\nfunc keep(err error, fast bool) error {\n if fast {\n  return err\n }\n ch := make(chan error, 1)\n select {\n case ch <- record(err):\n default:\n }\n return nil\n}\nfunc F() error {\n var out error\n if err := Load(); err != nil {\n  out = keep(err, false)\n }\n return out\n}",
			id:   "p.F", want: nil,
		},
		{
			name: "recording only in a select case's body doesn't cover the default",
			src:  "import \"log/slog\"\nfunc Load() error { return nil }\nfunc record(err error) error {\n slog.Error(\"load\", \"error\", err)\n return err\n}\nfunc F(ch chan int) {\n if err := Load(); err != nil {\n  select {\n  case <-ch:\n   slog.Error(\"load\", \"error\", err)\n  default:\n  }\n }\n}",
			id:   "p.F", want: []string{"no-trace the error from `Load` is handled here, and nothing records it certain"},
		},
		{
			name: "blank in the error position drops it",
			src:  "func Load() (int, error) { return 0, nil }\nfunc F() { n, _ := Load()\n _ = n }",
			id:   "p.F", want: []string{"no-swallow `_` drops the error from `Load` certain"},
		},
		{
			name: "_ = on an error-only call",
			src:  "func Save() error { return nil }\nfunc F() { _ = Save() }",
			id:   "p.F", want: []string{"no-swallow `_ =` drops the error from `Save` certain"},
		},
		{
			name: "method resolved by receiver type, not by name",
			src: `type Cache struct{}
func (Cache) Save() {}
type Store struct{}
func (Store) Save() error { return nil }
func F(c Cache, s *Store) {
	c.Save()
	s.Save()
}`,
			id: "p.F", want: []string{"no-swallow `discarded return` drops the error from `s.Save` certain"},
		},
		{
			name: "interface field method",
			src: `type Repo interface { Save() error }
type Service struct{ repo Repo }
func (s *Service) Do() {
	s.repo.Save()
}`,
			id: "p.Service.Do", want: []string{"no-swallow `discarded return` drops the error from `repo.Save` certain"},
		},
		{
			name: "unknown receiver matched by name is possible",
			src: `type Store struct{}
func (Store) Flush() error { return nil }
func F(get func() any) {
	x := get()
	x.(interface{ Flush() error }).Flush()
}`,
			id: "p.F", want: []string{"no-swallow `discarded return` drops the error from `expr.Flush` possible"},
		},
		{
			name: "external receiver is not guessed",
			src: `import "os"
type Store struct{}
func (Store) Close() error { return nil }
func F(f *os.File) { f.Close() }`,
			id: "p.F", want: nil,
		},
		{
			name: "allow on the line above",
			src:  "func F() {\n\t// errpath: allow(no-panic) startup only\n\tpanic(\"x\")\n}",
			id:   "p.F", want: []string{"no-panic `panic` can panic certain allowed"},
		},
		{
			name: "allow for another rule does not apply",
			src:  "func F() {\n\t// errpath: allow(no-swallow) nope\n\tpanic(\"x\")\n}",
			id:   "p.F", want: []string{"no-panic `panic` can panic certain"},
		},
		{
			name: "panic in a labeled loop",
			src:  "func F() {\nL:\n\tfor {\n\t\tpanic(1)\n\t\tbreak L\n\t}\n}",
			id:   "p.F", want: []string{"no-panic `panic` can panic certain"},
		},
		{
			name: "dropped error in error cleanup",
			src:  "func A() error { return nil }\nfunc F() error {\n if err := A(); err != nil {\n  A()\n  return err\n }\n return nil }",
			id:   "p.F", want: []string{"no-swallow `discarded return` drops the error from `A` certain"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := analyse(t, map[string]string{"p.go": "package p\n\n" + tc.src + "\n"})
			if len(r.ParseFailures) > 0 {
				t.Fatalf("parse: %+v", r.ParseFailures)
			}
			got := findingsIn(r, tc.id)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("findings\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestBadAllows(t *testing.T) {
	r := analyse(t, map[string]string{"p.go": `package p

func F() {
	// errpath: allow(no-panic)
	panic(1)
	// errpath: allow(no-crash) because
	// errpath: allow(no-panic oops
}
`})
	var got []string
	for _, f := range r.Findings {
		if f.Rule == errpath.BadAllow {
			if f.Function != nil {
				t.Fatalf("bad-allow has a function: %+v", f)
			}
			got = append(got, fmt.Sprintf("%d %s", f.Line, f.Message))
		}
	}
	want := []string{
		"4 allow(no-panic) needs a reason, so it was ignored",
		"6 unknown rule `no-crash` in allow comment",
		"7 unclosed allow comment",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if f := findingsIn(r, "p.F"); len(f) != 1 || strings.HasSuffix(f[0], "allowed") {
		t.Fatalf("an allow without a reason must not silence: %q", f)
	}
}

func TestCrossPackageResolution(t *testing.T) {
	r := analyse(t, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.22\n",
		"store/store.go": `package store

type DbError struct{}

func (*DbError) Error() string { return "db" }

// errpath: error DbError
func Save() error { return nil }

type Repo struct{}

func (r *Repo) Get() (int, error) { return 0, &DbError{} }

func Open() *Repo { return &Repo{} }
`,
		"api/api.go": `package api

import (
	db "example.com/app/store"
)

func Handle() error {
	if err := db.Save(); err != nil {
		return err
	}
	repo := db.Open()
	n, err := repo.Get()
	if err != nil {
		return err
	}
	_ = n
	return nil
}
`,
		"api/api_test.go": "package api\n",
	})
	fn := byID(t, r, "api.Handle")
	if got, want := dump(fn.Nodes), "step db.Save<DbError/callee> ; step repo.Get<DbError/callee> ; ok"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	byID(t, r, "store.Repo.Get")
	if r.SupportFilesSkipped != 1 {
		t.Fatalf("support files skipped = %d", r.SupportFilesSkipped)
	}
}

func TestDuplicateIDsAreNumbered(t *testing.T) {
	r := analyse(t, map[string]string{
		"a_linux.go":   "//go:build linux\n\npackage p\n\nfunc Open() error { return nil }\n",
		"a_windows.go": "//go:build windows\n\npackage p\n\nfunc Open() error { panic(1) }\n",
	})
	byID(t, r, "p.Open")
	byID(t, r, "p.Open#2")
}

func TestParseFailureIsReported(t *testing.T) {
	r := analyse(t, map[string]string{"ok.go": "package p\nfunc F() {}\n", "bad.go": "package p\nfunc {"})
	if len(r.ParseFailures) != 1 || r.ParseFailures[0].File != "bad.go" {
		t.Fatalf("parse failures = %+v", r.ParseFailures)
	}
	byID(t, r, "p.F")
}

func TestNothingIsLeftUnanalysed(t *testing.T) {
	r := analyse(t, map[string]string{"p.go": "package p\nfunc F() (err error) {\n\tgoto end\nend:\n\treturn\n}\n"})
	if len(r.Unanalysed) != 0 {
		t.Fatalf("unanalysed = %+v", r.Unanalysed)
	}
}

func TestDiagramShowsCleanupAndConversion(t *testing.T) {
	r := analyse(t, map[string]string{"p.go": `package p

var ErrDB error

func A() error { return nil }
func Log()     {}

func F() error {
	if err := A(); err != nil {
		Log()
		return ErrDB
	}
	return nil
}
`})
	d := errpath.RenderFunction(byID(t, r, "p.F"))
	if !strings.Contains(d, `[/"ErrDB"/]`) {
		t.Fatalf("diagram should exit as ErrDB:\n%s", d)
	}
	m := errpath.FunctionToMachine(byID(t, r, "p.F"))
	if _, ok := m.States["ErrDB"]; !ok {
		t.Fatalf("machine should have ErrDB final: %v", m.States)
	}
}

func TestDiagramLabelsCannotBreakTheFence(t *testing.T) {
	r := analyse(t, map[string]string{"p.go": "package p\n\nfunc F(s string, ch chan int) {\n\tif s == \"```<img>\" {\n\t\tpanic(1)\n\t}\n}\n"})
	d := errpath.RenderFunction(byID(t, r, "p.F"))
	if strings.Contains(d, "`") || strings.Contains(d, "<img>") {
		t.Fatalf("unescaped label:\n%s", d)
	}
}

func TestHelperWithoutBodyIsNotARecorder(t *testing.T) {
	r := analyse(t, map[string]string{"p.go": "package p\nfunc Load() error { return nil }\nfunc asm(err error)\nfunc F() {\n\tif err := Load(); err != nil {\n\t\tasm(err)\n\t}\n}\n"})
	if got := findingsIn(r, "p.F"); len(got) != 1 {
		t.Fatalf("findings: %q", got)
	}
}

// Each statement is judged once, so many branches in a row stay fast.
func TestManyBranchesInAHandlerStayLinear(t *testing.T) {
	var b strings.Builder
	b.WriteString("package p\nimport \"log/slog\"\nfunc Load() error { return nil }\nfunc F(x bool) {\n\tif err := Load(); err != nil {\n")
	for range 200 {
		b.WriteString("\t\tif x {\n\t\t}\n")
	}
	b.WriteString("\t\tslog.Error(\"x\", \"error\", err)\n\t}\n}\n")
	start := time.Now()
	r := analyse(t, map[string]string{"p.go": b.String()})
	if got := findingsIn(r, "p.F"); len(got) != 0 {
		t.Fatalf("findings: %q", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("200 branches took %s", d)
	}
}
