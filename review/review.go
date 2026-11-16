// Package review compares the error railways of two git refs: how a change
// moved error propagation, recovery and panic paths.
//
// Each exit has an identity (what it is) and a state (where it sits, how its
// error is converted, whether it's allowed). Comparing them separately keeps
// "same call, now inside an if" from cancelling out as no change.
package review

import (
	"archive/tar"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/jagreehal/errpath-go"
)

// Marker starts every review comment, so the Action can find and update it.
const Marker = "<!-- errpath-review -->"

const (
	maxFunctions = 20
	maxDiagrams  = 5
	maxSpan      = 200
	maxComment   = 60_000
)

// Risk level of a review.
type Risk string

const (
	RiskLow      Risk = "low"
	RiskModerate Risk = "moderate"
	RiskHigh     Risk = "high"
)

// Status of a function change.
type Status string

const (
	StatusChanged Status = "changed"
	StatusAdded   Status = "added"
)

// ItemKind classifies a flattened railway item.
type ItemKind string

const (
	ItemStep    ItemKind = "step"
	ItemFail    ItemKind = "fail"
	ItemPanic   ItemKind = "panic"
	ItemSwallow ItemKind = "swallow"
	ItemRecover ItemKind = "recover"
	ItemReturn  ItemKind = "return"
	ItemJump    ItemKind = "jump"
	ItemFork    ItemKind = "fork"
	// ItemDiverge is a loop nothing leaves: what follows it can't run.
	ItemDiverge ItemKind = "diverge"
)

// State is what can change about an exit while it stays the same exit.
type State struct {
	// Context is the branches and loops around it, outermost first.
	Context     string   `json:"context"`
	Conversions []string `json:"conversions"`
	Allowed     *string  `json:"allowed"`
	// Target is where a jump sends control, such as "next iteration 0" or
	// "goto L". This, not how the jump is spelled, makes it the same jump.
	Target *string `json:"target,omitempty"`
	// Untraced: a handler that doesn't record its error (no-trace).
	Untraced bool `json:"untraced,omitempty"`
}

// Item is one way out of the happy path, flattened from the IR.
type Item struct {
	Kind  ItemKind `json:"kind"`
	Label string   `json:"label"`
	// Error is the callee's error, when its own declaration named one.
	Error *string `json:"error"`
	Line  int     `json:"line"`
	State
	key     string
	finding *errpath.Finding
}

func (i Item) exact() string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%v\x00%s\x00%s\x00%v", i.key, i.Context, strings.Join(i.Conversions, "."),
		i.Allowed != nil, errpath.ErrorString(i.Allowed), errpath.ErrorString(i.Target), i.Untraced)
}

// ItemChange is the same exit in a different state.
type ItemChange struct {
	Item
	From State `json:"from"`
}

// MarshalJSON keeps the embedded item flat next to "from".
func (c ItemChange) MarshalJSON() ([]byte, error) {
	type flat struct {
		Item
		From State `json:"from"`
	}
	return json.Marshal(flat(c))
}

// ReturnsChange is a function's error result changing.
type ReturnsChange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// FnChange is a per-function railway diff.
type FnChange struct {
	ID        string         `json:"id"`
	Status    Status         `json:"status"`
	File      string         `json:"file"`
	Line      int            `json:"line"`
	MovedFrom *string        `json:"moved_from"`
	Returns   *ReturnsChange `json:"returns"`
	Added     []Item         `json:"added"`
	Removed   []Item         `json:"removed"`
	Changed   []ItemChange   `json:"changed"`
	// Reordered: the exits on both sides run in a different order.
	Reordered bool   `json:"reordered"`
	Diagram   string `json:"-"`
}

// NewFinding is a finding with the severity the Action annotates it with.
type NewFinding struct {
	Severity string
	errpath.Finding
}

// MarshalJSON flattens the finding next to severity.
func (n NewFinding) MarshalJSON() ([]byte, error) {
	type flat struct {
		Severity string `json:"severity"`
		errpath.Finding
	}
	return json.Marshal(flat(n))
}

