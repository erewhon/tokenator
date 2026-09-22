// Command tokenator is a token profiler for AI coding agents. The command
// line itself lives in package cli so the pitf unified CLI can mount it;
// this wrapper only maps Run's result to an exit code.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/erewhon/tokenator/cli"
)

// version is stamped by goreleaser via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cli.Version = version
	err := cli.Run(context.Background(), os.Args[1:])
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return
	}
	var ue *cli.UsageError
	if errors.As(err, &ue) {
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
