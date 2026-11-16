package review_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jagreehal/errpath-go"
	"github.com/jagreehal/errpath-go/review"
)

func TestFlattenAndCompare(t *testing.T) {
	fn := func(nodes ...errpath.Node) errpath.Report {
		return errpath.Report{SchemaVersion: errpath.SchemaVersion, Functions: []errpath.Function{{
			ID: "p.F", Name: "F", File: "f.go", Line: 1, Nodes: nodes,
		}}}
	}
	base := fn(errpath.Succeed())
	head := fn(errpath.PanicNode("panic", "panic", true, errpath.ConfidenceCertain, 2, nil))
	rev := review.Compare(base, head, "collapsed")
	if !rev.Changed || rev.Risk != review.RiskHigh {
		t.Fatalf("changed=%v risk=%s", rev.Changed, rev.Risk)
	}
	data, err := json.Marshal(rev)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"risk", "markdown", "newFindings", "changed", "new_allows", "moved"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
	nf := raw["newFindings"].([]any)[0].(map[string]any)
	for _, key := range []string{"severity", "rule", "file", "line", "message"} {
		if _, ok := nf[key]; !ok {
			t.Fatalf("newFindings[0] missing %s: %v", key, nf)
		}
	}
}

func TestComparePathsUnchanged(t *testing.T) {
	dir := t.TempDir()
	src := `package p
func F() error { return nil }
`
	path := filepath.Join(dir, "f.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	rev, err := review.ComparePaths(dir, dir, "collapsed")
	if err != nil {
		t.Fatal(err)
	}
	if rev.Changed {
		t.Fatalf("unexpected change: %+v", rev)
	}
}
