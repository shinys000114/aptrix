// SPDX-License-Identifier: GPL-3.0-only

// Package config loads the aptrix TOML file that declares, per published
// prefix, which distributions and components should exist.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the whole aptrix.toml.
type Config struct {
	Aptly   Aptly               `toml:"aptly"`
	Signing Signing             `toml:"signing"`
	Publish map[string]*Publish `toml:"publish"`
}

// Aptly holds connection settings. Credentials come from the environment
// (APTRIX_TOKEN, or APTRIX_USERNAME/APTRIX_PASSWORD), never from the file.
type Aptly struct {
	URL string `toml:"url"`
}

// Signing is passed to aptly on every publish create/update.
type Signing struct {
	// Skip disables signing. Only meant for tests; aptrix refuses to sync
	// with it unless the aptly URL is a loopback address.
	Skip           bool   `toml:"skip"`
	GpgKey         string `toml:"gpg_key"`
	PassphraseFile string `toml:"passphrase_file"`
}

// Publish describes one published prefix (one aptly "publish" per dist).
type Publish struct {
	Alias string `toml:"-"`

	// Storage is the aptly publish endpoint, e.g. "filesystem:public".
	// Empty means aptly's default publish root.
	Storage string `toml:"storage"`
	// Prefix is the publish prefix, e.g. "my-repo". Empty means ".".
	Prefix string `toml:"prefix"`

	Dists         []string `toml:"dists"`
	Components    []string `toml:"components"`
	Architectures []string `toml:"architectures"`

	// Only restricts a component to a subset of Dists.
	Only map[string][]string `toml:"only"`

	// RepoName is the local repo name template. {dist} and {component}
	// are substituted. Default: "<prefix>-{dist}-{component}".
	RepoName string `toml:"repo_name"`
	// Comment is the template used when creating local repos.
	Comment string `toml:"comment"`

	Origin string `toml:"origin"`
	Label  string `toml:"label"`

	// Keep, when > 0, keeps only the newest Keep versions of each package
	// (per dist, component and architecture) after add and promote.
	Keep int `toml:"keep"`
	// SnapshotMaxAge, e.g. "30d", drops aptrix's automatic snapshots
	// (pre-*) of this publish that are older than this, after each change.
	SnapshotMaxAge string `toml:"snapshot_max_age"`
}

// ParseAge parses durations like "30d", "12h", "90m" (Go syntax plus "d").
func ParseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		var n int
		if _, err := fmt.Sscanf(strings.TrimSuffix(s, "d"), "%d", &n); err != nil || n < 0 || fmt.Sprint(n)+"d" != s {
			return 0, fmt.Errorf("invalid age %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid age %q (use e.g. 30d, 12h)", s)
	}
	return d, nil
}

