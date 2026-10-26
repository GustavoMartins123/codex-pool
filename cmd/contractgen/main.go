package main

import (
	"flag"
	"fmt"
	"os"

	"codex-pool-proxy/internal/contractgen"
)

func main() {
	root := flag.String("root", ".", "repository root")
	check := flag.Bool("check", false, "fail if generated contracts are missing or stale")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "contractgen does not accept positional arguments")
		os.Exit(1)
	}
	if err := contractgen.Run(*root, *check); err != nil {
		fmt.Fprintln(os.Stderr, "contractgen:", err)
		os.Exit(1)
	}
}
