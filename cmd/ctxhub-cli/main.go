// Command ov is the OpenViking CLI.
package main

import (
	"fmt"
	"os"

	"github.com/saker-ai/ctxhub/internal/cli"
	"github.com/saker-ai/ctxhub/internal/version"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(version.String())
		return
	}
	if err := cli.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "ov:", err)
		os.Exit(1)
	}
}