// Review is the review JSON document. markdown, risk and newFindings are what
// the GitHub Action reads.
type Review struct {
	SchemaVersion int `json:"schema_version"`
	// Changed false means no change was seen and Markdown is empty. Read it with
	// Coverage: it is not proof that nothing changed.
	Changed     bool         `json:"changed"`
	Risk        Risk         `json:"risk"`
	Markdown    string       `json:"markdown"`
	NewFindings []NewFinding `json:"newFindings"`
	// NewAllows are findings this change introduced and silenced with an allow comment.
	NewAllows []errpath.Finding `json:"new_allows"`
	Functions []FnChange        `json:"functions"`
	// Moved counts functions that moved or were renamed with their railway intact.
	Moved int `json:"moved"`
	// NewFunctions and RemovedFunctions with nothing to flag stay silent.
	NewFunctions        int                  `json:"new_functions"`
	RemovedFunctions    int                  `json:"removed_functions"`
	Notes               []string             `json:"notes"`
	Coverage            errpath.Coverage     `json:"coverage"`
	UnparsedFiles       int                  `json:"unparsed_files"`
	SupportFilesSkipped int                  `json:"support_files_skipped"`
	Unanalysed          []errpath.Unanalysed `json:"unanalysed"`
	Summary             string               `json:"summary"`
}

// Options for Run.
type Options struct {
	Repo string
	Base string
	Head string
	// Paths limits the review to repo-relative paths; empty means everything.
	Paths    []string
	Diagrams string // open | collapsed
}

func nest(context, part string) string {
	if context == "" {
		return part
	}
	return context + " / " + part
}

// Flatten converts a function's railway to comparable items.
func Flatten(fn errpath.Function) []Item {
	var out []Item
	flatten(fn, fn.Nodes, "", nil, "end", &out)
	return out
}

// frame is a loop, switch or select a jump can land on, with where break
// and continue go from it.
type frame struct {
	label string
	loop  bool
	brk   string
	cont  string
}

// destination is where a jump sends control. Two jumps spelled differently
// that land in the same place, such as a break leaving a select that ends a
// loop body and a continue of that loop, are the same jump.
func destination(name string, frames []frame) *string {
	word, label, _ := strings.Cut(name, " ")
	if word == "goto" {
		return &name
	}
	for i := len(frames) - 1; i >= 0; i-- {
		f := frames[i]
		if label != "" && f.label != label {
			continue
		}
		switch {
		case word == "break":
			return &f.brk
		case word == "continue" && f.loop:
			return &f.cont
		}
	}
	return nil
}

// flatten appends items for nodes. after is where control goes when the
// nodes finish: what follows them, or the next loop iteration.
func flatten(fn errpath.Function, nodes []errpath.Node, context string, frames []frame, after string, out *[]Item) {
	for k, n := range nodes {
		// Where control goes after this node: what follows it, or after the
		// enclosing list when it's the last thing there.
		next := fmt.Sprintf("after %d.%d", len(frames), k)
		if lastBut(nodes[k+1:]) {
			next = after
		}
		if n.Kind == "loop" {
			flattenLoop(fn, n, context, frames, next, out)
			continue
		}
		it, ok := item(fn, n, context)
		if !ok {
			continue
		}
		if n.Kind == "jump" {
			it.Target = destination(n.Name, frames)
		}
		*out = append(*out, it)
		if n.Kind == "step" {
			flatten(fn, n.Cleanup, nest(context, "if "+n.Name+" fails"), frames, "", out)
		}
		armFrames := frames
		if n.Kind == "branch" && errpath.BreaksOut(n.Label) {
			armFrames = append(slices.Clone(frames), frame{label: errpath.StmtLabel(n.Label), brk: next})
		}
		for _, arm := range n.Arms {
			flatten(fn, arm.Body, nest(context, n.Label+": "+arm.Label), armFrames, next, out)
		}
	}
}

// lastBut: nothing but labels follows, so control leaves the list next.
func lastBut(rest []errpath.Node) bool {
	for _, n := range rest {
		if n.Kind != "label" {
			return false
		}
	}
	return true
}

func flattenLoop(fn errpath.Function, n errpath.Node, context string, frames []frame, next string, out *[]Item) {
	inner := nest(context, n.Label)
	if errpath.Diverges(n) {
		*out = append(*out, Item{Kind: ItemDiverge, Label: n.Label, Line: n.Line, State: State{Context: context}, key: "diverge"})
	}
	again := fmt.Sprintf("next iteration %d", len(frames))
	loopFrames := append(slices.Clone(frames), frame{label: errpath.StmtLabel(n.Label), loop: true, brk: next, cont: again})
	flatten(fn, n.Head, nest(inner, "condition"), frames, again, out)
	flatten(fn, n.Body, inner, loopFrames, again, out)
	flatten(fn, n.Post, inner, frames, again, out)
}

