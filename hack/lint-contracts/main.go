package main

import (
	"fmt"
	"os"
)

// Run it from the root of the repository: `mise run lint:contracts`.
func main() {
	problems, err := lint(os.DirFS("."))
	if err != nil {
		fmt.Fprintln(os.Stderr, "lint:contracts:", err)
		os.Exit(2)
	}
	for _, problem := range problems {
		fmt.Fprintln(os.Stderr, problem)
	}
	if len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "lint:contracts: %d problem(s) in the links between %s and the tests\n",
			len(problems), readmePath)
		os.Exit(1)
	}
}
