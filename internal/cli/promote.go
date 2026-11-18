// SPDX-License-Identifier: GPL-3.0-only

package cli

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/shinys000114/aptrix/internal/config"
)

// parseCellRef parses "<alias>/<component>".
func (e *env) parseCellRef(cfg *config.Config, ref string) (*config.Publish, string, error) {
	alias, comp, ok := strings.Cut(ref, "/")
	if !ok || alias == "" || comp == "" {
		return nil, "", fmt.Errorf("%q: expected <alias>/<component>: %w", ref, ErrUsage)
	}
	pubs, err := cfg.Select([]string{alias})
	if err != nil {
		return nil, "", err
	}
	if !slices.Contains(pubs[0].Components, comp) {
		return nil, "", fmt.Errorf("component %q is not in publish.%s", comp, alias)
	}
	return pubs[0], comp, nil
}

// cmdPromote copies (or moves) packages between cells without re-uploading.
func (e *env) cmdPromote(args []string) error {
	fs := newFlags(e, "promote", "<src-alias>/<component> <dst-alias>/<component> <glob>[=version]... [--dist d1,d2|all] [--to-dist d] [--yes]")
	distFlag := fs.String("dist", "all", `source dists (comma separated or "all")`)
	toDist := fs.String("to-dist", "", "copy into this dist instead of the same one (needs a single --dist)")
	version := fs.String("version", "", `version conditions, e.g. ">=1.2" (dpkg ordering)`)
	arch := fs.String("arch", "", "only this architecture")
	allVersions := fs.Bool("all-versions", false, "copy every matching version (default: only the newest per package and arch)")
	move := fs.Bool("move", false, "remove the packages from the source after copying")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	noSnap := fs.Bool("no-snapshot", false, "do not snapshot the affected repos first")
	noPublish := fs.Bool("no-publish", false, "do not republish")
	pos, err := parse(fs, args)
	if err != nil || len(pos) < 3 {
		fs.Usage()
		return ErrUsage
	}
	cfg, err := e.load()
	if err != nil {
		return err
	}
	src, srcComp, err := e.parseCellRef(cfg, pos[0])
	if err != nil {
		return err
	}
	dst, dstComp, err := e.parseCellRef(cfg, pos[1])
	if err != nil {
		return err
	}
	type spec struct{ pat, ver string }
	var sp []spec
	exactVersion := false
	for _, a := range pos[2:] {
		n, v, hasV := strings.Cut(a, "=")
		if hasV && v == "" {
			return fmt.Errorf("empty version in %q: %w", a, ErrUsage)
		}
		if _, err := path.Match(n, ""); err != nil {
			return fmt.Errorf("bad package pattern %q: %v: %w", n, err, ErrUsage)
		}
		sp = append(sp, spec{n, v})
		exactVersion = exactVersion || hasV
	}
	m, err := newMatcher(nil, *version, *arch)
	if err != nil {
		return err
	}
	srcDists, err := targetDists(src, *distFlag)
	if err != nil {
		return err
	}
	if *toDist != "" {
		if len(srcDists) != 1 {
			return fmt.Errorf("--to-dist needs exactly one source --dist: %w", ErrUsage)
		}
		if !slices.Contains(dst.Dists, *toDist) {
			return fmt.Errorf("dist %q is not in publish.%s", *toDist, dst.Alias)
		}
	}
	mapDist := func(d string) string {
		if *toDist != "" {
			return *toDist
		}
		return d
	}
	// Only dists that exist on both sides.
	var dists []string
	for _, d := range srcDists {
		if src.HasDist(srcComp, d) && dst.HasDist(dstComp, mapDist(d)) {
			dists = append(dists, d)
		}
	}
	if len(dists) == 0 {
		return fmt.Errorf("no dist has both %s and %s", pos[0], pos[1])
	}
	if src == dst && srcComp == dstComp && *toDist == "" {
		return fmt.Errorf("source and destination are the same cells: %w", ErrUsage)
	}

	c, err := e.client(cfg, false)
	if err != nil {
		return err
	}
	rows, err := e.collect(c, src, []string{srcComp}, dists)
	if err != nil {
		return err
	}
	var picked []pkgRow
	for _, r := range filterRows(rows, m) {
		for _, s := range sp {
			if hit, _ := path.Match(s.pat, r.Package.Package); hit && (s.ver == "" || s.ver == r.Version) {
				picked = append(picked, r)
				break
			}
		}
	}
	if !*allVersions && !exactVersion && *version == "" {
		picked = newestOnly(picked)
	}

	type item struct {
		row     pkgRow
		dstRepo string
		dstDist string
	}
	var items []item
	var skipped int
	dstKeys := map[string]map[string]bool{}
	for _, r := range picked {
		dd := mapDist(r.Dist)
		repo := dst.RepoFor(dd, dstComp)
		if repo == r.Repo {
			// copying a repo onto itself is a no-op, and --move would delete
			return fmt.Errorf("%s/%s: destination is the source repo %s: %w", r.Dist, r.Component, repo, ErrUsage)
		}
		if r.Architecture != "all" && !slices.Contains(dst.Architectures, r.Architecture) {
			return fmt.Errorf("%s %s: architecture %s is not published by %s", r.Package.Package, r.Version, r.Architecture, dst.Alias)
		}
		if dstKeys[repo] == nil {
			keys, err := c.PackageKeys(e.ctx, repo)
			if err != nil {
				return fmt.Errorf("destination repo %s: %w (run 'aptrix sync %s'?)", repo, err, dst.Alias)
			}
			dstKeys[repo] = map[string]bool{}
			for _, k := range keys {
				dstKeys[repo][k] = true
			}
		}
		if dstKeys[repo][r.Key] && !*move {
			skipped++
			continue
		}
		items = append(items, item{r, repo, dd})
	}
	if len(items) == 0 {
		fmt.Fprintf(e.stdout, "Nothing to promote (%d already present).\n", skipped)
		return nil
	}
	verb := "copy"
	if *move {
		verb = "move"
	}
	tw := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PACKAGE\tVERSION\tARCH\tFROM\tTO")
	for _, it := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s/%s/%s\t%s/%s/%s\n", it.row.Package.Package, it.row.Version, it.row.Architecture,
			src.Alias, it.row.Component, it.row.Dist, dst.Alias, dstComp, it.dstDist)
	}
	tw.Flush()
	if skipped > 0 {
		fmt.Fprintf(e.stdout, "(%d already present in the destination, skipped)\n", skipped)
	}
	if err := e.confirm(*yes, fmt.Sprintf("%s %d package(s) on %s", verb, len(items), cfg.Aptly.URL)); err != nil {
		return err
	}

	dstTouched := map[string]config.Cell{}
	srcTouched := map[string]config.Cell{}
	addBy := map[string][]string{}
	delBy := map[string][]string{}
	var names []string
	for _, it := range items {
		dstTouched[it.dstRepo] = config.Cell{Dist: it.dstDist, Component: dstComp, Repo: it.dstRepo}
		if !dstKeys[it.dstRepo][it.row.Key] {
			addBy[it.dstRepo] = append(addBy[it.dstRepo], it.row.Key)
		}
		if *move {
			srcTouched[it.row.Repo] = config.Cell{Dist: it.row.Dist, Component: it.row.Component, Repo: it.row.Repo}
			delBy[it.row.Repo] = append(delBy[it.row.Repo], it.row.Key)
		}
		if !slices.Contains(names, it.row.Package.Package) {
			names = append(names, it.row.Package.Package)
		}
	}
	if !*noSnap {
		var repos []string
		repos = append(repos, sortedKeys(dstTouched)...)
		repos = append(repos, sortedKeys(srcTouched)...)
		// one label covers both sides when they belong to the same publish
		if src == dst {
			if _, err := e.preSnapshot(c, dst, repos, "pre-promote", "aptrix: before promote "+strings.Join(pos[2:], " ")); err != nil {
				return err
			}
		} else {
			if _, err := e.preSnapshot(c, dst, sortedKeys(dstTouched), "pre-promote", "aptrix: before promote "+strings.Join(pos[2:], " ")); err != nil {
				return err
			}
			if *move {
				if _, err := e.preSnapshot(c, src, sortedKeys(srcTouched), "pre-promote", "aptrix: before promote (move) "+strings.Join(pos[2:], " ")); err != nil {
					return err
				}
			}
		}
	}
	for _, repo := range sortedKeys(addBy) {
		if err := c.AddPackages(e.ctx, repo, addBy[repo]); err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "added %d to %s\n", len(addBy[repo]), repo)
	}
	for _, repo := range sortedKeys(delBy) {
		if err := c.DeletePackages(e.ctx, repo, delBy[repo]); err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "removed %d from %s (moved)\n", len(delBy[repo]), repo)
	}
	if src == dst {
		for k, v := range srcTouched {
			dstTouched[k] = v
		}
		return e.finish(c, cfg, dst, dstTouched, names, false, *noPublish)
	}
	if err := e.finish(c, cfg, dst, dstTouched, names, false, *noPublish); err != nil {
		return err
	}
	if *move {
		return e.finish(c, cfg, src, srcTouched, nil, false, *noPublish)
	}
	return nil
}

