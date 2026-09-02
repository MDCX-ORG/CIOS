// cios CLI entry point. Per PRMT-012 §5 MUST, main is a thin wrapper
// around cli.Main — all logic lives in the cli/ package so tests can
// drive it without forking processes.
package main

import (
	"os"

	"github.com/yurimeng/cios/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
