package main

import (
	"os"

	"github.com/devsy-org/devsy/cmd"
	"github.com/devsy-org/devsy/pkg/ghcredentials"
)

func main() {
	// gh must receive the caller's exact environment and streams, without
	// Devsy's envfile loading, argument parsing, logging, or telemetry.
	if len(os.Args) >= 4 && os.Args[1] == "internal" && os.Args[2] == "gh" {
		os.Exit(ghcredentials.Run(os.Args[3:]))
	}
	cmd.Execute()
}
