package errpath_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jagreehal/errpath-go"
	"github.com/jagreehal/errpath-go/review"
)

func checkoutDir(t *testing.T) string {
	t.Helper()
	return filepath.Join("examples", "checkout")
}

func TestAnalyseCheckoutPlaceOrder(t *testing.T) {
	report, err := errpath.AnalysePath(checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	place := findFn(t, report, "PlaceOrder")
	want := "step Price<PriceError/callee> ; " +
		"branch if cart.HasCoupon {yes: step ValidateCoupon<CouponError/callee> | no: } ; " +
		"step Charge<CardDeclined/callee> ; step SaveOrder<DbError/callee> ; " +
		"step SendConfirmation<EmailError/callee> ; ok"
	if got := dump(place.Nodes); got != want {
		t.Fatalf("PlaceOrder\n got: %s\nwant: %s", got, want)
	}
	if price := findFn(t, report, "Price"); price.Returns.Error != "PriceError" {
		t.Fatalf("Price returns.error = %q", price.Returns.Error)
	}
}

func TestMachineJSONCheckout(t *testing.T) {
	report, err := errpath.AnalysePath(checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	place := findFn(t, report, "PlaceOrder")
	machine := errpath.FunctionToMachine(place)

	if machine.ID != "PlaceOrder" {
		t.Fatalf("id = %q", machine.ID)
	}
	if machine.States == nil {
		t.Fatal("expected states")
	}
	if _, ok := machine.States[machine.Initial]; !ok {
		t.Fatalf("initial %q not in states", machine.Initial)
	}

	finals := map[string]errpath.MachineState{}
	for name, st := range machine.States {
		if st.Type == "final" {
			finals[name] = st
		}
	}
	if _, ok := finals["Success"]; !ok {
		t.Fatalf("missing Success final; finals=%v", keysOf(finals))
	}
	errorFinals := 0
	for _, st := range finals {
		for _, tag := range st.Tags {
			if tag == "error" {
				errorFinals++
			}
		}
	}
	if errorFinals == 0 {
		t.Fatal("expected tagged error finals for autovisualizer")
	}

	data, err := json.Marshal(machine)
	if err != nil {
		t.Fatal(err)
	}
	var round errpath.Machine
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatal(err)
	}
}

func TestDiagramContainsSteps(t *testing.T) {
	report, err := errpath.AnalysePath(checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	place := findFn(t, report, "PlaceOrder")
	mermaid := errpath.RenderFunction(place)
	if mermaid[:12] != "flowchart LR" {
		t.Fatalf("mermaid starts with %q", mermaid[:min(20, len(mermaid))])
	}
	for _, want := range []string{"Price", "Done", "Charge"} {
		if !contains(mermaid, want) {
			t.Fatalf("mermaid missing %q:\n%s", want, mermaid)
		}
	}
}

func TestPanicAndSwallowFindings(t *testing.T) {
	dir := t.TempDir()
	src := `package bad

func Inner() error { return nil }

func Bad() error {
	x := Inner()
	_ = x
	_ = Inner()
	panic("nope")
	return nil
}
`
	path := filepath.Join(dir, "bad.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := errpath.AnalysePath(path)
	if err != nil {
		t.Fatal(err)
	}
	rules := map[string]int{}
	for _, f := range report.Findings {
		rules[f.Rule]++
	}
	if rules[errpath.NoSwallow] < 1 {
		t.Fatalf("expected no-swallow, findings=%v", report.Findings)
	}
	if rules[errpath.NoPanic] < 1 {
		t.Fatalf("expected no-panic, findings=%v", report.Findings)
	}
}

func TestEarlyReturnErr(t *testing.T) {
	dir := t.TempDir()
	src := `package check

type ValueError struct{}

func (e *ValueError) Error() string { return "neg" }

func Check(n int) (int, error) {
	if n < 0 {
		return 0, &ValueError{}
	}
	return n, nil
}
`
	path := filepath.Join(dir, "check.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := errpath.AnalysePath(path)
	if err != nil {
		t.Fatal(err)
	}
	fn := findFn(t, report, "Check")
	if len(fn.Nodes) < 2 {
		t.Fatalf("nodes=%+v", fn.Nodes)
	}
	if fn.Nodes[0].Kind != "branch" {
		t.Fatalf("first=%+v", fn.Nodes[0])
	}
	arm := fn.Nodes[0].Arms[0]
	if len(arm.Body) == 0 || arm.Body[0].Kind != "fail" || arm.Body[0].FailError != "ValueError" {
		t.Fatalf("yes arm=%+v", arm.Body)
	}
}

func TestIRSchemaKeys(t *testing.T) {
	report, err := errpath.AnalysePath(checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if int(raw["schema_version"].(float64)) != errpath.SchemaVersion {
		t.Fatalf("schema_version=%v", raw["schema_version"])
	}
	for _, key := range []string{"coverage", "functions", "findings", "parse_failures"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("missing key %s", key)
		}
	}
	place := findFn(t, report, "PlaceOrder")
	stepData, err := json.Marshal(place.Nodes[0])
	if err != nil {
		t.Fatal(err)
	}
	var step map[string]any
	if err := json.Unmarshal(stepData, &step); err != nil {
		t.Fatal(err)
	}
	if step["kind"] != "step" {
		t.Fatalf("step=%v", step)
	}
}

func TestCompareReview(t *testing.T) {
	baseDir := t.TempDir()
	headDir := t.TempDir()
	baseSrc := `package p

func F() error {
	return nil
}
`
	headSrc := `package p

func Inner() error { return nil }

func F() error {
	_ = Inner()
	return nil
}
`
	if err := os.WriteFile(filepath.Join(baseDir, "f.go"), []byte(baseSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(headDir, "f.go"), []byte(headSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	rev, err := review.ComparePaths(baseDir, headDir, "collapsed")
	if err != nil {
		t.Fatal(err)
	}
	if !rev.Changed {
		t.Fatal("expected changed")
	}
	// A dropped error is a warning, so moderate; only a certain panic is high.
	if rev.Risk != review.RiskModerate {
		t.Fatalf("risk=%s", rev.Risk)
	}
	if len(rev.NewFindings) != 1 || rev.NewFindings[0].Severity != "warning" {
		t.Fatalf("new findings=%+v", rev.NewFindings)
	}
	if rev.Markdown == "" || rev.Markdown[:len(review.Marker)] != review.Marker {
		t.Fatalf("markdown=%q", rev.Markdown)
	}
}

func findFn(t *testing.T, report errpath.Report, name string) errpath.Function {
	t.Helper()
	for _, f := range report.Functions {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("function %s not found", name)
	return errpath.Function{}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
