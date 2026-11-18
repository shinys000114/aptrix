// SPDX-License-Identifier: GPL-3.0-only

package cli

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/shinys000114/aptrix/internal/aptly"
	"github.com/shinys000114/aptrix/internal/config"
	"github.com/shinys000114/aptrix/internal/plan"
)

// Snapshots are named "<repo>@<label>"; one label groups the snapshots of
// several repos taken together.
const snapSep = "@"

var labelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func snapName(repo, label string) string { return repo + snapSep + label }

func newLabel(prefix string) string {
	return prefix + "-" + time.Now().Format("20060102-150405.000")
}

// cellRepos returns repo -> cell for the selected cells of p.
func cellRepos(p *config.Publish, comps, dists []string) map[string]config.Cell {
	out := map[string]config.Cell{}
	for _, cell := range p.Cells() {
		if (len(comps) == 0 || slices.Contains(comps, cell.Component)) && (len(dists) == 0 || slices.Contains(dists, cell.Dist)) {
			out[cell.Repo] = cell
		}
	}
	return out
}

// takeSnapshots snapshots repos under one label.
func (e *env) takeSnapshots(c *aptly.Client, repos []string, label, desc string) error {
	for _, r := range repos {
		if err := c.SnapshotRepo(e.ctx, r, snapName(r, label), desc); err != nil {
			return fmt.Errorf("snapshot of %s: %w", r, err)
		}
	}
	return nil
}

func (e *env) cmdSnapshot(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, "Usage: aptrix snapshot create|list|drop ...\n")
		return ErrUsage
	}
	switch args[0] {
	case "create":
		return e.snapshotCreate(args[1:])
	case "list", "ls":
		return e.snapshotList(args[1:])
	case "drop", "rm":
		return e.snapshotDrop(args[1:])
	}
	fmt.Fprintf(e.stderr, "unknown snapshot command %q (create, list, drop)\n", args[0])
	return ErrUsage
}

func (e *env) selectCells(alias string, comp string, distFlag string) (*config.Config, *config.Publish, map[string]config.Cell, error) {
	cfg, err := e.load()
	if err != nil {
		return nil, nil, nil, err
	}
	pubs, err := cfg.Select([]string{alias})
	if err != nil {
		return nil, nil, nil, err
	}
	p := pubs[0]
	var comps []string
	if comp != "" {
		if !slices.Contains(p.Components, comp) {
			return nil, nil, nil, fmt.Errorf("component %q is not in publish.%s", comp, alias)
		}
		comps = []string{comp}
	}
	var dists []string
	if distFlag != "" {
		if dists, err = targetDists(p, distFlag); err != nil {
			return nil, nil, nil, err
		}
	}
	return cfg, p, cellRepos(p, comps, dists), nil
}

func (e *env) snapshotCreate(args []string) error {
	fs := newFlags(e, "snapshot create", "<alias> [component] [--dist d1,d2] [--label L]")
	distFlag := fs.String("dist", "", "only these dists (comma separated)")
	label := fs.String("label", "", "snapshot label (default: manual-<timestamp>)")
	desc := fs.String("m", "", "description")
	pos, err := parse(fs, args)
	if err != nil || len(pos) < 1 || len(pos) > 2 {
		fs.Usage()
		return ErrUsage
	}
	comp := ""
	if len(pos) == 2 {
		comp = pos[1]
	}
	cfg, _, cells, err := e.selectCells(pos[0], comp, *distFlag)
	if err != nil {
		return err
	}
	if *label == "" {
		*label = newLabel("manual")
	}
	if !labelRe.MatchString(*label) {
		return fmt.Errorf("invalid label %q", *label)
	}
	c, err := e.client(cfg, false)
	if err != nil {
		return err
	}
	st, err := plan.Load(e.ctx, c, nil)
	if err != nil {
		return err
	}
	var repos []string
	for _, r := range sortedKeys(cells) {
		if _, ok := st.Repos[r]; ok {
			repos = append(repos, r)
		}
	}
	if len(repos) == 0 {
		return fmt.Errorf("no existing repos selected")
	}
	if *desc == "" {
		*desc = "aptrix snapshot create"
	}
	if err := e.takeSnapshots(c, repos, *label, *desc); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "snapshot %s: %d repo(s)\n", *label, len(repos))
	return nil
}

