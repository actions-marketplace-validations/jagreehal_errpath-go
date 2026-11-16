package errpath_test

import (
	"fmt"

	"github.com/jagreehal/errpath-go"
)

func ExampleAnalysePath() {
	report, err := errpath.AnalysePath("examples/checkout")
	if err != nil {
		panic(err)
	}
	for _, f := range report.Functions {
		if f.Name == "PlaceOrder" {
			fmt.Println(f.Name, len(f.Nodes) > 0)
			break
		}
	}
	// Output: PlaceOrder true
}

func ExampleFunctionToMachine() {
	report, err := errpath.AnalysePath("examples/checkout")
	if err != nil {
		panic(err)
	}
	for _, f := range report.Functions {
		if f.Name == "PlaceOrder" {
			m := errpath.FunctionToMachine(f)
			fmt.Println(m.ID, m.States["Success"].Type)
			break
		}
	}
	// Output: PlaceOrder final
}
