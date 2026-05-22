// Command kvault is the dual-mode entry: MCP server over stdio,
// dashboard HTTP server, or one-shot CLI for index / search / stats /
// purge. Mode is chosen by flag at startup. See ../../../IMPL_PLAN.md
// T-C.9 for the full surface specification.
//
// This file is the T-C.1 bootstrap stub: it just prints the version
// and exits so the module is buildable and the CI matrix has something
// to compile. T-C.9 replaces the body.
package main

import (
	"fmt"
	"os"
)

// Version is overridden at build time via
//
//	-ldflags="-X main.Version=v0.1.0"
var Version = "0.1.0-dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(Version)
		return
	}
	fmt.Fprintln(os.Stderr,
		"kvault: T-C.1 bootstrap. Real entry lands in T-C.9 "+
			"(MCP / dashboard / one-shot modes).")
	os.Exit(0)
}
