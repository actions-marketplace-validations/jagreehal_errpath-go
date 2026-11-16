package errpath

import (
	"encoding/json"
)

// SchemaVersion bumps when IR meaning changes.
const SchemaVersion = 1

// Finding rule ids.
const (
	NoPanic   = "no-panic"
	NoSwallow = "no-swallow"
	// BadAllow flags an allow comment that was ignored: unknown rule or no reason.
	BadAllow = "bad-allow"
	// NoTrace flags an error handled where it happens without being recorded
	// anywhere OpenTelemetry can see it.
	NoTrace = "no-trace"
)

// Confidence of a finding or inferred edge.
type Confidence string

const (
	ConfidenceCertain  Confidence = "certain"
	ConfidencePossible Confidence = "possible"
)

// ErrorSource says where a step's error type came from.
type ErrorSource string

const (
	ErrorSourceCallee    ErrorSource = "callee"
	ErrorSourceEnclosing ErrorSource = "enclosing"
	ErrorSourceUnknown   ErrorSource = "unknown"
)

// Arm is one branch of a Branch node.
type Arm struct {
	Label string `json:"label"`
	Body  []Node `json:"body"`
}

// Node is a tagged IR node in a function railway.
//
// Kinds: step, fail, succeed, panic, swallow, recover, jump, label, branch, loop.
type Node struct {
	Kind string `json:"kind"`

	// step / panic / swallow / recover / jump
	Name string `json:"name,omitempty"`

	// step: the callee's error, where it came from, and how the function
	// changed it before returning ("wrap" for fmt.Errorf with %w).
	Error       *string     `json:"error,omitempty"`
	Source      ErrorSource `json:"source,omitempty"`
	Conversions []string    `json:"conversions,omitempty"`
	// step: what runs on the error path before it returns.
	Cleanup []Node `json:"cleanup,omitempty"`
	Line    int    `json:"line,omitempty"`

	// branch / loop
	Label string `json:"label,omitempty"`
	Arms  []Arm  `json:"arms,omitempty"`
	Body  []Node `json:"body,omitempty"`

	// loop: can_skip when the body may not run at all; head is what the
	// condition runs before each check; post is a for statement's post
	// statement, which continue runs too.
	CanSkip bool   `json:"can_skip,omitempty"`
	Head    []Node `json:"head,omitempty"`
	Post    []Node `json:"post,omitempty"`

	// fail (serialised as "error")
	FailError string `json:"-"`

	// panic: terminal means the path always stops here.
	Terminal bool    `json:"terminal,omitempty"`
	Allowed  *string `json:"allowed,omitempty"`

	// recover: the handler doesn't record the error on every path (no-trace).
	Untraced bool `json:"untraced,omitempty"`

	// swallow / recover / panic
	How        string     `json:"how,omitempty"`
	Confidence Confidence `json:"confidence,omitempty"`
}

// MarshalJSON emits fail.error as "error" and keeps succeed minimal.
func (n Node) MarshalJSON() ([]byte, error) {
	type alias Node
	switch n.Kind {
	case "fail":
		return json.Marshal(struct {
			Kind  string `json:"kind"`
			Error string `json:"error"`
			Line  int    `json:"line,omitempty"`
		}{Kind: "fail", Error: n.FailError, Line: n.Line})
	case "succeed":
		return json.Marshal(struct {
			Kind string `json:"kind"`
		}{Kind: "succeed"})
	}
	return json.Marshal(alias(n))
}

// UnmarshalJSON restores Node including fail.error.
func (n *Node) UnmarshalJSON(data []byte) error {
	type alias Node
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*n = Node(a)
	if n.Kind == "fail" {
		var f struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(data, &f); err != nil {
			return err
		}
		n.FailError = f.Error
		n.Error = nil
	}
	return nil
}

// Step constructs a step node.
func Step(name string, errorType *string, source ErrorSource, line int) Node {
	return Node{Kind: "step", Name: name, Error: errorType, Source: source, Line: line}
}

// BranchNode constructs a branch.
func BranchNode(label string, arms []Arm) Node {
	return Node{Kind: "branch", Label: label, Arms: arms}
}