// item is one node as an item, without what it contains.
func item(fn errpath.Function, n errpath.Node, context string) (Item, bool) {
	it := Item{Line: n.Line, State: State{Context: context}}
	if f, ok := errpath.NodeFinding(fn, n); ok {
		it.finding = &f
	}
	switch n.Kind {
	case "step":
		it.Kind, it.Label = ItemStep, n.Name
		if n.Source == errpath.ErrorSourceCallee {
			it.Error = n.Error
		}
		it.Conversions = n.Conversions
		it.key = fmt.Sprintf("step %s %s", n.Name, errpath.ErrorString(it.Error))
	case "fail":
		it.Kind, it.Label, it.key = ItemFail, n.FailError, "fail "+n.FailError
	case "succeed":
		it.Kind, it.Label, it.Line, it.key = ItemReturn, "return", fn.Line, "return"
	case "panic":
		it.Kind, it.Label, it.Allowed, it.key = ItemPanic, n.Name, n.Allowed, "panic "+n.Name
	case "swallow":
		it.Kind, it.Label, it.Allowed, it.key = ItemSwallow, n.Name, n.Allowed, fmt.Sprintf("swallow %s %s", n.How, n.Name)
	case "recover":
		it.Kind, it.Label, it.Allowed, it.Untraced, it.key = ItemRecover, n.Name, n.Allowed, n.Untraced, fmt.Sprintf("recover %s %s", n.How, n.Name)
	case "jump":
		// What makes a jump the same jump is where it lands (State.Target).
		it.Kind, it.Label, it.key = ItemJump, n.Name, "jump"
	case "branch":
		it.Kind, it.Label, it.key = ItemFork, n.Label, "fork "+n.Label
	default:
		return Item{}, false
	}
	return it, true
}

// renames pairs each local only head declares with one only base declared,
// in order: most likely the same variable, renamed.
func renames(base, head []string) map[string]string {
	var gone, added []string
	for _, b := range base {
		if !slices.Contains(head, b) {
			gone = append(gone, b)
		}
	}
	for _, h := range head {
		if !slices.Contains(base, h) {
			added = append(added, h)
		}
	}
	m := map[string]string{}
	for i := 0; i < len(gone) && i < len(added); i++ {
		m[added[i]] = gone[i]
	}
	return m
}

// rename replaces whole-word uses of the names in s. A name after a dot is a
// field or method, not the local.
func rename(s string, m map[string]string) string {
	if len(m) == 0 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if !isWord(s[i]) {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && isWord(s[j]) {
			j++
		}
		word := s[i:j]
		if to, ok := m[word]; ok && (i == 0 || s[i-1] != '.') {
			word = to
		}
		b.WriteString(word)
		i = j
	}
	return b.String()
}

func isWord(c byte) bool {
	return c == '_' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || c >= 0x80
}

// inBaseNames rewrites head items in base's local names, for comparing.
func inBaseNames(items []Item, m map[string]string) {
	for i := range items {
		it := &items[i]
		it.key = rename(it.key, m)
		it.Label = rename(it.Label, m)
		it.Context = rename(it.Context, m)
		conv := make([]string, len(it.Conversions))
		for j, c := range it.Conversions {
			conv[j] = rename(c, m)
		}
		it.Conversions = conv
	}
}

// without returns items in a with no exact counterpart in b, counting repeats.
func without(a, b []Item) []Item {
	left := map[string]int{}
	for _, i := range b {
		left[i.exact()]++
	}
	var out []Item
	for _, i := range a {
		if left[i.exact()] > 0 {
			left[i.exact()]--
			continue
		}
		out = append(out, i)
	}
	return out
}

// diffItems splits the difference into added, removed, and the same exit in a new state.
func diffItems(base, head []Item) (plus, minus []Item, changed []ItemChange) {
	minus = without(base, head)
	for _, item := range without(head, base) {
		if i := slices.IndexFunc(minus, func(b Item) bool { return b.key == item.key }); i >= 0 {
			changed = append(changed, ItemChange{Item: item, From: minus[i].State})
			minus = slices.Delete(minus, i, i+1)
			continue
		}
		plus = append(plus, item)
	}
	// A fork only adds to what its arms' exits already say by appearing.
	isFork := func(i Item) bool { return i.Kind == ItemFork }
	return slices.DeleteFunc(plus, isFork), slices.DeleteFunc(minus, isFork), changed
}

