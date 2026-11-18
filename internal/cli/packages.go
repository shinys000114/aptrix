// SPDX-License-Identifier: GPL-3.0-only

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/shinys000114/aptrix/internal/aptly"
	"github.com/shinys000114/aptrix/internal/config"
	"github.com/shinys000114/aptrix/internal/debver"
	"github.com/shinys000114/aptrix/internal/plan"
)

// debFile is a .deb named name_version_arch.deb.
type debFile struct {
	Path, Name, Version, Arch string
}

func parseDebName(path string) (debFile, error) {
	base := filepath.Base(path)
	if !strings.HasSuffix(base, ".deb") {
		return debFile{}, fmt.Errorf("%s: not a .deb file", base)
	}
	parts := strings.Split(strings.TrimSuffix(base, ".deb"), "_")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return debFile{}, fmt.Errorf("%s: file name must be <name>_<version>_<arch>.deb", base)
	}
	return debFile{Path: path, Name: parts[0], Version: parts[1], Arch: parts[2]}, nil
}

// detectDist returns the dist encoded as a "~<dist>" version suffix.
func detectDist(version string, dists []string) string {
	i := strings.LastIndexByte(version, '~')
	if i < 0 {
		return ""
	}
	tail := version[i+1:]
	if slices.Contains(dists, tail) {
		return tail
	}
	return ""
}

// targetDists resolves --dist for p ("" = none, "all" = every dist).
func targetDists(p *config.Publish, flag string) ([]string, error) {
	if flag == "" {
		return nil, nil
	}
	if flag == "all" {
		return p.Dists, nil
	}
	ds := splitList(flag)
	for _, d := range ds {
		if !slices.Contains(p.Dists, d) {
			return nil, fmt.Errorf("dist %q is not in publish.%s (dists: %s)", d, p.Alias, strings.Join(p.Dists, ", "))
		}
	}
	return ds, nil
}

func (e *env) cmdAdd(args []string) error {
	fs := newFlags(e, "add", "<alias> <component> <file.deb>... [--dist d1,d2|all]")
	distFlag := fs.String("dist", "", `target dists (comma separated or "all"); default: from the "~<dist>" version suffix`)
	forceReplace := fs.Bool("force-replace", false, "replace an existing package with the same name/version/arch but different content\n(also overwrites the published pool file; clients that cached the old file will see a hash mismatch)")
	noPublish := fs.Bool("no-publish", false, "add to the repos but do not republish")
	pos, err := parse(fs, args)
	if err != nil || len(pos) < 3 {
		fs.Usage()
		return ErrUsage
	}
	alias, comp, files := pos[0], pos[1], pos[2:]
	cfg, err := e.load()
	if err != nil {
		return err
	}
	pubs, err := cfg.Select([]string{alias})
	if err != nil {
		return err
	}
	p := pubs[0]
	if !slices.Contains(p.Components, comp) {
		return fmt.Errorf("component %q is not in publish.%s (components: %s)", comp, alias, strings.Join(p.Components, ", "))
	}
	explicit, err := targetDists(p, *distFlag)
	if err != nil {
		return err
	}

	// Resolve every file to its target dists before touching aptly.
	byDist := map[string][]debFile{}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			return err
		}
		d, err := parseDebName(f)
		if err != nil {
			return err
		}
		if d.Arch != "all" && !slices.Contains(p.Architectures, d.Arch) {
			return fmt.Errorf("%s: architecture %s is not published by %s (%s)", filepath.Base(f), d.Arch, alias, strings.Join(p.Architectures, ", "))
		}
		dists := explicit
		if dists == nil {
			dist := detectDist(d.Version, p.Dists)
			if dist == "" {
				return fmt.Errorf("%s: cannot tell the dist from version %q (no ~<dist> suffix); use --dist", filepath.Base(f), d.Version)
			}
			dists = []string{dist}
		}
		for _, dist := range dists {
			if !p.HasDist(comp, dist) {
				return fmt.Errorf("%s/%s is not a cell of publish.%s (see [publish.%s.only])", dist, comp, alias, alias)
			}
			byDist[dist] = append(byDist[dist], d)
		}
	}

	c, err := e.client(cfg, false)
	if err != nil {
		return err
	}
	st, err := plan.Load(e.ctx, c, pubs)
	if err != nil {
		return err
	}
	dists := sortedKeys(byDist)
	for _, dist := range dists {
		repo := p.RepoFor(dist, comp)
		if _, ok := st.Repos[repo]; !ok {
			return fmt.Errorf("repo %s does not exist yet; run 'aptrix sync %s' first", repo, alias)
		}
		if _, ok := st.Published(p, dist); !ok && !*noPublish {
			return fmt.Errorf("%s %s is not published yet; run 'aptrix sync %s' first", p.Target(), dist, alias)
		}
	}

	stamp := fmt.Sprintf("aptrix-%d-%d", time.Now().Unix(), os.Getpid())
	var done []string
	partial := func(err error) error {
		if len(done) == 0 {
			return err
		}
		return fmt.Errorf("%w\n(already added to %s but not republished; fix the problem and rerun the same command)", err, strings.Join(done, ", "))
	}
	for _, dist := range dists {
		repo := p.RepoFor(dist, comp)
		dir := stamp + "-" + dist
		paths := make([]string, len(byDist[dist]))
		for i, d := range byDist[dist] {
			paths[i] = d.Path
		}
		fmt.Fprintf(e.stdout, "upload  %d file(s) -> %s\n", len(paths), repo)
		if err := c.Upload(e.ctx, dir, paths); err != nil {
			c.DeleteUploadDir(e.ctx, dir)
			return partial(err)
		}
		res, err := c.ImportDir(e.ctx, repo, dir, *forceReplace)
		cleanErr := c.DeleteUploadDir(e.ctx, dir)
		if err != nil {
			return partial(err)
		}
		done = append(done, repo)
		if cleanErr != nil {
			fmt.Fprintf(e.stderr, "warning: could not delete upload dir %s: %v\n", dir, cleanErr)
		}
		for _, a := range res.Report.Added {
			fmt.Fprintln(e.stdout, "  "+a)
		}
		for _, r := range res.Report.Removed {
			fmt.Fprintln(e.stdout, "  "+r)
		}
		for _, w := range res.Report.Warnings {
			fmt.Fprintln(e.stderr, "  warning: "+w)
		}
	}
	if *noPublish {
		return nil
	}
	if err := e.republish(c, cfg, p, dists, *forceReplace); err != nil {
		return fmt.Errorf("%w\n(packages are in the repos; rerun the same command to retry publishing)", err)
	}
	return nil
}

