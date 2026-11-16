// Command outpost is the Outpost server. It runs on the cluster.
package main

import (
	"fmt"
	"os"
)

const usage = "usage: outpost <serve|migrate|images>"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve", "migrate", "images":
		fmt.Fprintf(os.Stderr, "outpost %s: not built yet\n", os.Args[1])
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
}
