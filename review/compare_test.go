package review_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jagreehal/errpath-go/review"
)

func compareSrc(t *testing.T, base, head string) review.Review {
	t.Helper()
	b, h := t.TempDir(), t.TempDir()
	for dir, src := range map[string]string{b: base, h: head} {
		if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rev, err := review.ComparePaths(b, h, "collapsed")
	if err != nil {
		t.Fatal(err)
	}
	return rev
}

func TestAllowedPanicIsNotANewFinding(t *testing.T) {
	rev := compareSrc(t,
		"package p\nfunc F() {}\n",
		"package p\nfunc F() {\n\t// errpath: allow(no-panic) startup only\n\tpanic(1)\n}\n")
	if len(rev.NewFindings) != 0 || len(rev.NewAllows) != 1 {
		t.Fatalf("findings=%+v allows=%+v", rev.NewFindings, rev.NewAllows)
	}
	if rev.Risk == review.RiskHigh {
		t.Fatal("an allowed panic must not raise risk to high")
	}
	if !strings.Contains(rev.Markdown, "New allow comments") {
		t.Fatalf("markdown should list the allow:\n%s", rev.Markdown)
	}
}

func TestRemovingAnAllowIsANewFinding(t *testing.T) {
	rev := compareSrc(t,
		"package p\nfunc F() {\n\t// errpath: allow(no-panic) startup only\n\tpanic(1)\n}\n",
		"package p\nfunc F() {\n\tpanic(1)\n}\n")
	if len(rev.NewFindings) != 1 || rev.Risk != review.RiskHigh {
		t.Fatalf("findings=%+v risk=%s", rev.NewFindings, rev.Risk)
	}
}

func TestExistingPanicIsNotNew(t *testing.T) {
	src := "package p\nfunc F() {\n\tpanic(1)\n}\n"
	rev := compareSrc(t, src, "\n\n"+src)
	if rev.Changed {
		t.Fatalf("moving lines is not a change: %+v", rev)
	}
}

func TestRenamedFunctionIsMoved(t *testing.T) {
	rev := compareSrc(t,
		"package p\nfunc A() error { return nil }\nfunc Old() error {\n\tif err := A(); err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n",
		"package p\nfunc A() error { return nil }\nfunc New() error {\n\tif err := A(); err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n")
	if rev.Changed || rev.Moved != 1 {
		t.Fatalf("changed=%v moved=%d functions=%+v", rev.Changed, rev.Moved, rev.Functions)
	}
}

func TestStepMovedIntoBranchIsAChange(t *testing.T) {
	rev := compareSrc(t,
		"package p\nfunc A() error { return nil }\nfunc F(x bool) error {\n\tif err := A(); err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n",
		"package p\nfunc A() error { return nil }\nfunc F(x bool) error {\n\tif x {\n\t\tif err := A(); err != nil {\n\t\t\treturn err\n\t\t}\n\t}\n\treturn nil\n}\n")
	if len(rev.Functions) != 1 || len(rev.Functions[0].Changed) != 1 || rev.Risk != review.RiskModerate {
		t.Fatalf("risk=%s functions=%+v", rev.Risk, rev.Functions)
	}
	if !strings.Contains(rev.Markdown, "moved from `top level` to `if x: yes`") {
		t.Fatalf("markdown:\n%s", rev.Markdown)
	}
}

func TestNewBadAllowIsAWarning(t *testing.T) {
	rev := compareSrc(t,
		"package p\nfunc F() {}\n",
		"package p\nfunc F() {\n\t// errpath: allow(no-panic)\n}\n")
	if len(rev.NewFindings) != 1 || rev.NewFindings[0].Rule != "bad-allow" || rev.NewFindings[0].Severity != "warning" {
		t.Fatalf("findings=%+v", rev.NewFindings)
	}
}

func TestNewParseFailureIsANote(t *testing.T) {
	rev := compareSrc(t, "package p\nfunc F() {}\n", "package p\nfunc F( {}\n")
	if len(rev.Notes) != 1 || !rev.Changed || rev.Risk != review.RiskModerate {
		t.Fatalf("notes=%v changed=%v risk=%s", rev.Notes, rev.Changed, rev.Risk)
	}
}

func TestMarkdownEscapesAuthorText(t *testing.T) {
	rev := compareSrc(t,
		"package p\nfunc F() {}\n",
		"package p\nfunc F() {\n\t// errpath: allow(no-panic) see ``` @someone ![x](http://e)\n\tpanic(1)\n}\n")
	md := rev.Markdown
	reason := "see ``` @someone ![x](http://e)"
	if !strings.Contains(md, "````"+reason+"````") {
		t.Fatalf("allow reason should sit inside a longer fence:\n%s", md)
	}
}

func TestMarkdownFitsInAComment(t *testing.T) {
	var base, head strings.Builder
	base.WriteString("package p\n")
	head.WriteString("package p\n")
	for i := range 400 {
		name := "Function" + strings.Repeat("X", 80) + string(rune('A'+i%26)) + strings.Repeat("Y", i)
		base.WriteString("func " + name + "() {}\n")
		head.WriteString("func " + name + "() {\n\tpanic(1)\n}\n")
	}
	rev := compareSrc(t, base.String(), head.String())
	if len(rev.Markdown) > 60_000 || !strings.HasPrefix(rev.Markdown, review.Marker) {
		t.Fatalf("markdown length %d", len(rev.Markdown))
	}
	if len(rev.NewFindings) != 400 {
		t.Fatalf("json keeps every finding: %d", len(rev.NewFindings))
	}
}

// TestRunUsesMergeBase: changes merged into main after the branch left must
// not show up as the branch undoing them.
func TestRunUsesMergeBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	write("go.mod", "module example.com/m\n\ngo 1.22\n")
	write("a.go", "package m\nfunc A() {}\n")
	write("b.go", "package m\nfunc B() {}\n")
	run("add", ".")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "feature")
	write("a.go", "package m\nfunc A() {\n\tpanic(1)\n}\n")
	run("commit", "-q", "-am", "feature")
	run("checkout", "-q", "main")
	write("b.go", "package m\nfunc B() {\n\tpanic(2)\n}\n")
	run("commit", "-q", "-am", "main moves on")

	rev, err := review.Run(review.Options{Repo: dir, Base: "main", Head: "feature"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rev.Functions) != 1 || rev.Functions[0].ID != "m.A" {
		t.Fatalf("only the branch's change should show: %+v", rev.Functions)
	}

	sub, err := review.Run(review.Options{Repo: dir, Base: "main", Head: "feature", Paths: []string{"b.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if sub.Changed {
		t.Fatalf("paths should limit the review: %+v", sub.Functions)
	}

	if _, err := review.Run(review.Options{Repo: dir, Base: "nope", Head: "feature"}); err == nil {
		t.Fatal("unknown base should fail")
	}
}

func TestJumpsAreComparedByWhereTheyLand(t *testing.T) {
	loop := func(jump, after string) string {
		return "package p\nfunc store() error { return nil }\nfunc F(in chan int) error {\n\tfor {\n\t\tselect {\n\t\tcase <-in:\n\t\t\tif err := store(); err != nil {\n\t\t\t\treturn err\n\t\t\t}\n\t\tdefault:\n\t\t\t" + jump + "\n\t\t}\n" + after + "\t}\n}\n"
	}
	if rev := compareSrc(t, loop("break", ""), loop("continue", "")); rev.Changed {
		t.Fatalf("break out of a select that ends the loop body is continue: %s", rev.Markdown)
	}
	tail := "\t\tif err := store(); err != nil {\n\t\t\treturn err\n\t\t}\n"
	rev := compareSrc(t, loop("break", tail), loop("continue", tail))
	if !rev.Changed || !strings.Contains(rev.Markdown, "now lands somewhere else") {
		t.Fatalf("continue now skips the store after the select: %s", rev.Markdown)
	}
}

func TestLoopThatNoLongerEnds(t *testing.T) {
	rev := compareSrc(t,
		"package p\nfunc F(x bool) {\n\tfor {\n\t\tif x {\n\t\t\tbreak\n\t\t}\n\t}\n}\n",
		"package p\nfunc F(x bool) {\n\tfor {\n\t\tif x {\n\t\t\tcontinue\n\t\t}\n\t}\n}\n")
	if rev.Risk != review.RiskModerate || !strings.Contains(rev.Markdown, "loop that never finishes") {
		t.Fatalf("risk=%s\n%s", rev.Risk, rev.Markdown)
	}
}

func TestHandlerThatRecordsNothing(t *testing.T) {
	handler := func(body string) string {
		return "package p\nimport \"log/slog\"\nfunc send() error { return nil }\nfunc retry() {}\nfunc F() {\n\tif err := send(); err != nil {\n" + body + "\t}\n}\n"
	}
	recorded := handler("\t\tslog.Error(\"send\", \"error\", err)\n\t\tretry()\n")
	silent := handler("\t\tretry()\n")

	rev := compareSrc(t, "package p\nfunc send() error { return nil }\nfunc retry() {}\nfunc F() {}\n", silent)
	if len(rev.NewFindings) != 1 || rev.NewFindings[0].Rule != "no-trace" || rev.Risk != review.RiskModerate {
		t.Fatalf("a new handler that records nothing: risk=%s findings=%+v", rev.Risk, rev.NewFindings)
	}
	if !strings.Contains(rev.Markdown, "slog.ErrorContext") {
		t.Fatalf("markdown should say how to record:\n%s", rev.Markdown)
	}

	rev = compareSrc(t, recorded, silent)
	if len(rev.NewFindings) != 1 || !strings.Contains(rev.Markdown, "no longer records the error") {
		t.Fatalf("dropping the log line from a handler: findings=%+v\n%s", rev.NewFindings, rev.Markdown)
	}

	rev = compareSrc(t, silent, recorded)
	if len(rev.NewFindings) != 0 || !strings.Contains(rev.Markdown, "now records the error") {
		t.Fatalf("adding a log line: findings=%+v\n%s", rev.NewFindings, rev.Markdown)
	}

	allowed := strings.Replace(silent, "\tif err", "\t// errpath: allow(no-trace) retried until it works\n\tif err", 1)
	rev = compareSrc(t, recorded, allowed)
	if len(rev.NewFindings) != 0 || len(rev.NewAllows) != 1 {
		t.Fatalf("an allowed handler: findings=%+v allows=%+v", rev.NewFindings, rev.NewAllows)
	}
}
