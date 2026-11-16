// Command outpost-agent is the machine agent. It runs on each attached machine and dials out to the server.
package main

import (
	"fmt"
	"os"
)

const usage = "usage: outpost-agent <pair|run>"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "pair", "run":
		fmt.Fprintf(os.Stderr, "outpost-agent %s: not built yet\n", os.Args[1])
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
}
