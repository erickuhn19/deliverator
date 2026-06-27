// Command deliverator is a single-binary, non-custodial Hyperliquid execution +
// tracking CLI designed to be driven by an autonomous agent (OpenClaw). See
// TOOLS.md for the agent contract and README.md for the human overview.
package main

import (
	_ "embed"

	"github.com/erickuhn19/deliverator/cmd"
)

// TOOLS.md is embedded so `deliverator tools` can print the contract on any box,
// with no files alongside the binary.
//
//go:embed TOOLS.md
var toolsMarkdown string

func main() {
	cmd.ToolsMarkdown = toolsMarkdown
	cmd.Execute()
}
