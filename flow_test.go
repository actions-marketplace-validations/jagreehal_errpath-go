package errpath_test

import (
	"testing"

	"github.com/jagreehal/errpath-go"
)

const flowPrelude = `import "errors"
var ErrX = errors.New("x")
var ErrSkip = errors.New("skip")
func A() (int, error) { return 0, nil }
func MustParse(s string) int { return 0 }
`

// flowCases: checks further from the call, and what error variables hold.
var flowCases = []struct {
	name  string
	src   string
	id    string
	want  string
	error string
}{
	{
		name: "check a few lines after the call",
		src:  flowPrelude + "func F() error {\n\tn, err := A()\n\tx := MustParse(\"1\") + n\n\t_ = x\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}",
		id:   "p.F", want: "panic MustParse ; step A<error/unknown> ; ok",
	},
	{
		name: "errors.Is before the nil check",
		src:  flowPrelude + "func F() error {\n\t_, err := A()\n\tif errors.Is(err, ErrSkip) {\n\t\treturn nil\n\t}\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}",
		id:   "p.F", want: "branch if errors.Is() {yes: ok | no: } ; step A<error/unknown> ; ok",
	},
	{
		name: "reassigned before the check pairs with the second call",
		src:  flowPrelude + "func B() (int, error) { return 0, nil }\nfunc F() error {\n\t_, err := A()\n\t_, err = B()\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}",
		id:   "p.F", want: "step B<error/unknown> ; ok",
	},
	{
		name: "return v, err passes the callee's error on",
		src:  flowPrelude + "func F() (int, error) {\n\tn, err := A()\n\treturn n, err\n}",
		id:   "p.F", want: "step A<error/unknown> ; ok",
	},
	{
		name: "bare return after a checked call",
		src:  flowPrelude + "func F() (n int, err error) {\n\tn, err = A()\n\tif err != nil {\n\t\treturn\n\t}\n\treturn\n}",
		id:   "p.F", want: "step A<error/unknown> ; ok",
	},
	{
		name: "bare return of a set sentinel",
		src:  flowPrelude + "func F() (err error) {\n\terr = ErrX\n\treturn\n}",
		id:   "p.F", want: "fail ErrX", error: "error",
	},
	{
		name: "bare return of an untouched named error",
		src:  "func F() (err error) {\n\treturn\n}",
		id:   "p.F", want: "ok",
	},
	{
		name: "bare return after branches disagree",
		src:  flowPrelude + "func F(x bool) (err error) {\n\tif x {\n\t\terr = ErrX\n\t}\n\treturn\n}",
		id:   "p.F", want: "branch if err != nil {yes: fail error | no: ok}",
	},
	{
		name: "bare return of an unchecked call",
		src:  flowPrelude + "func F() (n int, err error) {\n\tn, err = A()\n\treturn\n}",
		id:   "p.F", want: "step A<error/unknown> ; ok",
	},
	{
		name: "a shadowed err in a block leaves the result alone",
		src:  flowPrelude + "func F(x bool) (err error) {\n\tif x {\n\t\t_, err := A()\n\t\t_ = err\n\t}\n\treturn\n}",
		id:   "p.F", want: "ok",
	},
	{
		name: "bare return inside a check of another variable",
		src:  flowPrelude + "func F() (err error) {\n\t_, e2 := A()\n\tif e2 != nil {\n\t\treturn\n\t}\n\treturn\n}",
		id:   "p.F", want: "recover A ; branch if e2 != nil {yes: ok | no: } ; ok",
	},
	{
		name: "a continue before the check leaves the error unchecked on that path",
		src:  flowPrelude + "func F(ids []int) error {\n\tfor _, id := range ids {\n\t\t_, err := A()\n\t\tif id == 2 {\n\t\t\tcontinue\n\t\t}\n\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n\t}\n\treturn nil\n}",
		id:   "p.F", want: "loop for range ids {branch if id == 2 {yes: continue | no: } ; branch if err != nil {yes: fail error | no: }} ; ok",
	},
	{
		name: "errors.Is on an error known to be nil is dead",
		src:  flowPrelude + "func F() (err error) {\n\tif errors.Is(err, ErrSkip) {\n\t\tpanic(1)\n\t}\n\t_, err = A()\n\tif err != nil {\n\t\treturn\n\t}\n\treturn\n}",
		id:   "p.F", want: "step A<error/unknown> ; ok",
	},
	{
		name: "error parameter may be nil",
		src:  "func F(err error) error {\n\treturn err\n}",
		id:   "p.F", want: "branch if err != nil {yes: fail error | no: ok}",
	},
	{
		name: "checked error parameter",
		src:  "func F(err error) error {\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}",
		id:   "p.F", want: "branch if err != nil {yes: fail error | no: } ; ok",
	},
}

