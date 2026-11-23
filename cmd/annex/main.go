// Command annex is Annex from a terminal: github.com/eversonder/annex-cli/cli.
package main

import (
	"context"
	"os"

	"github.com/eversonder/annex-cli/cli"
)

func main() {
	app := &cli.App{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Open: cli.OpenBrowser}
	os.Exit(app.Run(context.Background(), os.Args[1:]))
}
