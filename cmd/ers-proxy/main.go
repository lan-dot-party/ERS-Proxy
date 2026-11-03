// Command ers-proxy serves a portal for the web interfaces of network devices
// and a compatibility proxy for the EDM of Avaya/Extreme ERS switches.
package main

import (
	"os"

	"github.com/lan-dot-party/ers-proxy/cmd/ers-proxy/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
