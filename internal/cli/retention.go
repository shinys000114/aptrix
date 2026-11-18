// SPDX-License-Identifier: GPL-3.0-only

package cli

import (
	"fmt"
	"slices"
	"text/tabwriter"

	"github.com/shinys000114/aptrix/internal/config"
)

// cmdPruneVersions keeps the newest N versions of every package in the
// selected cells.
func (e *env) cmdPruneVersions(args []string) error {
	fs := newFlags(e, "prune-versions", "<alias> [component] [--dist d1,d2] [--keep N] [--name glob] [--yes]")
	distFlag := fs.String("dist", "", "only these dists (comma separated)")
	keep := fs.Int("keep", 0, "versions to keep per package, dist, component and arch (default: the publish's keep)")
	name := fs.String("name", "", "only packages matching this glob")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	noSnap := fs.Bool("no-snapshot", false, "do not snapshot the affected repos first")
	noPublish := fs.Bool("no-publish", false, "do not republish")
	pos, err := parse(fs, args)
	if err != nil || len(pos) < 1 || len(pos) > 2 {
		fs.Usage()
		return ErrUsage
	}
	comp := ""
	if len(pos) == 2 {
		comp = pos[1]
	}
	cfg, p, cells, err := e.selectCells(pos[0], comp, *distFlag)
	if err != nil {
		return err
	}
	n := *keep
	if n == 0 {
		n = p.Keep
	}
	if n <= 0 {
		return fmt.Errorf("no keep count: pass --keep N or set keep in [publish.%s]", p.Alias)
	}
	var names []string
	if *name != "" {
		names = []string{*name}
	}
	m, err := newMatcher(names, "", "")
	if err != nil {
		return err
	}
	c, err := e.client(cfg, false)
	if err != nil {
		return err
	}
	var comps, dists []string
	for _, cell := range cells {
		if !slices.Contains(comps, cell.Component) {
			comps = append(comps, cell.Component)
		}
		if !slices.Contains(dists, cell.Dist) {
			dists = append(dists, cell.Dist)
		}
	}
	rows, err := e.collect(c, p, comps, dists)
	if err != nil {
		return err
	}
	var in []pkgRow
	for _, r := range filterRows(rows, m) {
		if _, ok := cells[r.Repo]; ok {
			in = append(in, r)
		}
	}
	victims := beyondKeep(in, n)
	sortRows(victims, p.Dists)
	if len(victims) == 0 {
		fmt.Fprintf(e.stdout, "Nothing to prune (keep %d).\n", n)
		return nil
	}
	tw := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "- DIST\tCOMPONENT\tPACKAGE\tVERSION\tARCH")
	for _, r := range victims {
		fmt.Fprintf(tw, "- %s\t%s\t%s\t%s\t%s\n", r.Dist, r.Component, r.Package.Package, r.Version, r.Architecture)
	}
	tw.Flush()
	if err := e.confirm(*yes, fmt.Sprintf("remove %d old version(s) (keep %d) from %s", len(victims), n, cfg.Aptly.URL)); err != nil {
		return err
	}
	touched := map[string]config.Cell{}
	for _, r := range victims {
		touched[r.Repo] = cells[r.Repo]
	}
	if !*noSnap {
		if _, err := e.preSnapshot(c, p, sortedKeys(touched), "pre-prune", fmt.Sprintf("aptrix: before prune-versions keep %d", n)); err != nil {
			return err
		}
	}
	if err := e.deleteRows(c, victims, fmt.Sprintf("keep %d", n)); err != nil {
		return err
	}
	return e.finish(c, cfg, p, touched, nil, false, *noPublish)
}

// cmdGC runs aptly's db cleanup to free pool space.
func (e *env) cmdGC(args []string) error {
	fs := newFlags(e, "gc", "[--yes]")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 0 {
		fs.Usage()
		return ErrUsage
	}
	cfg, err := e.load()
	if err != nil {
		return err
	}
	c, err := e.client(cfg, false)
	if err != nil {
		return err
	}
	if err := e.confirm(*yes, fmt.Sprintf("run aptly db cleanup on %s (deletes every package and pool file no repo or snapshot references, server-wide)", cfg.Aptly.URL)); err != nil {
		return err
	}
	fmt.Fprintln(e.stdout, "running aptly db cleanup...")
	if err := c.DBCleanup(e.ctx); err != nil {
		return err
	}
	fmt.Fprintln(e.stdout, "done (details are in the aptly server log)")
	return nil
}