// Cell is one (dist, component) pair and the local repo backing it.
type Cell struct {
	Dist      string
	Component string
	Repo      string
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_-]*$`)

// DefaultPaths are searched in order when no -c flag or APTRIX_CONFIG is given.
func DefaultPaths() []string {
	paths := []string{"aptrix.toml"}
	if dir, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(dir, "aptrix", "aptrix.toml"))
	}
	return append(paths, "/etc/aptrix/aptrix.toml")
}

// Find returns the config path to use.
func Find(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("APTRIX_CONFIG"); env != "" {
		return env, nil
	}
	for _, p := range DefaultPaths() {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no config file found (tried %s); use -c or APTRIX_CONFIG", strings.Join(DefaultPaths(), ", "))
}

// IsURL reports whether a config location is an http(s) URL.
func IsURL(loc string) bool {
	return strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://")
}

// maxRemoteConfig bounds how much is read from a config URL.
const maxRemoteConfig = 1 << 20

// Load reads and validates a config file, or fetches it when loc is an
// http(s) URL. A URL lets many hosts share one config; it is fetched on
// every run (no cache) and any failure is an error.
func Load(loc string) (*Config, error) {
	var data []byte
	var err error
	if IsURL(loc) {
		data, err = fetch(loc)
	} else {
		data, err = os.ReadFile(loc)
	}
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func fetch(u string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return nil, fmt.Errorf("fetching config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching config %s: HTTP %d", u, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteConfig+1))
	if err != nil {
		return nil, fmt.Errorf("fetching config %s: %w", u, err)
	}
	if len(data) > maxRemoteConfig {
		return nil, fmt.Errorf("config %s is larger than %d bytes", u, maxRemoteConfig)
	}
	return data, nil
}

// Parse decodes and validates TOML data.
func Parse(data []byte) (*Config, error) {
	var c Config
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return nil, err
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		keys := make([]string, len(undec))
		for i, k := range undec {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown config keys: %s", strings.Join(keys, ", "))
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	var errs []error
	if c.Aptly.URL == "" {
		errs = append(errs, errors.New("aptly.url is required"))
	}
	if !c.Signing.Skip && c.Signing.GpgKey == "" {
		errs = append(errs, errors.New("signing.gpg_key is required (or signing.skip = true for tests)"))
	}
	if len(c.Publish) == 0 {
		errs = append(errs, errors.New("at least one [publish.<alias>] table is required"))
	}
	seenRepo := map[string]string{}
	seenTarget := map[string]string{}
	for alias, p := range c.Publish {
		p.Alias = alias
		where := "publish." + alias
		if !nameRe.MatchString(alias) {
			errs = append(errs, fmt.Errorf("%s: invalid alias", where))
		}
		if p.Prefix == "" {
			p.Prefix = "."
		}
		if p.RepoName == "" {
			base := p.Prefix
			if base == "." {
				base = alias
			}
			p.RepoName = strings.ReplaceAll(base, "/", "-") + "-{dist}-{component}"
		}
		if !strings.Contains(p.RepoName, "{dist}") {
			errs = append(errs, fmt.Errorf("%s: repo_name must contain {dist}", where))
		}
		if !strings.Contains(p.RepoName, "{component}") && len(p.Components) > 1 {
			errs = append(errs, fmt.Errorf("%s: repo_name must contain {component} when there is more than one component", where))
		}
		if len(p.Dists) == 0 {
			errs = append(errs, fmt.Errorf("%s: dists is empty", where))
		}
		if len(p.Components) == 0 {
			errs = append(errs, fmt.Errorf("%s: components is empty", where))
		}
		if len(p.Architectures) == 0 {
			errs = append(errs, fmt.Errorf("%s: architectures is empty (aptly cannot infer them for empty repos)", where))
		}
		for _, list := range [][]string{p.Dists, p.Components, p.Architectures} {
			if d := dup(list); d != "" {
				errs = append(errs, fmt.Errorf("%s: duplicate entry %q", where, d))
			}
			for _, v := range list {
				if !nameRe.MatchString(v) {
					errs = append(errs, fmt.Errorf("%s: invalid name %q", where, v))
				}
			}
		}
		for comp, dists := range p.Only {
			if !slices.Contains(p.Components, comp) {
				errs = append(errs, fmt.Errorf("%s.only: %q is not in components", where, comp))
			}
			for _, d := range dists {
				if !slices.Contains(p.Dists, d) {
					errs = append(errs, fmt.Errorf("%s.only.%s: %q is not in dists", where, comp, d))
				}
			}
		}
		if p.Keep < 0 {
			errs = append(errs, fmt.Errorf("%s: keep must be >= 0", where))
		}
		if p.SnapshotMaxAge != "" {
			if d, err := ParseAge(p.SnapshotMaxAge); err != nil {
				errs = append(errs, fmt.Errorf("%s: snapshot_max_age: %v", where, err))
			} else if d <= 0 {
				errs = append(errs, fmt.Errorf("%s: snapshot_max_age must be > 0", where))
			}
		}
		target := p.Storage + ":" + p.Prefix
		if other, ok := seenTarget[target]; ok {
			errs = append(errs, fmt.Errorf("%s: same storage/prefix as publish.%s", where, other))
		}
		seenTarget[target] = alias
		for _, cell := range p.Cells() {
			if other, ok := seenRepo[cell.Repo]; ok && other != alias {
				errs = append(errs, fmt.Errorf("%s: local repo %q is also used by publish.%s", where, cell.Repo, other))
			}
			seenRepo[cell.Repo] = alias
			if !nameRe.MatchString(cell.Repo) {
				errs = append(errs, fmt.Errorf("%s: generated repo name %q is invalid", where, cell.Repo))
			}
		}
	}
	return errors.Join(errs...)
}

func dup(list []string) string {
	seen := map[string]bool{}
	for _, v := range list {
		if seen[v] {
			return v
		}
		seen[v] = true
	}
	return ""
}

// Aliases returns publish aliases in sorted order.
func (c *Config) Aliases() []string {
	out := make([]string, 0, len(c.Publish))
	for a := range c.Publish {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Select returns the publishes named by aliases (all when empty).
func (c *Config) Select(aliases []string) ([]*Publish, error) {
	if len(aliases) == 0 {
		aliases = c.Aliases()
	}
	out := make([]*Publish, 0, len(aliases))
	for _, a := range aliases {
		p, ok := c.Publish[a]
		if !ok {
			return nil, fmt.Errorf("unknown publish alias %q (known: %s)", a, strings.Join(c.Aliases(), ", "))
		}
		out = append(out, p)
	}
	return out, nil
}

// HasDist reports whether component is published for dist.
func (p *Publish) HasDist(component, dist string) bool {
	if !slices.Contains(p.Components, component) || !slices.Contains(p.Dists, dist) {
		return false
	}
	only, ok := p.Only[component]
	return !ok || slices.Contains(only, dist)
}

// RepoFor returns the local repo name for a cell.
func (p *Publish) RepoFor(dist, component string) string {
	r := strings.ReplaceAll(p.RepoName, "{dist}", dist)
	return strings.ReplaceAll(r, "{component}", component)
}

// CommentFor returns the comment used when creating a local repo.
func (p *Publish) CommentFor(dist, component string) string {
	c := strings.ReplaceAll(p.Comment, "{dist}", dist)
	return strings.ReplaceAll(c, "{component}", component)
}

// Cells returns every desired (dist, component) cell, ordered by dist then component.
func (p *Publish) Cells() []Cell {
	var out []Cell
	for _, d := range p.Dists {
		out = append(out, p.CellsFor(d)...)
	}
	return out
}

// CellsFor returns the desired cells of one dist, sorted by component.
func (p *Publish) CellsFor(dist string) []Cell {
	var out []Cell
	for _, c := range p.Components {
		if p.HasDist(c, dist) {
			out = append(out, Cell{Dist: dist, Component: c, Repo: p.RepoFor(dist, c)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Component < out[j].Component })
	return out
}

// Target returns "storage:prefix" as aptly displays it.
func (p *Publish) Target() string {
	if p.Storage == "" {
		return p.Prefix
	}
	return p.Storage + ":" + p.Prefix
}
