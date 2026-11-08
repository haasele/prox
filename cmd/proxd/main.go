package main

import (
	"github.com/haasele/prox/internal/cli"
	"github.com/haasele/prox/internal/version"
)

func main() {
	cli.DaemonMain(version.Version)
}
