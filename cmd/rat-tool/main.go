package main

import (
	"fmt"
	"os"
)

func main() {
	// Not implemented yet: fail rather than pretend to succeed.
	fmt.Fprintln(os.Stderr, "rat-tool: not implemented yet")
	os.Exit(1)
}
