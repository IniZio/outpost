// Command outpost-cli starts, lists, watches and steers runs from a laptop.
package main

import (
	"fmt"
	"os"
)

const usage = "usage: outpost-cli <start|list|watch|send>"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "start", "list", "watch", "send":
		fmt.Fprintf(os.Stderr, "outpost-cli %s: not built yet\n", os.Args[1])
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
}
