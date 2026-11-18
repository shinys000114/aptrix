// SPDX-License-Identifier: GPL-3.0-only

// Command aptrix manages aptly dist x component matrices from a TOML file.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/shinys000114/aptrix/internal/cli"
)

// Set by the linker (see .goreleaser.yaml and Makefile).
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	cli.Version, cli.Commit, cli.Date = version, commit, date
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case err == nil:
	case errors.Is(err, cli.ErrUsage):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "aptrix:", err)
		os.Exit(1)
	}
}
