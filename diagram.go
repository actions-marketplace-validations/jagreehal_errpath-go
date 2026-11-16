package errpath

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// RenderFunction returns a Mermaid flowchart LR for fn.
func RenderFunction(fn Function) string {
	return Render(fn.Name, fn.Nodes)
}

// Render returns a Mermaid flowchart LR for nodes.
func Render(fnName string, nodes []Node) string {
	r := &mermaid{
		lines: []string{
			"flowchart LR",
			fmt.Sprintf(`  Start(["%s"])`, text(fnName)),
			"  Done((Success))",
		},
		errors: map[string]string{},
	}
	r.link("Start", "", r.emit(nodes, "Done"))
	r.gotos.resolve("Done", func(from, to string) { r.link(from, "", to) })
	if len(r.errors) > 0 {
		r.lines = append(r.lines, "  classDef error fill:#fdecea,stroke:#d93025,color:#a50e0e")
		ids := make([]string, 0, len(r.errors))
		for _, id := range r.errors {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		r.lines = append(r.lines, "  class "+strings.Join(ids, ",")+" error")
	}
	return strings.Join(r.lines, "\n")
}

type mermaid struct {
	lines  []string
	n      int
	errors map[string]string
	frames frames
	gotos  gotos
}

func (r *mermaid) add(prefix, shape string) string {
	r.n++
	id := fmt.Sprintf("%s%d", prefix, r.n)
	r.lines = append(r.lines, "  "+id+shape)
	return id
}

func (r *mermaid) link(src, label, dst string) {
	if label == "" {
		r.lines = append(r.lines, fmt.Sprintf("  %s --> %s", src, dst))
		return
	}
	clean := strings.ReplaceAll(strings.ReplaceAll(label, "_", ""), " ", "")
	alnum := true
	for _, c := range clean {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) {
			alnum = false
			break
		}
	}
	if alnum {
		r.lines = append(r.lines, fmt.Sprintf("  %s -->|%s| %s", src, label, dst))
	} else {
		r.lines = append(r.lines, fmt.Sprintf(`  %s -->|"%s"| %s`, src, text(label), dst))
	}
}

func (r *mermaid) errorBox(label string) string {
	if id, ok := r.errors[label]; ok {
		return id
	}
	id := r.add("E", fmt.Sprintf(`[/"%s"/]`, text(label)))
	r.errors[label] = id
	return id
}

func (r *mermaid) emit(nodes []Node, join string) string { //nolint:gocyclo // one case per node kind; splitting would only scatter them
	if len(nodes) == 0 {
		return join
	}
	node, after := nodes[0], nodes[1:]
	// After an exit nothing runs until a label, which a goto may still reach.
	if stopsHere(node) {
		if i := labelIndex(after); i >= 0 {
			r.emit(after[i:], join)
		}
	}
	switch node.Kind {
	case "succeed":
		return "Done"
	case "fail":
		err := node.FailError
		if err == "" {
			err = "Error"
		}
		return r.errorBox(err)
	case "label":
		target := r.emit(after, join)
		r.gotos.mark(node.Name, target)
		return target
	case "jump":
		// Nothing after a jump in the same block runs, up to a label.
		jid := r.add("J", fmt.Sprintf(`["%s"]`, text(node.Name)))
		if target, ok := r.frames.target(node.Name); ok {
			r.link(jid, "", target)
		} else if label, isGoto := strings.CutPrefix(node.Name, "goto "); isGoto {
			r.gotos.pending = append(r.gotos.pending, pendingGoto{jid, label})
		}
		return jid
	}

	var rest string
	if !stopsHere(node) {
		rest = r.emit(after, join)
	}

	switch node.Kind {
	case "step":
		sid := r.add("S", fmt.Sprintf(`["%s"]`, text(node.Name)))
		r.link(sid, "ok", rest)
		r.link(sid, "err", r.emit(errorPath(node), "Done"))
		return sid
	case "panic":
		pid := r.add("P", fmt.Sprintf(`["%s"]`, text(node.Name)))
		if !node.Terminal {
			r.link(pid, "ok", rest)
		}
		edge, box := "panic", "Panic"
		if node.How == "exit" {
			edge, box = "exit", "Exit"
		}
		r.link(pid, edge, r.errorBox(box))
		return pid
	case "swallow":
		wid := r.add("W", fmt.Sprintf(`["swallow %s"]`, text(node.Name)))
		r.link(wid, "", rest)
		return wid
	case "recover":
		rid := r.add("R", fmt.Sprintf(`["recover %s"]`, text(node.How)))
		r.link(rid, "", rest)
		return rid
	case "branch":
		did := r.add("D", fmt.Sprintf(`{"%s"}`, text(node.Label)))
		targets := emitArms(&r.frames, node, rest, r.emit)
		for i, arm := range node.Arms {
			r.link(did, arm.Label, targets[i])
		}
		return did
	case "loop":
		lid := r.add("L", fmt.Sprintf(`["%s"]`, text(node.Label)))
		entry, body := emitLoop(&r.frames, node, lid, rest, r.emit)
		r.link(lid, "next", body)
		if !Diverges(node) {
			r.link(lid, "done", rest)
		}
		return entry
	default:
		return rest
	}
}

// StepErrorLabel is the error a step's failure leaves the function as.
func StepErrorLabel(n Node) string {
	for i := len(n.Conversions) - 1; i >= 0; i-- {
		if n.Conversions[i] != "wrap" {
			return n.Conversions[i]
		}
	}
	return or(ErrorString(n.Error), "Error")
}

// errorPath is what runs when a step fails: its cleanup, then the exit,
// unless the cleanup itself ends the path.
func errorPath(n Node) []Node {
	path := append([]Node{}, n.Cleanup...)
	if !endsWithExit(path) {
		path = append(path, Fail(StepErrorLabel(n), 0))
	}
	return path
}

// text makes a label safe inside a quoted Mermaid string. Labels come from the
// code under review, so a backtick mustn't be able to close the fence around
// a diagram in a PR comment, nor <...> become HTML.
func text(label string) string {
	label = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, label)
	return labelEscaper.Replace(label)
}

var labelEscaper = strings.NewReplacer(`"`, "#quot;", "`", "#96;", "<", "#lt;", ">", "#gt;")