func (e *env) republish(c *aptly.Client, cfg *config.Config, p *config.Publish, dists []string, forceOverwrite bool) error {
	for _, dist := range dists {
		fmt.Fprintf(e.stdout, "publish %s %s\n", p.Target(), dist)
		if err := c.UpdatePublish(e.ctx, p.Storage, p.Prefix, dist, signing(cfg), forceOverwrite); err != nil {
			return err
		}
	}
	return nil
}

type pkgRow struct {
	Dist, Component, Repo string
	aptly.Package
}

// collect lists packages of the given cells.
func (e *env) collect(c *aptly.Client, p *config.Publish, comps, dists []string) ([]pkgRow, error) {
	var rows []pkgRow
	for _, d := range dists {
		for _, comp := range comps {
			if !p.HasDist(comp, d) {
				continue
			}
			repo := p.RepoFor(d, comp)
			pkgs, err := c.Packages(e.ctx, repo)
			if aptly.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			for _, pk := range pkgs {
				rows = append(rows, pkgRow{Dist: d, Component: comp, Repo: repo, Package: pk})
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Dist != b.Dist {
			return slices.Index(p.Dists, a.Dist) < slices.Index(p.Dists, b.Dist)
		}
		if a.Component != b.Component {
			return a.Component < b.Component
		}
		if a.Package.Package != b.Package.Package {
			return a.Package.Package < b.Package.Package
		}
		if a.Architecture != b.Architecture {
			return a.Architecture < b.Architecture
		}
		return debver.Compare(a.Version, b.Version) > 0 // newest first
	})
	return rows, nil
}

func (e *env) cmdLs(args []string) error {
	fs := newFlags(e, "ls", "<alias> [component] [--dist d1,d2] [--name pkg]")
	distFlag := fs.String("dist", "", "only these dists (comma separated)")
	name := fs.String("name", "", "only this package name")
	pos, err := parse(fs, args)
	if err != nil || len(pos) < 1 || len(pos) > 2 {
		fs.Usage()
		return ErrUsage
	}
	cfg, err := e.load()
	if err != nil {
		return err
	}
	pubs, err := cfg.Select(pos[:1])
	if err != nil {
		return err
	}
	p := pubs[0]
	comps := p.Components
	if len(pos) == 2 {
		if !slices.Contains(p.Components, pos[1]) {
			return fmt.Errorf("component %q is not in publish.%s", pos[1], p.Alias)
		}
		comps = pos[1:]
	}
	dists := p.Dists
	if *distFlag != "" {
		if dists, err = targetDists(p, *distFlag); err != nil {
			return err
		}
	}
	c, err := e.client(cfg, true)
	if err != nil {
		return err
	}
	rows, err := e.collect(c, p, comps, dists)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DIST\tCOMPONENT\tPACKAGE\tVERSION\tARCH")
	n := 0
	for _, r := range rows {
		if *name != "" && r.Package.Package != *name {
			continue
		}
		n++
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Dist, r.Component, r.Package.Package, r.Version, r.Architecture)
	}
	tw.Flush()
	fmt.Fprintf(e.stdout, "%d package(s)\n", n)
	return nil
}