// altersBehaviour: does this change what can happen, not just how it's annotated?
func altersBehaviour(c ItemChange) bool {
	typed := func(s State) []string {
		return slices.DeleteFunc(slices.Clone(s.Conversions), func(v string) bool { return v == "wrap" })
	}
	return c.From.Context != c.Context || !slices.Equal(typed(c.From), typed(c.State)) ||
		errpath.ErrorString(c.From.Target) != errpath.ErrorString(c.Target)
}

func describe(r errpath.Returns) string {
	if r.Kind == "result" {
		return "error " + r.Error
	}
	return "no error"
}

// fingerprint is whole-railway identity, for spotting a function that moved intact.
func fingerprint(f errpath.Function) string {
	items := Flatten(f)
	if len(items) == 0 {
		return ""
	}
	keys := make([]string, len(items))
	for i, it := range items {
		keys[i] = it.exact()
	}
	return describe(f.Returns) + "\n" + strings.Join(keys, "\n")
}

type fnPair struct{ base, head errpath.Function }

// matchUp moves functions with the same key out of added/removed into pairs.
// With unique, a key shared by several functions is too weak to trust.
func matchUp(pairs *[]fnPair, added, removed *[]errpath.Function, key func(errpath.Function) string, unique bool) {
	candidates := map[string][]int{}
	for i := len(*removed) - 1; i >= 0; i-- {
		if k := key((*removed)[i]); k != "" {
			candidates[k] = append(candidates[k], i)
		}
	}
	claims := map[string]int{}
	for _, h := range *added {
		if k := key(h); k != "" {
			claims[k]++
		}
	}
	taken := map[int]bool{}
	var keep []errpath.Function
	for _, h := range *added {
		k := key(h)
		list := candidates[k]
		if k == "" || len(list) == 0 || (unique && (len(list) != 1 || claims[k] != 1)) {
			keep = append(keep, h)
			continue
		}
		i := list[len(list)-1]
		candidates[k] = list[:len(list)-1]
		*pairs = append(*pairs, fnPair{(*removed)[i], h})
		taken[i] = true
	}
	*added = keep
	var rest []errpath.Function
	for i, f := range *removed {
		if !taken[i] {
			rest = append(rest, f)
		}
	}
	*removed = rest
}

// pair matches each head function with its base self: same id, then an
// identical railway under a new id, then a unique shared name.
func pair(base, head []errpath.Function) (pairs []fnPair, added, removed []errpath.Function) {
	byID := map[string]errpath.Function{}
	for _, f := range base {
		byID[f.ID] = f
	}
	headIDs := map[string]bool{}
	for _, h := range head {
		headIDs[h.ID] = true
		if b, ok := byID[h.ID]; ok {
			pairs = append(pairs, fnPair{b, h})
		} else {
			added = append(added, h)
		}
	}
	for _, f := range base {
		if !headIDs[f.ID] {
			removed = append(removed, f)
		}
	}
	matchUp(&pairs, &added, &removed, fingerprint, false)
	matchUp(&pairs, &added, &removed, func(f errpath.Function) string { return f.Name }, true)
	return pairs, added, removed
}

func severity(f errpath.Finding) string {
	if f.Rule == errpath.NoPanic && f.Confidence == errpath.ConfidenceCertain {
		return "error"
	}
	return "warning"
}

// newBadAllows are bad allow comments that weren't already there.
func newBadAllows(base, head errpath.Report) []errpath.Finding {
	left := map[string]int{}
	for _, f := range base.Findings {
		if f.Function == nil {
			left[f.File+"\x00"+f.Message]++
		}
	}
	var out []errpath.Finding
	for _, f := range head.Findings {
		if f.Function != nil {
			continue
		}
		if k := f.File + "\x00" + f.Message; left[k] > 0 {
			left[k]--
			continue
		}
		out = append(out, f)
	}
	return out
}

func keys(list []Item) []string {
	var out []string
	for _, i := range list {
		if i.Kind != ItemFork {
			out = append(out, i.key)
		}
	}
	return out
}

func exacts(list []Item) []string {
	out := make([]string, len(list))
	for i, it := range list {
		out[i] = it.exact()
	}
	return out
}

