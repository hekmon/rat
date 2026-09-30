package main

import (
	"fmt"
	"os"
)

func main() {
	// Not implemented yet: fail rather than pretend to serve.
	fmt.Fprintln(os.Stderr, "rat: not implemented yet")
	os.Exit(1)
}
