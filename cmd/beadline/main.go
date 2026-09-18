// Command beadline forecasts when high-level beads (milestones, epics, goals)
// will be done, from how long AI agents actually take. See docs/design.md.
package main

import (
	"os"

	"github.com/alexknips/beadline/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
