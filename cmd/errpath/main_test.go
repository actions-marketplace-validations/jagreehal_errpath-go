package main

import (
	"slices"
	"testing"
)

func TestParseFlags(t *testing.T) {
	var base, out string
	values := map[string]*string{"--base": &base, "-o": &out}
	rest, err := parseFlags([]string{"pkg/a", "--base", "main", "pkg/b", "-o=r.json"}, values)
	if err != nil || base != "main" || out != "r.json" || !slices.Equal(rest, []string{"pkg/a", "pkg/b"}) {
		t.Fatalf("base=%q out=%q rest=%v err=%v", base, out, rest, err)
	}
	for _, bad := range [][]string{{"--base"}, {"--nope", "x"}} {
		if _, err := parseFlags(bad, values); err == nil {
			t.Fatalf("%v should fail", bad)
		}
	}
}

func TestFindFunctionsPrefersExactID(t *testing.T) {
	r := run([]string{"diagram", "../../examples/checkout", "PlaceOrder", "-o", t.TempDir() + "/d.mmd"})
	if r != 0 {
		t.Fatalf("exit %d", r)
	}
	if run([]string{"diagram", "../../examples/checkout", "Nope"}) != 1 {
		t.Fatal("missing function should exit 1")
	}
	if run([]string{"bogus"}) != 2 {
		t.Fatal("unknown command should exit 2")
	}
}
