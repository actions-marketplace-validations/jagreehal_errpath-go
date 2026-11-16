package errpath

import (
	"regexp"
	"strings"
)

// Jump targets for the renderers: break leaves the innermost for, switch or
// select (or the labelled one), continue re-enters the innermost loop, and
// fallthrough enters the next case.

type frameKind int

const (
	frameLoop frameKind = iota
	frameSwitch
	frameCase
)

type frame struct {
	kind  frameKind
	label string // Go label on the statement, if any
	brk   string // where break lands
	cont  string // where continue lands (loops)
	fall  string // where fallthrough lands (cases)
}

type frames []frame

var stmtLabelRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*): `)

// StmtLabel is the Go label a loop or branch label starts with, as in "outer: for".
func StmtLabel(label string) string { return stmtLabel(label) }

// BreaksOut says whether a branch label is a switch or select, which break leaves.
func BreaksOut(label string) bool { return breaksOut(label) }

// stmtLabel is the Go label a loop or branch label starts with, as in "outer: for".
func stmtLabel(label string) string {
	if m := stmtLabelRe.FindStringSubmatch(label); m != nil {
		return m[1]
	}
	return ""
}

// breaksOut says whether a branch is a break target: a switch or select.
func breaksOut(label string) bool {
	rest := strings.TrimPrefix(label, stmtLabel(label)+": ")
	return strings.HasPrefix(rest, "switch") || strings.HasPrefix(rest, "select")
}

// target is where a jump node goes, or false when it can't be resolved (goto).
func (fs frames) target(jump string) (string, bool) {
	word, label, _ := strings.Cut(jump, " ")
	for i := len(fs) - 1; i >= 0; i-- {
		f := fs[i]
		switch word {
		case "break":
			if f.kind != frameCase && (label == "" || f.label == label) {
				return f.brk, true
			}
		case "continue":
			if f.kind == frameLoop && (label == "" || f.label == label) {
				return f.cont, true
			}
		case "fallthrough":
			if f.kind == frameCase {
				return f.fall, true
			}
		default:
			return "", false
		}
	}
	return "", false
}

// emitArms emits a branch's arms last first, so each case knows where its
// fallthrough lands, and gives switch and select arms a break target.
func emitArms(fs *frames, n Node, rest string, emit func([]Node, string) string) []string {
	targets := make([]string, len(n.Arms))
	if breaksOut(n.Label) {
		*fs = append(*fs, frame{kind: frameSwitch, label: stmtLabel(n.Label), brk: rest})
		defer func() { *fs = (*fs)[:len(*fs)-1] }()
	}
	next := rest
	for i := len(n.Arms) - 1; i >= 0; i-- {
		*fs = append(*fs, frame{kind: frameCase, fall: next})
		targets[i] = emit(n.Arms[i].Body, rest)
		*fs = (*fs)[:len(*fs)-1]
		next = targets[i]
	}
	return targets
}

// emitLoop emits a loop: its condition's head, which runs before every check
// of self, then the body, with break bound to after the loop and continue to
// the post statement, which goes back to the head. It returns where the loop
// is entered and where its body starts.
func emitLoop(fs *frames, n Node, self, rest string, emit func([]Node, string) string) (entry, body string) {
	entry = emit(n.Head, self)
	post := emit(n.Post, entry)
	*fs = append(*fs, frame{kind: frameLoop, label: stmtLabel(n.Label), brk: rest, cont: post})
	defer func() { *fs = (*fs)[:len(*fs)-1] }()
	return entry, emit(n.Body, post)
}

// Diverges says whether a loop never ends: it can't be skipped and nothing
// breaks out of it (a return or panic leaves the function, not the loop).
func Diverges(n Node) bool {
	return n.Kind == "loop" && !n.CanSkip && !breaksOutOf(n)
}

func breaksOutOf(loop Node) bool {
	label := stmtLabel(loop.Label)
	return anyBreaks(loop.Body, label, false) || anyBreaks(loop.Post, label, false)
}

// anyBreaks: does a node leave the loop named label? inner is set inside a
// nested loop, switch or select, which a plain break leaves instead.
func anyBreaks(nodes []Node, label string, inner bool) bool {
	for _, n := range nodes {
		var hit bool
		switch n.Kind {
		case "jump":
			hit = breaksTo(n.Name, label, inner)
		case "loop":
			hit = anyBreaks(n.Body, label, true) || anyBreaks(n.Post, label, true)
		case "step":
			hit = anyBreaks(n.Cleanup, label, inner)
		case "branch":
			for _, a := range n.Arms {
				hit = hit || anyBreaks(a.Body, label, inner || breaksOut(n.Label))
			}
		}
		if hit {
			return true
		}
	}
	return false
}

func breaksTo(jump, label string, inner bool) bool {
	word, l, _ := strings.Cut(jump, " ")
	if word == "goto" {
		return true // it may leave
	}
	return word == "break" && ((l == "" && !inner) || (l != "" && l == label))
}

// stopsHere: nothing after this node in its block runs, except from a label.
func stopsHere(n Node) bool {
	return n.Kind == "succeed" || n.Kind == "fail" || n.Kind == "jump" || (n.Kind == "panic" && n.Terminal) || Diverges(n)
}

// gotos resolves goto jumps once every label has been seen, since a label can
// come after the jump that targets it.
type gotos struct {
	labels  map[string]string // label -> what runs from it
	pending []pendingGoto
}

type pendingGoto struct{ from, label string }

func (g *gotos) mark(label, target string) {
	if g.labels == nil {
		g.labels = map[string]string{}
	}
	g.labels[label] = target
}

// labelIndex is the first label in nodes: code after a jump is unreachable up
// to there, but a goto may still land on it.
func labelIndex(nodes []Node) int {
	for i, n := range nodes {
		if n.Kind == "label" {
			return i
		}
	}
	return -1
}

// resolve calls link for each goto with where it lands, or done when its
// label was never seen.
func (g *gotos) resolve(done string, link func(from, to string)) {
	for _, p := range g.pending {
		to, ok := g.labels[p.label]
		if !ok {
			to = done
		}
		link(p.from, to)
	}
}
