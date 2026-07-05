// Command openviking-migrate performs ovpack import/export and vectordb backend migrations.
package main

import (
	"fmt"
	"os"

	"github.com/saker-ai/ctxhub/internal/migrate"
	"github.com/saker-ai/ctxhub/internal/version"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(version.String())
		return
	}
	if err := migrate.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "openviking-migrate:", err)
		os.Exit(1)
	}
}
