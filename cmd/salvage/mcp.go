package main

// The `salvage mcp` subcommand (spec 0032): serve the Model Context Protocol
// server over stdio so an agent runtime can drive Salvage's restore/verify/
// attest loop as structured tools.
//
// Wiring: main.go's dispatch switch needs the case
//
//	case "mcp":
//		cmdMCP(os.Args[2:])
//
// and the usage text the line
//
//	salvage mcp                             serve Salvage as an MCP server over stdio (agent tools for run/check/inspect/last-good/fleet/verify/attest/scaffold)

import (
	"flag"
	"fmt"
	"os"

	"salvage.sh/internal/mcpserver"
)

// cmdMCP starts the MCP server on stdin/stdout and serves until the host
// closes the transport. It is non-interactive and inherits the process
// environment and ~/.salvage/credentials (spec 0032 R1); all human-readable
// diagnostics go to stderr, keeping stdout a clean protocol stream.
func cmdMCP(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	_ = fs.Parse(args)
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: salvage mcp (no arguments; speaks MCP on stdio)")
		os.Exit(2)
	}
	if err := mcpserver.Serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mcp transport error:", err)
		os.Exit(2)
	}
}