// cmdFind searches every configured publish.
func (e *env) cmdFind(args []string) error {
	fs := newFlags(e, "find", "<glob>... [--alias a,b] [--component c] [--dist d1,d2] [--version cond] [--arch a] [--json]")
	aliasFlag := fs.String("alias", "", "only these publishes (comma separated)")
	comp := fs.String("component", "", "only this component")
	distFlag := fs.String("dist", "", "only these dists (comma separated)")
	version := fs.String("version", "", `version conditions, e.g. ">=1.2"`)
	arch := fs.String("arch", "", "only this architecture")
	newest := fs.Bool("newest", false, "only the newest version per cell, package and arch")
	asJSON := fs.Bool("json", false, "JSON output")
	pos, err := parse(fs, args)
	if err != nil || len(pos) == 0 {
		fs.Usage()
		return ErrUsage
	}
	m, err := newMatcher(pos, *version, *arch)
	if err != nil {
		return err
	}
	cfg, err := e.load()
	if err != nil {
		return err
	}
	pubs, err := cfg.Select(splitList(*aliasFlag))
	if err != nil {
		return err
	}
	c, err := e.client(cfg, true)
	if err != nil {
		return err
	}
	want := splitList(*distFlag)
	var all []pkgRow
	var distOrder []string
	for _, p := range pubs {
		comps := p.Components
		if *comp != "" {
			if !slices.Contains(p.Components, *comp) {
				continue
			}
			comps = []string{*comp}
		}
		var dists []string
		for _, d := range p.Dists {
			if len(want) == 0 || slices.Contains(want, d) {
				dists = append(dists, d)
			}
			if !slices.Contains(distOrder, d) {
				distOrder = append(distOrder, d)
			}
		}
		rows, err := e.collect(c, p, comps, dists)
		if err != nil {
			return err
		}
		all = append(all, filterRows(rows, m)...)
	}
	if *newest {
		all = newestOnly(all)
	}
	sortRows(all, distOrder)
	return e.printRows(all, true, *asJSON)
}