// LoopNode constructs a loop.
func LoopNode(label string, canSkip bool, body []Node) Node {
	return Node{Kind: "loop", Label: label, CanSkip: canSkip, Body: body}
}

// Fail constructs a fail exit.
func Fail(errorType string, line int) Node {
	return Node{Kind: "fail", FailError: errorType, Line: line}
}

// Succeed constructs a success exit.
func Succeed() Node {
	return Node{Kind: "succeed"}
}

// PanicNode constructs a panic node. how is "panic" or "exit".
func PanicNode(name, how string, terminal bool, confidence Confidence, line int, allowed *string) Node {
	return Node{Kind: "panic", Name: name, How: how, Terminal: terminal, Confidence: confidence, Line: line, Allowed: allowed}
}

// SwallowNode constructs a swallow node.
func SwallowNode(name, how string, confidence Confidence, line int, allowed *string) Node {
	return Node{Kind: "swallow", Name: name, How: how, Confidence: confidence, Line: line, Allowed: allowed}
}

// RecoverNode constructs a recover node.
func RecoverNode(name, how string, line int) Node {
	return Node{Kind: "recover", Name: name, How: how, Line: line}
}

// LabelNode marks where a goto to name lands.
func LabelNode(name string, line int) Node {
	return Node{Kind: "label", Name: name, Line: line}
}

// Jump constructs a jump node.
func Jump(name string, line int) Node {
	return Node{Kind: "jump", Name: name, Line: line}
}

// Returns describes what a function returns.
type Returns struct {
	Kind  string `json:"kind"` // result | other
	Error string `json:"error,omitempty"`
}

// Function is one analysed function, method or function literal.
type Function struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	File    string  `json:"file"`
	Line    int     `json:"line"`
	Returns Returns `json:"returns"`
	Nodes   []Node  `json:"nodes"`
	// Locals are the names the function declares, in order, so a review can
	// tell a renamed variable from a changed condition.
	Locals []string `json:"locals,omitempty"`
}

// Finding is a no-panic / no-swallow / no-trace / bad-allow hit.
type Finding struct {
	Rule       string     `json:"rule"`
	File       string     `json:"file"`
	Line       int        `json:"line"`
	Message    string     `json:"message"`
	Confidence Confidence `json:"confidence"`
	// Function is nil for findings about comments rather than code (bad-allow).
	Function *string `json:"function,omitempty"`
	Allowed  *string `json:"allowed,omitempty"`
}

// Coverage counts for a report.
type Coverage struct {
	Functions          int `json:"functions"`
	Steps              int `json:"steps"`
	ErrorFromCallee    int `json:"error_from_callee"`
	ErrorFromEnclosing int `json:"error_from_enclosing"`
	ErrorUnknown       int `json:"error_unknown"`
	Unanalysed         int `json:"unanalysed"`
}

// Unanalysed records code that was seen but not followed.
type Unanalysed struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Function string `json:"function"`
	Code     string `json:"code"`
	Reason   string `json:"reason"`
}

// ParseFailure records a file that would not parse.
type ParseFailure struct {
	File    string `json:"file"`
	Message string `json:"message"`
}

// Report is the top-level IR document.
type Report struct {
	SchemaVersion int            `json:"schema_version"`
	Coverage      Coverage       `json:"coverage"`
	Functions     []Function     `json:"functions"`
	Findings      []Finding      `json:"findings"`
	ParseFailures []ParseFailure `json:"parse_failures"`
	Unanalysed    []Unanalysed   `json:"unanalysed"`
	// SupportFilesSkipped counts _test.go files, which are not analysed.
	SupportFilesSkipped int `json:"support_files_skipped"`
}

// NewReport returns an empty versioned report.
func NewReport() Report {
	return Report{
		SchemaVersion: SchemaVersion,
		Functions:     []Function{},
		Findings:      []Finding{},
		ParseFailures: []ParseFailure{},
		Unanalysed:    []Unanalysed{},
	}
}

// ErrorString returns a display string for a step error pointer.
func ErrorString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// StrPtr returns a pointer to s.
func StrPtr(s string) *string { return &s }
