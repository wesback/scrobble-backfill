package main

import (
	"fmt"
	"io"
	"os"

	"github.com/wesback/scrobble-backfill/internal/cli"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprintln(stdout, version)
		return 0
	}
	return cli.Run(args, stdout, stderr)
}