// Compare diffs two analysed refs. Pure, so everything but git is testable.
func Compare(base, head errpath.Report, diagrams string) Review {
	r := Review{
		SchemaVersion:       errpath.SchemaVersion,
		NewFindings:         []NewFinding{},
		NewAllows:           []errpath.Finding{},
		Functions:           []FnChange{},
		Notes:               []string{},
		Unanalysed:          []errpath.Unanalysed{},
		Coverage:            head.Coverage,
		UnparsedFiles:       len(head.ParseFailures),
		SupportFilesSkipped: head.SupportFilesSkipped,
	}
	pairs, added, removed := pair(base.Functions, head.Functions)
	for _, p := range pairs {
		ch, moved := diffPair(p)
		if ch != nil {
			r.Functions = append(r.Functions, *ch)
		} else if moved {
			r.Moved++
		}
	}
	for _, f := range removed {
		if len(Flatten(f)) > 0 {
			r.RemovedFunctions++
		}
	}
	for _, f := range added {
		// A new function is only worth showing for what it flags.
		plus := Flatten(f)
		if !slices.ContainsFunc(plus, func(i Item) bool { return i.finding != nil }) {
			r.NewFunctions++
			continue
		}
		r.Functions = append(r.Functions, FnChange{
			ID: f.ID, Status: StatusAdded, File: f.File, Line: f.Line,
			Added: plus, Removed: []Item{}, Changed: []ItemChange{}, Diagram: errpath.RenderFunction(f),
		})
	}
	slices.SortFunc(r.Functions, func(a, b FnChange) int {
		return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line), cmp.Compare(a.ID, b.ID))
	})
	r.collectFindings(base, head)
	r.Notes = append(r.Notes, parseNotes(base, head)...)
	shown := map[string]bool{}
	for _, f := range r.Functions {
		shown[f.ID] = true
	}
	for _, u := range head.Unanalysed {
		if shown[u.Function] {
			r.Unanalysed = append(r.Unanalysed, u)
		}
	}

	r.Risk = riskOf(r)
	r.Changed = len(r.Functions) > 0 || len(r.NewFindings) > 0 || len(r.Notes) > 0
	seen := "no failure-path change seen"
	if r.Changed {
		seen = "changes seen"
	}
	r.Summary = fmt.Sprintf("%s in %d functions; not analysed: %d unparsed files, %d test files, %d spots errpath could not follow",
		seen, r.Coverage.Functions, r.UnparsedFiles, r.SupportFilesSkipped, r.Coverage.Unanalysed)
	if r.Changed {
		r.Markdown = fittedMarkdown(r, diagrams != "open")
	}
	return r
}

// diffPair compares a function with its base self: a change, or nil with
// moved set when it only moved or was renamed.
func diffPair(p fnPair) (ch *FnChange, moved bool) {
	bi, hi := Flatten(p.base), Flatten(p.head)
	inBaseNames(hi, renames(p.base.Locals, p.head.Locals))
	plus, minus, changed := diffItems(bi, hi)
	unchanged := len(plus) == 0 && len(minus) == 0 && len(changed) == 0
	// Order of what's on both sides, whatever else happened to it.
	reordered := !slices.Equal(keys(without(bi, minus)), keys(without(hi, plus))) ||
		(unchanged && !slices.Equal(exacts(bi), exacts(hi)))
	var returns *ReturnsChange
	if p.base.Returns != p.head.Returns {
		returns = &ReturnsChange{From: describe(p.base.Returns), To: describe(p.head.Returns)}
	}
	if unchanged && !reordered && returns == nil {
		return nil, p.base.ID != p.head.ID
	}
	ch = &FnChange{
		ID: p.head.ID, Status: StatusChanged, File: p.head.File, Line: p.head.Line,
		Returns: returns, Added: orEmpty(plus), Removed: orEmpty(minus), Changed: changed,
		Reordered: reordered, Diagram: errpath.RenderFunction(p.head),
	}
	if ch.Changed == nil {
		ch.Changed = []ItemChange{}
	}
	if p.base.ID != p.head.ID {
		id := p.base.ID
		ch.MovedFrom = &id
	}
	return ch, false
}

