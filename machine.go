package errpath

import (
	"fmt"
	"strings"
	"unicode"
)

// Machine is data-only XState MachineJSON for autovisualizer.
type Machine struct {
	ID      string                  `json:"id"`
	Initial string                  `json:"initial"`
	States  map[string]MachineState `json:"states"`
}

// MachineState is one state in MachineJSON.
type MachineState struct {
	Type        string                   `json:"type,omitempty"`
	Tags        []string                 `json:"tags,omitempty"`
	Description string                   `json:"description,omitempty"`
	On          map[string]MachineTarget `json:"on,omitempty"`
}

// MachineTarget is a transition target.
type MachineTarget struct {
	Target string `json:"target"`
}

// FunctionToMachine builds MachineJSON suitable for autovisualizer definition.
func FunctionToMachine(fn Function) Machine {
	m := &machineBuilder{
		used:        map[string]struct{}{},
		states:      map[string]MachineState{},
		errorFinals: map[string]string{},
	}
	m.successID = m.safeID("Success")
	m.states[m.successID] = MachineState{Type: "final"}

	initial := m.emit(fn.Nodes, m.successID)
	m.gotos.resolve(m.successID, func(from, to string) { m.states[from].On["ok"] = MachineTarget{Target: to} })
	if _, ok := m.states[initial]; !ok {
		initial = m.successID
	}
	return Machine{ID: fn.Name, Initial: initial, States: m.states}
}

type machineBuilder struct {
	used        map[string]struct{}
	states      map[string]MachineState
	errorFinals map[string]string
	successID   string
	frames      frames
	gotos       gotos
	stepN       int
}

func (m *machineBuilder) safeID(name string) string {
	var b strings.Builder
	for _, c := range name {
		if unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_' || c == '-' {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	base := strings.Trim(b.String(), "_")
	if base == "" {
		base = "state"
	}
	if unicode.IsDigit(rune(base[0])) {
		base = "s_" + base
	}
	candidate := base
	n := 2
	for {
		if _, ok := m.used[candidate]; !ok {
			m.used[candidate] = struct{}{}
			return candidate
		}
		candidate = fmt.Sprintf("%s_%d", base, n)
		n++
	}
}

func (m *machineBuilder) eventName(label string) string {
	var b strings.Builder
	for _, c := range label {
		if unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_' || c == '-' {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	cleaned := strings.Trim(b.String(), "_")
	if cleaned == "" {
		return "arm"
	}
	if len(cleaned) > 40 {
		cleaned = cleaned[:40]
	}
	return cleaned
}

func (m *machineBuilder) errorState(label string) string {
	if id, ok := m.errorFinals[label]; ok {
		return id
	}
	id := m.safeID(label)
	m.errorFinals[label] = id
	m.states[id] = MachineState{Type: "final", Tags: []string{"error"}}
	return id
}

func (m *machineBuilder) emit(nodes []Node, join string) string { //nolint:gocyclo // one case per node kind; splitting would only scatter them
	var at func(int) string
	at = func(index int) string {
		if index >= len(nodes) {
			return join
		}
		node := nodes[index]
		// After an exit nothing runs until a label, which a goto may still reach.
		if stopsHere(node) {
			if i := labelIndex(nodes[index+1:]); i >= 0 {
				at(index + 1 + i)
			}
		}
		switch node.Kind {
		case "label":
			target := at(index + 1)
			m.gotos.mark(node.Name, target)
			return target
		case "jump":
			sid := m.safeID(node.Name)
			m.states[sid] = MachineState{On: map[string]MachineTarget{}}
			if target, ok := m.frames.target(node.Name); ok {
				m.states[sid].On["ok"] = MachineTarget{Target: target}
			} else if label, isGoto := strings.CutPrefix(node.Name, "goto "); isGoto {
				m.gotos.pending = append(m.gotos.pending, pendingGoto{sid, label})
			}
			return sid
		}
		var rest string
		if !stopsHere(node) {
			rest = at(index + 1)
		}

		switch node.Kind {
		case "step":
			m.stepN++
			name := node.Name
			if name == "" {
				name = fmt.Sprintf("step_%d", m.stepN)
			}
			sid := m.safeID(name)
			on := map[string]MachineTarget{
				"ok":  {Target: rest},
				"err": {Target: m.emit(errorPath(node), m.successID)},
			}
			m.states[sid] = MachineState{On: on}
			return sid

		case "succeed":
			return m.successID

		case "fail":
			err := node.FailError
			if err == "" {
				err = "Error"
			}
			return m.errorState(err)

		case "panic":
			panicSID := m.safeID(or(node.Name, "panic"))
			box := "Panic"
			if node.How == "exit" {
				box = "Exit"
			}
			on := map[string]MachineTarget{node.How: {Target: m.errorState(box)}}
			if !node.Terminal {
				on["ok"] = MachineTarget{Target: rest}
			}
			m.states[panicSID] = MachineState{Tags: []string{"error"}, On: on}
			return panicSID

		case "swallow":
			sid := m.safeID("swallow_" + node.Name)
			m.states[sid] = MachineState{
				On:          map[string]MachineTarget{"ok": {Target: rest}},
				Description: fmt.Sprintf("swallowed (%s)", node.How),
			}
			return sid

		case "recover":
			sid := m.safeID("recover_" + node.Name)
			m.states[sid] = MachineState{
				On:          map[string]MachineTarget{"ok": {Target: rest}},
				Description: node.How,
			}
			return sid

		case "branch":
			sid := m.safeID("branch")
			targets := emitArms(&m.frames, node, rest, m.emit)
			on := map[string]MachineTarget{}
			for i, arm := range node.Arms {
				event := arm.Label
				if event != "yes" && event != "no" {
					event = m.eventName(arm.Label)
				}
				// Sanitising can make two labels equal ("a.b", "a/b"); keep both arms.
				unique := event
				for n := 2; ; n++ {
					if _, taken := on[unique]; !taken {
						break
					}
					unique = fmt.Sprintf("%s_%d", event, n)
				}
				on[unique] = MachineTarget{Target: targets[i]}
			}
			m.states[sid] = MachineState{On: on, Description: node.Label}
			return sid

		case "loop":
			sid := m.safeID("loop")
			entry, body := emitLoop(&m.frames, node, sid, rest, m.emit)
			on := map[string]MachineTarget{"next": {Target: body}}
			if !Diverges(node) {
				on["done"] = MachineTarget{Target: rest}
			}
			m.states[sid] = MachineState{On: on, Description: node.Label}
			return entry

		default:
			return rest
		}
	}
	return at(0)
}
