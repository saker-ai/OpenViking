// Command vikingbot is the multi-channel Agent bot process.
package main

import (
	"fmt"
	"os"

	"github.com/saker-ai/ctxhub/internal/bot"
	"github.com/saker-ai/ctxhub/internal/version"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(version.String())
		return
	}
	if err := bot.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "vikingbot:", err)
		os.Exit(1)
	}
}
