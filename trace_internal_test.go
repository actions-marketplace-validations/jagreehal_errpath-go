package errpath

import (
	"go/ast"
	"go/parser"
	"maps"
	"slices"
	"testing"
)

func TestWrappedArgs(t *testing.T) {
	for format, want := range map[string][]int{
		`"%w: %v"`:         {1},
		`"%v: %w"`:         {2},
		`"%d%% %w"`:        {2},
		`"%*d %w"`:         {3},
		`"%[2]w %[1]v"`:    {2},
		`"%w and %w"`:      {1, 2},
		`"%-10s %+.2f %w"`: {3},
		`"no verbs"`:       nil,
	} {
		expr, err := parser.ParseExpr("fmt.Errorf(" + format + ", a, b, c)")
		if err != nil {
			t.Fatal(err)
		}
		got := slices.Sorted(maps.Keys(wrappedArgs(expr.(*ast.CallExpr))))
		if !slices.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", format, got, want)
		}
	}
}