func (e *env) cmdRm(args []string) error {
	fs := newFlags(e, "rm", "<alias> <component> <package>[=version] [--dist d1,d2|all] [--keep N] [--yes]")
	distFlag := fs.String("dist", "all", `dists to remove from (comma separated or "all")`)
	keep := fs.Int("keep", -1, "keep the newest N versions per dist and architecture, remove the rest")
	arch := fs.String("arch", "", "only this architecture")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	noPublish := fs.Bool("no-publish", false, "remove from the repos but do not republish")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 3 {
		fs.Usage()
		return ErrUsage
	}
	alias, comp, spec := pos[0], pos[1], pos[2]
	pkgName, version, hasVersion := strings.Cut(spec, "=")
	if hasVersion && *keep >= 0 {
		return fmt.Errorf("use either <package>=<version> or --keep, not both: %w", ErrUsage)
	}
	if hasVersion && version == "" {
		return fmt.Errorf("empty version in %q: %w", spec, ErrUsage)
	}
	cfg, err := e.load()
	if err != nil {
		return err
	}
	pubs, err := cfg.Select([]string{alias})
	if err != nil {
		return err
	}
	p := pubs[0]
	if !slices.Contains(p.Components, comp) {
		return fmt.Errorf("component %q is not in publish.%s", comp, alias)
	}
	dists, err := targetDists(p, *distFlag)
	if err != nil {
		return err
	}
	c, err := e.client(cfg, false)
	if err != nil {
		return err
	}
	rows, err := e.collect(c, p, []string{comp}, dists)
	if err != nil {
		return err
	}

	var victims []pkgRow
	seen := map[string]int{} // repo/arch -> versions kept so far (rows are newest first)
	for _, r := range rows {
		if r.Package.Package != pkgName || (*arch != "" && r.Architecture != *arch) {
			continue
		}
		if hasVersion && r.Version != version {
			continue
		}
		if *keep >= 0 {
			k := r.Repo + "/" + r.Architecture
			if seen[k] < *keep {
				seen[k]++
				continue
			}
		}
		victims = append(victims, r)
	}
	if len(victims) == 0 {
		fmt.Fprintln(e.stdout, "Nothing to remove.")
		return nil
	}
	tw := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "- DIST\tCOMPONENT\tPACKAGE\tVERSION\tARCH")
	for _, r := range victims {
		fmt.Fprintf(tw, "- %s\t%s\t%s\t%s\t%s\n", r.Dist, r.Component, r.Package.Package, r.Version, r.Architecture)
	}
	tw.Flush()
	if err := e.confirm(*yes, fmt.Sprintf("remove %d package(s) from %s", len(victims), cfg.Aptly.URL)); err != nil {
		return err
	}
	byRepo := map[string][]string{}
	var touched []string
	for _, r := range victims {
		byRepo[r.Repo] = append(byRepo[r.Repo], r.Key)
		if !slices.Contains(touched, r.Dist) {
			touched = append(touched, r.Dist)
		}
	}
	for _, repo := range sortedKeys(byRepo) {
		if err := c.DeletePackages(e.ctx, repo, byRepo[repo]); err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "removed %d from %s\n", len(byRepo[repo]), repo)
	}
	if *noPublish {
		return nil
	}
	st, err := plan.Load(e.ctx, c, nil)
	if err != nil {
		return err
	}
	var publishable []string
	for _, d := range touched {
		if _, ok := st.Published(p, d); ok {
			publishable = append(publishable, d)
		}
	}
	return e.republish(c, cfg, p, publishable, false)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
