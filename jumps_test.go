package errpath_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/jagreehal/errpath-go"
)

var (
	mermaidNode = regexp.MustCompile(`^\s+([A-Z]+\d+)[\[{(/]+"?(.*?)"?[\]})/]+$`)
	mermaidEdge = regexp.MustCompile(`^\s+(\S+) -->(?:\|"?(.*?)"?\|)? (\S+)$`)
)

// graph parses a rendered diagram into node text and edges keyed by node text.
func graph(t *testing.T, d string) map[string][]string {
	t.Helper()
	names := map[string]string{"Done": "Done", "Start": "Start"}
	type edge struct{ src, label, dst string }
	var edges []edge
	for _, line := range strings.Split(d, "\n") {
		if m := mermaidEdge.FindStringSubmatch(line); m != nil {
			edges = append(edges, edge{m[1], m[2], m[3]})
		} else if m := mermaidNode.FindStringSubmatch(line); m != nil {
			names[m[1]] = m[2]
		}
	}
	out := map[string][]string{}
	for _, e := range edges {
		out[names[e.src]] = append(out[names[e.src]], strings.TrimSpace(e.label+" "+names[e.dst]))
	}
	return out
}

func diagramOf(t *testing.T, body string) (map[string][]string, errpath.Machine) {
	t.Helper()
	r := analyse(t, map[string]string{"p.go": "package p\n\n" + body + "\n"})
	fn := byID(t, r, "p.F")
	return graph(t, errpath.RenderFunction(fn)), errpath.FunctionToMachine(fn)
}

func want(t *testing.T, g map[string][]string, from string, to ...string) {
	t.Helper()
	if got := strings.Join(g[from], ", "); got != strings.Join(to, ", ") {
		t.Fatalf("%s goes to [%s], want [%s]\ngraph: %v", from, got, strings.Join(to, ", "), g)
	}
}

func TestBreakLeavesTheLoop(t *testing.T) {
	g, m := diagramOf(t, "func F() {\n\tfor {\n\t\tbreak\n\t}\n\tpanic(\"after\")\n}")
	want(t, g, "break", "panic")
	want(t, g, "for", "next break", "done panic")
	if got := m.States["break"].On["ok"].Target; got != "panic" {
		t.Fatalf("machine break goes to %q", got)
	}
}

func TestContinueAndLabelledBreak(t *testing.T) {
	g, _ := diagramOf(t, `func F(xs []int, c bool) {
outer:
	for range xs {
		for c {
			if c {
				continue
			}
			break outer
		}
	}
	panic("after")
}`)
	want(t, g, "continue", "for c")
	want(t, g, "break outer", "panic")
}

func TestBreakInSwitchLeavesOnlyTheSwitch(t *testing.T) {
	g, _ := diagramOf(t, `func F(n int) {
	for {
		switch n {
		case 1:
			break
		}
		panic("in loop")
	}
}`)
	want(t, g, "break", "panic")
}

func TestFallthroughEntersNextCase(t *testing.T) {
	g, _ := diagramOf(t, `func F(n int) {
	switch n {
	case 1:
		fallthrough
	case 2:
		panic("two")
	}
}`)
	want(t, g, "fallthrough", "panic")
}

func TestMachineKeepsArmsWhoseLabelsCollide(t *testing.T) {
	_, m := diagramOf(t, `func F(s string) {
	switch s {
	case "a.b":
		panic("first")
	case "a/b":
	}
}`)
	var sw errpath.MachineState
	for _, st := range m.States {
		if st.Description == `switch s` {
			sw = st
		}
	}
	if len(sw.On) != 3 {
		t.Fatalf("want 3 events (two cases and default), got %v", sw.On)
	}
	reachesPanic := false
	for _, tr := range sw.On {
		reachesPanic = reachesPanic || tr.Target == "panic"
	}
	if !reachesPanic {
		t.Fatalf("panic arm lost: %v", sw.On)
	}
}

func TestContinueRunsThePostStatement(t *testing.T) {
	g, m := diagramOf(t, `func Drop() error { return nil }
func F() {
	for i := 0; i < 2; Drop() {
		i++
		continue
	}
}`)
	want(t, g, "continue", "swallow Drop")
	want(t, g, "swallow Drop", "for i #lt; 2")
	want(t, g, "for i #lt; 2", "next continue", "done Done")
	if got := m.States["continue"].On["ok"].Target; got != "swallow_Drop" {
		t.Fatalf("machine continue goes to %q", got)
	}
}

func TestEmptyInfiniteLoopNeverFinishes(t *testing.T) {
	g, m := diagramOf(t, "func F() {\n\tfor {\n\t}\n\tpanic(\"unreachable\")\n}")
	want(t, g, "for", "next for")
	if _, drawn := g["panic"]; drawn {
		t.Fatalf("unreachable panic drawn: %v", g)
	}
	if m.Initial != "loop" {
		t.Fatalf("machine starts at %q, want the loop; states %v", m.Initial, m.States)
	}
	if _, ok := m.States["loop"].On["done"]; ok {
		t.Fatal("a loop that never ends has no done transition")
	}
	r := analyse(t, map[string]string{"p.go": "package p\n\nfunc F() {\n\tfor {\n\t}\n\tpanic(\"unreachable\")\n}\n"})
	if got := dump(byID(t, r, "p.F").Nodes); got != "loop for {} ; panic panic!" {
		t.Fatalf("IR %s", got)
	}
}

func TestLoopWithABreakStillFinishes(t *testing.T) {
	g, _ := diagramOf(t, "func F(x bool) {\n\tfor {\n\t\tif x {\n\t\t\tbreak\n\t\t}\n\t}\n\tpanic(\"after\")\n}")
	want(t, g, "for", "next if x", "done panic")
}

func TestLoopConditionIsEvaluatedEachIteration(t *testing.T) {
	src := "func MustContinue() bool { return false }\nfunc F() {\n\tfor MustContinue() {\n\t}\n}"
	r := analyse(t, map[string]string{"p.go": "package p\n\n" + src + "\n"})
	fn := byID(t, r, "p.F")
	if got := dump(fn.Nodes); got != "loop for MustContinue() {head: panic MustContinue | } ; ok" {
		t.Fatalf("IR %s", got)
	}
	if f := findingsIn(r, "p.F"); len(f) != 1 || !strings.Contains(f[0], "MustContinue") {
		t.Fatalf("findings %q", f)
	}
	g, m := diagramOf(t, src)
	want(t, g, "Start", "MustContinue")
	want(t, g, "MustContinue", "ok for MustContinue()", "panic Panic")
	want(t, g, "for MustContinue()", "next MustContinue", "done Done")
	if m.Initial != "MustContinue" {
		t.Fatalf("machine starts at %q", m.Initial)
	}
}

func TestContinueReevaluatesTheCondition(t *testing.T) {
	g, _ := diagramOf(t, "func MustMore() bool { return false }\nfunc Drop() error { return nil }\nfunc F() {\n\tfor i := 0; MustMore(); Drop() {\n\t\ti++\n\t\tcontinue\n\t}\n}")
	want(t, g, "continue", "swallow Drop")
	want(t, g, "swallow Drop", "MustMore")
}