// collectFindings splits the findings this change introduced into new ones
// and allowed ones, counting an allow comment that came or went.
func (r *Review) collectFindings(base, head errpath.Report) {
	add := func(f errpath.Finding) {
		if f.Allowed != nil {
			r.NewAllows = append(r.NewAllows, f)
		} else {
			r.NewFindings = append(r.NewFindings, NewFinding{Severity: severity(f), Finding: f})
		}
	}
	for _, f := range r.Functions {
		for _, i := range f.Added {
			if i.finding != nil {
				add(*i.finding)
			}
		}
		for _, c := range f.Changed {
			// An allow that came or went, or a handler that stopped recording.
			if c.finding != nil && ((c.From.Allowed == nil) != (c.Allowed == nil) || (c.Untraced && !c.From.Untraced)) {
				add(*c.finding)
			}
		}
	}
	for _, f := range newBadAllows(base, head) {
		r.NewFindings = append(r.NewFindings, NewFinding{Severity: "warning", Finding: f})
	}
}

// parseNotes: files that stopped parsing, whose functions weren't compared.
func parseNotes(base, head errpath.Report) []string {
	failed := map[string]bool{}
	for _, p := range base.ParseFailures {
		failed[p.File] = true
	}
	var notes []string
	for _, p := range head.ParseFailures {
		if !failed[p.File] {
			notes = append(notes, fmt.Sprintf("%s could not be read, so its functions were not compared: %s", span(p.File), span(p.Message)))
		}
	}
	return notes
}

func orEmpty(items []Item) []Item {
	if items == nil {
		return []Item{}
	}
	return items
}

func riskOf(r Review) Risk {
	for _, f := range r.NewFindings {
		if f.Severity == "error" {
			return RiskHigh
		}
	}
	if len(r.NewFindings) > 0 || len(r.Notes) > 0 {
		return RiskModerate
	}
	for _, f := range r.Functions {
		if f.Returns != nil || f.Reordered {
			return RiskModerate
		}
		// These coming or going decide what else runs, or whether an error
		// reaches the caller at all.
		for _, i := range slices.Concat(f.Added, f.Removed) {
			if i.Kind == ItemJump || i.Kind == ItemRecover || i.Kind == ItemDiverge {
				return RiskModerate
			}
		}
		if slices.ContainsFunc(f.Changed, altersBehaviour) {
			return RiskModerate
		}
	}
	return RiskLow
}

