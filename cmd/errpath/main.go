package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jagreehal/errpath-go"
	"github.com/jagreehal/errpath-go/review"
)

const usage = `errpath — see how Go code handles errors

Usage:
  errpath ir [path] [-o file]
  errpath diagram <path> <func> [-o file]
  errpath machine <path> <func> [-o file]
  errpath review --base <ref> [--head <ref>] [--repo <dir>] [paths...] [--format markdown|json] [--diagrams open|collapsed] [-o file]
  errpath version
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "ir":
		return cmdIR(args[1:])
	case "diagram":
		return cmdDiagram(args[1:])
	case "machine":
		return cmdMachine(args[1:])
	case "review":
		return cmdReview(args[1:])
	case "version", "-version", "--version":
		fmt.Println(errpath.Version)
		return 0
	case "-h", "-help", "--help", "help":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s", args[0], usage)
		return 2
	}
}

func cmdIR(args []string) int {
	path, out, err := parsePathOut(args, ".")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	report, err := errpath.AnalysePath(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return emitJSON(report, out)
}

func cmdDiagram(args []string) int {
	path, name, out, err := parsePathFuncOut(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fn, code := loadFunction(path, name)
	if code != 0 {
		return code
	}
	return emitText(errpath.RenderFunction(*fn), out)
}

func cmdMachine(args []string) int {
	path, name, out, err := parsePathFuncOut(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fn, code := loadFunction(path, name)
	if code != 0 {
		return code
	}
	return emitJSON(errpath.FunctionToMachine(*fn), out)
}

func cmdReview(args []string) int {
	opts := review.Options{Head: "HEAD", Diagrams: "collapsed"}
	format, out := "markdown", ""
	values := map[string]*string{
		"--base": &opts.Base, "--head": &opts.Head, "--repo": &opts.Repo,
		"--format": &format, "--diagrams": &opts.Diagrams, "-o": &out,
	}
	paths, err := parseFlags(args, values)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	opts.Paths = paths
	switch {
	case opts.Base == "":
		err = errors.New("--base is required")
	case opts.Diagrams != "open" && opts.Diagrams != "collapsed":
		err = errors.New("--diagrams must be open or collapsed")
	case format != "markdown" && format != "json":
		err = errors.New("--format must be markdown or json")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	rev, err := review.Run(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if format == "json" {
		return emitJSON(rev, out)
	}
	text := rev.Markdown
	if text == "" {
		text = rev.Summary
	}
	return emitText(text, out)
}

// parseFlags reads `--name value` and `--name=value` into values, wherever
// they appear, and returns the other arguments. The Action passes paths
// before flags, which the flag package would stop at.
func parseFlags(args []string, values map[string]*string) ([]string, error) {
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			continue
		}
		name, value, inline := strings.Cut(a, "=")
		dst, ok := values[name]
		if !ok {
			return nil, fmt.Errorf("unknown flag %s", a)
		}
		if !inline {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s needs a value", name)
			}
			i++
			value = args[i]
		}
		*dst = value
	}
	return rest, nil
}

func loadFunction(path, name string) (*errpath.Function, int) {
	report, err := errpath.AnalysePath(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, 1
	}
	hits := findFunctions(report.Functions, name)
	switch len(hits) {
	case 1:
		for _, u := range report.Unanalysed {
			if u.Function == hits[0].ID {
				fmt.Fprintf(os.Stderr, "errpath: note: %s:%d: %s: %s\n", u.File, u.Line, u.Code, u.Reason)
			}
		}
		return &hits[0], 0
	case 0:
		fmt.Fprintf(os.Stderr, "no function %q in %s\n", name, path)
	default:
		ids := make([]string, len(hits))
		for i, f := range hits {
			ids[i] = f.ID
		}
		fmt.Fprintf(os.Stderr, "%q is ambiguous; use one of:\n  %s\n", name, strings.Join(ids, "\n  "))
	}
	return nil, 1
}

// findFunctions matches an id, an id suffix after a dot, or a bare name.
// An exact id wins outright.
func findFunctions(fns []errpath.Function, name string) []errpath.Function {
	var hits []errpath.Function
	for _, f := range fns {
		if f.ID == name {
			return []errpath.Function{f}
		}
		if f.Name == name || strings.HasSuffix(f.ID, "."+name) {
			hits = append(hits, f)
		}
	}
	return hits
}

func parsePathOut(args []string, def string) (path, out string, err error) {
	pos, err := parseFlags(args, map[string]*string{"-o": &out})
	if err != nil {
		return "", "", err
	}
	path = def
	if len(pos) > 0 {
		path = pos[0]
	}
	return path, out, nil
}

func parsePathFuncOut(args []string) (path, name, out string, err error) {
	pos, err := parseFlags(args, map[string]*string{"-o": &out})
	if err != nil {
		return "", "", "", err
	}
	if len(pos) < 2 {
		return "", "", "", errors.New("usage: errpath <diagram|machine> <path> <func> [-o file]")
	}
	return pos[0], pos[1], out, nil
}

func emitJSON(v any, out string) int {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return emitText(string(data), out)
}

func emitText(text, out string) int {
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if out != "" {
		if err := os.WriteFile(out, []byte(text), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	fmt.Print(text)
	return 0
}
