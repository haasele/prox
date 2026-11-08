// Command prox is the single binary form: invoked as proxd (or prox daemon)
// it runs the daemon; otherwise it behaves as the gw CLI.
package main

import (
	"os"
	"path/filepath"

	"github.com/haasele/prox/internal/cli"
	"github.com/haasele/prox/internal/version"
)

func main() {
	if filepath.Base(os.Args[0]) == "proxd" || (len(os.Args) > 1 && os.Args[1] == "daemon") {
		cli.DaemonMain(version.Version)
		return
	}
	cli.Main(version.Version)
}
