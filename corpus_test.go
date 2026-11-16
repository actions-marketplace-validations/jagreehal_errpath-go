package errpath_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jagreehal/errpath-go"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/corpus")

// TestCorpusGolden pins the IR and diagrams for every corpus file. After an
// intended change, run `go test -run TestCorpusGolden -update` and review the diff.
func TestCorpusGolden(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "corpus", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no corpus files: %v", err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			r, err := errpath.AnalysePath(file)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.ParseFailures) > 0 {
				t.Fatalf("parse: %+v", r.ParseFailures)
			}
			diagrams := map[string]string{}
			for _, f := range r.Functions {
				diagrams[f.ID] = errpath.RenderFunction(f)
			}
			got, err := json.MarshalIndent(struct {
				Report   errpath.Report    `json:"report"`
				Diagrams map[string]string `json:"diagrams"`
			}{r, diagrams}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			golden := strings.TrimSuffix(file, ".go") + ".golden.json"
			if *update {
				if err := os.WriteFile(golden, got, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s differs from the analysis; run `go test -run TestCorpusGolden -update` and review the diff", golden)
			}
		})
	}
}