// span is an inline code span that holds any text. Everything here comes from
// the code under review, which a PR author controls: unescaped, a label or an
// allow reason could add mentions, links or images to the comment.
func span(s string) string {
	clean := []rune(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
	if len(clean) > maxSpan {
		clean = clean[:maxSpan]
	}
	run, longest := 0, 0
	for _, c := range clean {
		if c == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	text := string(clean)
	pad := ""
	if strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") {
		pad = " "
	}
	return fence + pad + text + pad + fence
}

// messageSpan is a finding message as one span: its own backticks are decoration.
func messageSpan(m string) string { return span(strings.ReplaceAll(m, "`", "")) }

func itemLine(i Item) string {
	var what string
	switch i.Kind {
	case ItemStep:
		what = "step " + span(i.Label)
	case ItemFail:
		what = "failure exit " + span(i.Label)
	case ItemPanic:
		what = "panic path " + span(i.Label)
	case ItemSwallow:
		what = "dropped error from " + span(i.Label)
	case ItemRecover:
		what = "handled error from " + span(i.Label)
	case ItemReturn:
		what = "early return"
	case ItemDiverge:
		what = "loop that never finishes " + span(i.Label)
	default:
		what = span(i.Label)
	}
	if i.Error != nil {
		what += " → " + span(*i.Error)
	}
	return what
}

func changeLines(c ItemChange) []string {
	at := func(s string) string { return or(s, "top level") }
	list := func(v []string) string {
		if len(v) == 0 {
			return "none"
		}
		return strings.Join(v, ".")
	}
	var out []string
	if c.From.Context != c.Context {
		out = append(out, fmt.Sprintf("moved from %s to %s", span(at(c.From.Context)), span(at(c.Context))))
	}
	if !slices.Equal(c.From.Conversions, c.Conversions) {
		out = append(out, fmt.Sprintf("error conversion %s → %s", span(list(c.From.Conversions)), span(list(c.Conversions))))
	}
	if errpath.ErrorString(c.From.Target) != errpath.ErrorString(c.Target) {
		out = append(out, "now lands somewhere else")
	}
	if c.From.Untraced != c.Untraced {
		if c.Untraced {
			out = append(out, "no longer records the error")
		} else {
			out = append(out, "now records the error")
		}
	}
	if (c.From.Allowed == nil) != (c.Allowed == nil) || errpath.ErrorString(c.From.Allowed) != errpath.ErrorString(c.Allowed) {
		if c.Allowed != nil {
			out = append(out, "allow comment: "+span(*c.Allowed))
		} else {
			out = append(out, "allow comment removed")
		}
	}
	what := itemLine(c.Item)
	for i, line := range out {
		out[i] = what + ": " + line
	}
	return out
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// fittedMarkdown keeps the comment under GitHub's size limit, which would
// otherwise lose the review altogether. Diagrams go first, then the tail.
func fittedMarkdown(r Review, collapse bool) string {
	if full := markdown(r, collapse, true); len(full) <= maxComment {
		return full
	}
	plain := markdown(r, collapse, false)
	if len(plain) <= maxComment {
		return plain
	}
	note := "\n…too long for a comment. The full list is in the JSON output.\n"
	cut := plain[:maxComment-len(note)]
	if i := strings.LastIndex(cut, "\n"); i > 0 {
		cut = cut[:i]
	}
	return cut + note
}

func markdown(r Review, collapse, diagrams bool) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("%s\n## Error path review: %s risk\n\n", Marker, r.Risk)
	w("%s changed, %s.\n\n", plural(len(r.Functions), "function"), plural(len(r.NewFindings), "new finding"))
	for _, n := range r.Notes {
		w("> %s\n\n", n)
	}
	writeFindings(w, r)
	if len(r.Functions) > 0 {
		w("### Changed error paths\n\n")
	}
	for i, f := range r.Functions {
		if i >= maxFunctions {
			w("…and %d more. The full list is in the JSON output.\n\n", len(r.Functions)-maxFunctions)
			break
		}
		writeFunction(w, f)
		if diagrams && i < maxDiagrams && f.Diagram != "" {
			writeDiagram(w, f.Diagram, collapse)
		}
	}
	if len(r.Unanalysed) > 0 {
		w("### Not followed\n\nerrpath couldn't follow these, so the paths above may be incomplete:\n\n")
		for _, u := range r.Unanalysed {
			w("- %s %s: %s\n", span(fmt.Sprintf("%s:%d", u.File, u.Line)), span(u.Code), u.Reason)
		}
		w("\n")
	}
	c := r.Coverage
	w("---\n<sub>Syntax-only analysis. In this ref, %d of %d steps name their error from the callee. %d `_test.go` files were skipped. Calls through interfaces or values of types outside this repository are only matched by method name.</sub>\n",
		c.ErrorFromCallee, c.Steps, r.SupportFilesSkipped)
	return b.String()
}

type writer func(format string, args ...any)

func writeFindings(w writer, r Review) {
	if len(r.NewFindings) > 0 {
		w("### New findings\n\n")
		for _, f := range r.NewFindings {
			possibly := ""
			if f.Confidence == errpath.ConfidencePossible {
				possibly = " (possible)"
			}
			w("- `%s` %s%s at %s\n", f.Rule, messageSpan(f.Message), possibly, span(fmt.Sprintf("%s:%d", f.File, f.Line)))
		}
		w("\n")
		if slices.ContainsFunc(r.NewFindings, func(f NewFinding) bool { return f.Rule == errpath.NoTrace }) {
			w("Record a handled error so it reaches your traces: `slog.ErrorContext(ctx, \"...\", \"error\", err)`, `span.RecordError(err)`, or `autotel.RecordError(ctx, err, nil)`.\n\n")
		}
	}
	if len(r.NewAllows) > 0 {
		w("### New allow comments\n\n")
		for _, f := range r.NewAllows {
			w("- `%s` %s at %s: %s\n", f.Rule, messageSpan(f.Message), span(fmt.Sprintf("%s:%d", f.File, f.Line)), span(errpath.ErrorString(f.Allowed)))
		}
		w("\n")
	}
}

func writeFunction(w writer, f FnChange) {
	status := ""
	if f.Status == StatusAdded {
		status = " (new)"
	}
	w("#### %s%s\n\n%s\n\n", span(f.ID), status, span(fmt.Sprintf("%s:%d", f.File, f.Line)))
	if f.MovedFrom != nil {
		w("- moved from %s\n", span(*f.MovedFrom))
	}
	if f.Returns != nil {
		w("- returns %s → %s\n", span(f.Returns.From), span(f.Returns.To))
	}
	for _, a := range f.Added {
		w("- added %s\n", itemLine(a))
	}
	for _, a := range f.Removed {
		w("- removed %s\n", itemLine(a))
	}
	for _, c := range f.Changed {
		for _, line := range changeLines(c) {
			w("- %s\n", line)
		}
	}
	if f.Reordered {
		w("- exits in a different order\n")
	}
	w("\n")
}

func writeDiagram(w writer, diagram string, collapse bool) {
	body := fmt.Sprintf("_Error paths: each step that can fail, and where its error goes._\n\n```mermaid\n%s\n```\n", diagram)
	if collapse {
		w("<details><summary>Error path diagram</summary>\n\n%s\n</details>\n\n", body)
	} else {
		w("%s\n", body)
	}
}

// Run compares two git refs, starting from their merge base.
func Run(opts Options) (Review, error) {
	if opts.Base == "" {
		return Review{}, errors.New("base ref is required")
	}
	opts.Repo = or(opts.Repo, ".")
	opts.Head = or(opts.Head, "HEAD")
	out, err := git(opts.Repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return Review{}, err
	}
	top := strings.TrimSpace(string(out))
	base, err := resolve(top, opts.Base)
	if err != nil {
		return Review{}, err
	}
	head, err := resolve(top, opts.Head)
	if err != nil {
		return Review{}, err
	}
	// From where the branch left, or a base that moved on would read as the
	// branch undoing everything merged since.
	start := base
	if mb, err := git(top, "merge-base", base, head); err == nil {
		start = strings.TrimSpace(string(mb))
	} else {
		fmt.Fprintf(os.Stderr, "no merge base with %s, so comparing against it directly (a shallow clone?)\n", opts.Base)
	}

	tmp, err := os.MkdirTemp("", "errpath-")
	if err != nil {
		return Review{}, err
	}
	defer func() { _ = os.RemoveAll(tmp) }() // best effort: it is a temp dir
	baseReport, err := reportAt(top, start, opts.Paths, filepath.Join(tmp, "base"))
	if err != nil {
		return Review{}, fmt.Errorf("base %s: %w", opts.Base, err)
	}
	headReport, err := reportAt(top, head, opts.Paths, filepath.Join(tmp, "head"))
	if err != nil {
		return Review{}, fmt.Errorf("head %s: %w", opts.Head, err)
	}
	return Compare(baseReport, headReport, opts.Diagrams), nil
}

// ComparePaths compares two directories without git.
func ComparePaths(basePath, headPath, diagrams string) (Review, error) {
	baseReport, err := errpath.AnalysePath(basePath)
	if err != nil {
		return Review{}, err
	}
	headReport, err := errpath.AnalysePath(headPath)
	if err != nil {
		return Review{}, err
	}
	return Compare(baseReport, headReport, diagrams), nil
}

func git(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("git: %w", err)
	}
	return out, nil
}

