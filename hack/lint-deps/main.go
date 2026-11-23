package main

import (
	"fmt"
	"os"
	"time"
)

// Run it from the root of the repository: `mise run lint:deps`.
func main() {
	problems, err := lint(os.DirFS("."), time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "lint:deps:", err)
		os.Exit(2)
	}
	for _, problem := range problems {
		fmt.Fprintln(os.Stderr, problem)
	}
	if len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "lint:deps: %d problem(s) with the dependency policy (AGENTS.md, \"Dependencies and versions\")\n", len(problems))
		os.Exit(1)
	}
}
