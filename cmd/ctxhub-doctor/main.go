// Command openviking-doctor performs configuration, connectivity, model, and vectordb health checks.
package main

import (
	"fmt"
	"os"

	"github.com/saker-ai/ctxhub/internal/doctor"
	"github.com/saker-ai/ctxhub/internal/version"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(version.String())
		return
	}
	if err := doctor.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "openviking-doctor:", err)
		os.Exit(1)
	}
}
