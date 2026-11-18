// SPDX-License-Identifier: GPL-3.0-only

// Package cli implements the aptrix command line.
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/shinys000114/aptrix/internal/aptly"
	"github.com/shinys000114/aptrix/internal/config"
)

// Build information, set by main.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// ErrUsage marks command line mistakes (exit code 2).
var ErrUsage = errors.New("usage error")

const usage = `aptrix - manage aptly dist x component matrices from a TOML file

Usage:
  aptrix [-c FILE|URL] [--url URL] <command> [args]

Commands:
  plan   [alias...]                         show what sync would change (read-only)
  sync   [alias...] [--prune] [--force]     create/remove repos, components and dists
  status [alias...]                         package count per dist x component (read-only)
  ls     <alias> [component] [--dist d,..]  list packages (read-only)
  add    <alias> <component> <file.deb>...  add packages and republish
  rm     <alias> <component> <package>[=version] [--keep N]
                                            remove packages and republish
  version                                   print version

Run "aptrix <command> -h" for command flags.

Environment:
  APTRIX_CONFIG                 config file or http(s) URL (default: ./aptrix.toml,
                                ~/.config/aptrix/aptrix.toml, /etc/aptrix/aptrix.toml)
  APTRIX_URL                    overrides aptly.url
  APTRIX_TOKEN                  sent as "Authorization: Bearer"
  APTRIX_USERNAME, APTRIX_PASSWORD  HTTP basic auth
`

type env struct {
	ctx     context.Context
	stdout  io.Writer
	stderr  io.Writer
	stdin   io.Reader
	isTTY   bool
	cfgPath string
	url     string
}

// Run executes aptrix with args (without the program name), reading
// confirmations from os.Stdin when it is a terminal.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return RunWith(ctx, args, Streams{Stdin: os.Stdin, Stdout: stdout, Stderr: stderr, IsTTY: isTerminal(os.Stdin)})
}

// Streams are the I/O of one invocation.
type Streams struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// IsTTY enables interactive confirmation prompts.
	IsTTY bool
}

// RunWith is Run with explicit I/O.
func RunWith(ctx context.Context, args []string, s Streams) error {
	stdout, stderr := s.Stdout, s.Stderr
	e := &env{ctx: ctx, stdout: stdout, stderr: stderr, stdin: s.Stdin, isTTY: s.IsTTY}
	gf := flag.NewFlagSet("aptrix", flag.ContinueOnError)
	gf.SetOutput(stderr)
	gf.Usage = func() { fmt.Fprint(stderr, usage) }
	gf.StringVar(&e.cfgPath, "c", "", "config file or http(s) URL")
	gf.StringVar(&e.cfgPath, "config", "", "config file or http(s) URL")
	gf.StringVar(&e.url, "url", "", "aptly API URL (overrides config)")
	if err := gf.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return ErrUsage
	}
	rest := gf.Args()
	if len(rest) == 0 {
		fmt.Fprint(stderr, usage)
		return ErrUsage
	}
	cmd, rest := rest[0], rest[1:]
	switch cmd {
	case "plan":
		return e.cmdSync(rest, true)
	case "sync", "apply":
		return e.cmdSync(rest, false)
	case "status":
		return e.cmdStatus(rest)
	case "ls", "list":
		return e.cmdLs(rest)
	case "add":
		return e.cmdAdd(rest)
	case "rm", "remove":
		return e.cmdRm(rest)
	case "version":
		fmt.Fprintf(stdout, "aptrix %s", Version)
		if Commit != "" {
			fmt.Fprintf(stdout, " (%s %s)", Commit, Date)
		}
		fmt.Fprintln(stdout)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	}
	fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
	return ErrUsage
}

// parse parses flags that may appear before, between or after positionals.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func newFlags(e *env, name, args string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.Usage = func() {
		fmt.Fprintf(e.stderr, "Usage: aptrix %s %s\n", name, args)
		fs.PrintDefaults()
	}
	return fs
}

func (e *env) load() (*config.Config, error) {
	path, err := config.Find(e.cfgPath)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if e.url != "" {
		cfg.Aptly.URL = e.url
	} else if u := os.Getenv("APTRIX_URL"); u != "" {
		cfg.Aptly.URL = u
	}
	return cfg, nil
}

func (e *env) client(cfg *config.Config, readOnly bool) (*aptly.Client, error) {
	c, err := aptly.New(cfg.Aptly.URL, aptly.Options{
		ReadOnly: readOnly,
		Token:    os.Getenv("APTRIX_TOKEN"),
		Username: os.Getenv("APTRIX_USERNAME"),
		Password: os.Getenv("APTRIX_PASSWORD"),
	})
	if err != nil {
		return nil, err
	}
	if !readOnly && cfg.Signing.Skip && !c.IsLoopback() {
		return nil, fmt.Errorf("signing.skip is only allowed against a loopback aptly (got %s)", c.URL())
	}
	return c, nil
}

func signing(cfg *config.Config) aptly.Signing {
	if cfg.Signing.Skip {
		return aptly.Signing{Skip: true}
	}
	return aptly.Signing{GpgKey: cfg.Signing.GpgKey, PassphraseFile: cfg.Signing.PassphraseFile, Batch: true}
}

// confirm asks for "yes" on a terminal. Without a terminal it fails, so a
// CI job never hangs on a prompt.
func (e *env) confirm(yes bool, what string) error {
	if yes {
		return nil
	}
	if !e.isTTY {
		return fmt.Errorf("%s needs confirmation: rerun with --yes", what)
	}
	fmt.Fprintf(e.stderr, "%s? type 'yes' to continue: ", what)
	line, _ := bufio.NewReader(e.stdin).ReadString('\n')
	if strings.TrimSpace(line) != "yes" {
		return errors.New("aborted")
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, v)
	}
	return out
}