func resolve(top, rev string) (string, error) {
	out, err := git(top, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// reportAt writes a commit's Go sources to dest and analyses them.
func reportAt(top, commit string, paths []string, dest string) (errpath.Report, error) {
	if err := os.MkdirAll(dest, 0o750); err != nil {
		return errpath.Report{}, err
	}
	if err := gitExport(top, commit, paths, dest); err != nil {
		return errpath.Report{}, err
	}
	return errpath.AnalysePath(dest)
}

// gitExport streams `git archive` and keeps Go sources under paths, plus every
// go.mod so imports resolve. Symlinks and anything escaping dest are skipped.
func gitExport(top, commit string, paths []string, dest string) error {
	prefixes := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.Trim(filepath.ToSlash(filepath.Clean(p)), "/")
		if p == "." || p == "" {
			prefixes = nil
			break
		}
		prefixes = append(prefixes, p)
	}
	wanted := func(name string) bool {
		if filepath.Base(name) == "go.mod" {
			return true
		}
		if !strings.HasSuffix(name, ".go") {
			return false
		}
		return len(prefixes) == 0 || slices.ContainsFunc(prefixes, func(p string) bool {
			return name == p || strings.HasPrefix(name, p+"/")
		})
	}

	cmd := exec.Command("git", "-C", top, "archive", "--format=tar", commit)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	extractErr := extract(tar.NewReader(stdout), dest, wanted)
	if extractErr != nil {
		_, _ = io.Copy(io.Discard, stdout) // let git finish writing before Wait
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git archive: %s", or(strings.TrimSpace(stderr.String()), err.Error()))
	}
	return extractErr
}

func extract(tr *tar.Reader, dest string, wanted func(string) bool) error {
	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || !wanted(hdr.Name) {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(hdr.Name))
		if !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
}