type labelInfo struct {
	Label   string
	Created time.Time
	Desc    string
	Snaps   map[string]string // repo -> snapshot name
}

// labels groups the snapshots of p's repos by label, newest first.
func labels(snaps []aptly.Snapshot, p *config.Publish) []*labelInfo {
	repos := cellRepos(p, nil, nil)
	byLabel := map[string]*labelInfo{}
	for _, s := range snaps {
		i := strings.LastIndex(s.Name, snapSep)
		if i <= 0 {
			continue
		}
		repo, label := s.Name[:i], s.Name[i+1:]
		if _, ok := repos[repo]; !ok {
			continue
		}
		li := byLabel[label]
		if li == nil {
			li = &labelInfo{Label: label, Created: s.CreatedAt, Desc: s.Description, Snaps: map[string]string{}}
			byLabel[label] = li
		}
		if s.CreatedAt.Before(li.Created) {
			li.Created = s.CreatedAt
		}
		li.Snaps[repo] = s.Name
	}
	out := make([]*labelInfo, 0, len(byLabel))
	for _, li := range byLabel {
		out = append(out, li)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

func (e *env) snapshotList(args []string) error {
	fs := newFlags(e, "snapshot list", "<alias>")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		fs.Usage()
		return ErrUsage
	}
	cfg, p, _, err := e.selectCells(pos[0], "", "")
	if err != nil {
		return err
	}
	c, err := e.client(cfg, true)
	if err != nil {
		return err
	}
	snaps, err := c.Snapshots(e.ctx)
	if err != nil {
		return err
	}
	ls := labels(snaps, p)
	tw := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LABEL\tCREATED\tREPOS\tDESCRIPTION")
	for _, l := range ls {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", l.Label, l.Created.Local().Format("2006-01-02 15:04:05"), len(l.Snaps), l.Desc)
	}
	tw.Flush()
	fmt.Fprintf(e.stdout, "%d snapshot label(s)\n", len(ls))
	return nil
}

func (e *env) snapshotDrop(args []string) error {
	fs := newFlags(e, "snapshot drop", "<alias> <label> [--yes]")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 2 {
		fs.Usage()
		return ErrUsage
	}
	cfg, p, _, err := e.selectCells(pos[0], "", "")
	if err != nil {
		return err
	}
	c, err := e.client(cfg, false)
	if err != nil {
		return err
	}
	snaps, err := c.Snapshots(e.ctx)
	if err != nil {
		return err
	}
	var li *labelInfo
	for _, l := range labels(snaps, p) {
		if l.Label == pos[1] {
			li = l
		}
	}
	if li == nil {
		return fmt.Errorf("no snapshot labelled %q for publish.%s", pos[1], p.Alias)
	}
	if err := e.confirm(*yes, fmt.Sprintf("drop %d snapshot(s) labelled %s", len(li.Snaps), li.Label)); err != nil {
		return err
	}
	for _, repo := range sortedKeys(li.Snaps) {
		if err := c.DropSnapshot(e.ctx, li.Snaps[repo]); err != nil {
			return err
		}
	}
	fmt.Fprintf(e.stdout, "dropped %d snapshot(s) labelled %s\n", len(li.Snaps), li.Label)
	return nil
}

func (e *env) cmdRollback(args []string) error {
	fs := newFlags(e, "rollback", "<alias> <label> [component] [--dist d1,d2] [--yes]")
	distFlag := fs.String("dist", "", "only these dists (comma separated)")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	noSnap := fs.Bool("no-snapshot", false, "do not snapshot the current state before rolling back")
	noPublish := fs.Bool("no-publish", false, "restore the repos but do not republish")
	pos, err := parse(fs, args)
	if err != nil || len(pos) < 2 || len(pos) > 3 {
		fs.Usage()
		return ErrUsage
	}
	comp := ""
	if len(pos) == 3 {
		comp = pos[2]
	}
	cfg, p, cells, err := e.selectCells(pos[0], comp, *distFlag)
	if err != nil {
		return err
	}
	c, err := e.client(cfg, false)
	if err != nil {
		return err
	}
	snaps, err := c.Snapshots(e.ctx)
	if err != nil {
		return err
	}
	var li *labelInfo
	for _, l := range labels(snaps, p) {
		if l.Label == pos[1] {
			li = l
		}
	}
	if li == nil {
		return fmt.Errorf("no snapshot labelled %q for publish.%s (see 'aptrix snapshot list %s')", pos[1], p.Alias, p.Alias)
	}

	type change struct {
		repo     string
		cell     config.Cell
		add, del []string
	}
	var changes []change
	for _, repo := range sortedKeys(li.Snaps) {
		cell, ok := cells[repo]
		if !ok {
			continue // filtered out by component/--dist
		}
		want, err := c.SnapshotPackageKeys(e.ctx, li.Snaps[repo])
		if err != nil {
			return err
		}
		have, err := c.PackageKeys(e.ctx, repo)
		if err != nil {
			return err
		}
		add, del := diffKeys(have, want)
		if len(add)+len(del) > 0 {
			changes = append(changes, change{repo: repo, cell: cell, add: add, del: del})
		}
	}
	if len(changes) == 0 {
		fmt.Fprintf(e.stdout, "Already at %s.\n", li.Label)
		return nil
	}
	tw := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DIST\tCOMPONENT\tRESTORE\tREMOVE")
	for _, ch := range changes {
		fmt.Fprintf(tw, "%s\t%s\t+%d\t-%d\n", ch.cell.Dist, ch.cell.Component, len(ch.add), len(ch.del))
	}
	tw.Flush()
	for _, ch := range changes {
		for _, k := range ch.add {
			fmt.Fprintf(e.stdout, "  + %s/%s %s\n", ch.cell.Dist, ch.cell.Component, k)
		}
		for _, k := range ch.del {
			fmt.Fprintf(e.stdout, "  - %s/%s %s\n", ch.cell.Dist, ch.cell.Component, k)
		}
	}
	if err := e.confirm(*yes, fmt.Sprintf("roll %d repo(s) back to %s on %s", len(changes), li.Label, cfg.Aptly.URL)); err != nil {
		return err
	}
	if !*noSnap {
		safety := newLabel("pre-rollback")
		repos := make([]string, len(changes))
		for i, ch := range changes {
			repos[i] = ch.repo
		}
		if err := e.takeSnapshots(c, repos, safety, "aptrix: before rollback to "+li.Label); err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "snapshot %s taken; undo with 'aptrix rollback %s %s'\n", safety, p.Alias, safety)
	}
	var dists []string
	for _, ch := range changes {
		// Remove first: re-adding a package that conflicts with a current one
		// (same name/version/arch, different content) would fail.
		if len(ch.del) > 0 {
			if err := c.DeletePackages(e.ctx, ch.repo, ch.del); err != nil {
				return err
			}
		}
		if len(ch.add) > 0 {
			if err := c.AddPackages(e.ctx, ch.repo, ch.add); err != nil {
				return err
			}
		}
		fmt.Fprintf(e.stdout, "restored %s (+%d -%d)\n", ch.repo, len(ch.add), len(ch.del))
		if !slices.Contains(dists, ch.cell.Dist) {
			dists = append(dists, ch.cell.Dist)
		}
	}
	if *noPublish {
		return nil
	}
	st, err := plan.Load(e.ctx, c, nil)
	if err != nil {
		return err
	}
	var publishable []string
	for _, d := range p.Dists { // keep config order
		if slices.Contains(dists, d) {
			if _, ok := st.Published(p, d); ok {
				publishable = append(publishable, d)
			}
		}
	}
	// A rolled-back pool file may differ from what is published now.
	return e.republish(c, cfg, p, publishable, true)
}

// diffKeys returns keys to add (in want, not have) and delete (in have, not want).
func diffKeys(have, want []string) (add, del []string) {
	h := map[string]bool{}
	for _, k := range have {
		h[k] = true
	}
	w := map[string]bool{}
	for _, k := range want {
		w[k] = true
		if !h[k] {
			add = append(add, k)
		}
	}
	for _, k := range have {
		if !w[k] {
			del = append(del, k)
		}
	}
	sort.Strings(add)
	sort.Strings(del)
	return add, del
}