func TestGotoForward(t *testing.T) {
	g, m := diagramOf(t, `import "os"
func F(x bool) {
	if x {
		goto end
	}
	panic("skipped")
end:
	os.Exit(1)
}`)
	want(t, g, "goto end", "os.Exit")
	if got := m.States["goto_end"].On["ok"].Target; got != "os_Exit" {
		t.Fatalf("machine goto lands on %q; states %v", got, m.States)
	}
}

func TestGotoBackward(t *testing.T) {
	g, m := diagramOf(t, `func F(n int) {
again:
	n--
	if n != 0 {
		goto again
	}
	panic("done")
}`)
	want(t, g, "goto again", "if n != 0")
	got := m.States["goto_again"].On["ok"].Target
	if m.States[got].Description != "if n != 0" {
		t.Fatalf("machine goto lands on %q", got)
	}
}

func TestGotoToMissingLabelEnds(t *testing.T) {
	// A label in an enclosing block is still found.
	g, _ := diagramOf(t, `func F(x bool) {
	for {
		if x {
			goto out
		}
	}
out:
	panic(1)
}`)
	want(t, g, "goto out", "panic")
	_ = errpath.Node{}
}

func TestGotoPastAReturn(t *testing.T) {
	g, m := diagramOf(t, `var ErrBad error
func F(n int) error {
	if n < 0 {
		goto fail
	}
	return nil
fail:
	return ErrBad
}`)
	want(t, g, "goto fail", "ErrBad")
	if got := m.States["goto_fail"].On["ok"].Target; got != "ErrBad" {
		t.Fatalf("machine goto lands on %q", got)
	}
}

func TestNothingAfterATerminalPanicIsDrawn(t *testing.T) {
	g, _ := diagramOf(t, "func Log() error { return nil }\nfunc F() {\n\tpanic(1)\n\tLog()\n}")
	if _, drawn := g["swallow Log"]; drawn {
		t.Fatalf("unreachable code drawn: %v", g)
	}
}

func TestExpressionsEvaluatedOutsideBodies(t *testing.T) {
	prelude := "func MustKind() int { return 0 }\nfunc MustChan() chan int { return nil }\nfunc MustGet() any { return nil }\nfunc MustConn() *Conn { return nil }\nfunc MustClean() {}\ntype Conn struct{}\nfunc (*Conn) Close() {}\n"
	cases := []struct{ name, body, want string }{
		{"case expression", "switch 1 {\n\tcase MustKind():\n\t\tpanic(1)\n\t}", "panic MustKind ; branch switch 1 {MustKind(): panic panic! | default: } ; ok"},
		{"select operand", "select {\n\tcase <-MustChan():\n\t}", "panic MustChan ; ok"},
		{"type switch guard", "switch MustGet().(type) {\n\tcase int:\n\t\tpanic(1)\n\t}", "panic MustGet ; branch switch MustGet().(type) {int: panic panic! | default: } ; ok"},
		{"defer receiver", "defer MustConn().Close()", "panic MustConn ; ok"},
		{"deferred Must call", "defer MustClean()", "panic MustClean ; ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := analyse(t, map[string]string{"p.go": "package p\n\n" + prelude + "func F() {\n\t" + tc.body + "\n}\n"})
			if got := dump(byID(t, r, "p.F").Nodes); got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}
