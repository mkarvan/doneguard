// Command doneguard stops Claude Code and Codex CLI from declaring a task
// done until verify commands pass and protected paths are untouched.
package main

import (
	"os"

	"github.com/mkarvan/doneguard/internal/cli"
)

func main() {
	os.Exit(cli.New().Run(os.Args[1:]))
}
