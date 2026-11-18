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
		fmt.Fprint(e.stderr, "Usage: aptrix snapshot create|list|diff|drop|prune ...\n")
		return ErrUsage
	}
	switch args[0] {
	case "create":
		return e.snapshotCreate(args[1:])
	case "list", "ls":
		return e.snapshotList(args[1:])
	case "drop", "rm":
		return e.snapshotDrop(args[1:])
	case "prune":
		return e.snapshotPrune(args[1:])
	case "diff":
		return e.snapshotDiff(args[1:])
	}
	fmt.Fprintf(e.stderr, "unknown snapshot command %q (create, list, drop, prune, diff)\n", args[0])
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
	fs := newFlags(e, "snapshot list", "<alias> [--json]")
	asJSON := fs.Bool("json", false, "JSON output")
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
	if *asJSON {
		type item struct {
			Label       string    `json:"label"`
			Created     time.Time `json:"created"`
			Description string    `json:"description"`
			Repos       []string  `json:"repos"`
		}
		out := []item{}
		for _, l := range ls {
			out = append(out, item{l.Label, l.Created, l.Desc, sortedKeys(l.Snaps)})
		}
		return e.json(out)
	}
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
	touched := map[string]config.Cell{}
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
		touched[ch.repo] = ch.cell
	}
	// A rolled-back pool file may differ from what is published now.
	return e.finish(c, cfg, p, touched, nil, true, *noPublish)
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

// autoPrefixes are the labels aptrix creates on its own.
var autoPrefixes = []string{"pre-add-", "pre-rm-", "pre-rollback-", "pre-promote-", "pre-prune-"}

func isAutoLabel(l string) bool {
	for _, p := range autoPrefixes {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// expireSnapshots drops automatic snapshots of p older than snapshot_max_age.
func (e *env) expireSnapshots(c *aptly.Client, p *config.Publish) error {
	if p.SnapshotMaxAge == "" {
		return nil
	}
	age, err := config.ParseAge(p.SnapshotMaxAge)
	if err != nil {
		return err
	}
	snaps, err := c.Snapshots(e.ctx)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-age)
	for _, l := range labels(snaps, p) {
		if !isAutoLabel(l.Label) || !l.Created.Before(cutoff) {
			continue
		}
		for _, repo := range sortedKeys(l.Snaps) {
			// another run (e.g. a parallel Jenkins job) may have dropped it
			if err := c.DropSnapshot(e.ctx, l.Snaps[repo]); err != nil && !aptly.IsNotFound(err) {
				return fmt.Errorf("expiring snapshot %s: %w", l.Snaps[repo], err)
			}
		}
		fmt.Fprintf(e.stdout, "expired snapshot %s (older than %s)\n", l.Label, p.SnapshotMaxAge)
	}
	return nil
}

func (e *env) snapshotPrune(args []string) error {
	fs := newFlags(e, "snapshot prune", "<alias> [--older-than 30d] [--keep-last N] [--include-manual] [--yes]")
	older := fs.String("older-than", "", `drop labels older than this (e.g. "30d", "12h")`)
	keepLast := fs.Int("keep-last", -1, "always keep the newest N labels")
	manual := fs.Bool("include-manual", false, "also prune labels not created automatically (e.g. manual-*)")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 || (*older == "" && *keepLast < 0) {
		fs.Usage()
		return ErrUsage
	}
	cfg, p, _, err := e.selectCells(pos[0], "", "")
	if err != nil {
		return err
	}
	var cutoff time.Time
	if *older != "" {
		age, err := config.ParseAge(*older)
		if err != nil {
			return err
		}
		cutoff = time.Now().Add(-age)
	}
	c, err := e.client(cfg, false)
	if err != nil {
		return err
	}
	snaps, err := c.Snapshots(e.ctx)
	if err != nil {
		return err
	}
	var victims []*labelInfo
	n := 0
	for _, l := range labels(snaps, p) { // newest first
		if !*manual && !isAutoLabel(l.Label) {
			continue
		}
		n++
		if *keepLast >= 0 && n <= *keepLast {
			continue
		}
		if !cutoff.IsZero() && !l.Created.Before(cutoff) {
			continue
		}
		victims = append(victims, l)
	}
	if len(victims) == 0 {
		fmt.Fprintln(e.stdout, "Nothing to prune.")
		return nil
	}
	for _, l := range victims {
		fmt.Fprintf(e.stdout, "- %s  %s  (%d repo(s))\n", l.Label, l.Created.Local().Format("2006-01-02 15:04:05"), len(l.Snaps))
	}
	if err := e.confirm(*yes, fmt.Sprintf("drop %d snapshot label(s)", len(victims))); err != nil {
		return err
	}
	for _, l := range victims {
		for _, repo := range sortedKeys(l.Snaps) {
			if err := c.DropSnapshot(e.ctx, l.Snaps[repo]); err != nil {
				return err
			}
		}
	}
	fmt.Fprintf(e.stdout, "dropped %d label(s); run 'aptrix gc' to free pool space\n", len(victims))
	return nil
}

// snapshotDiff shows what changed between a label and now, or between two labels.
func (e *env) snapshotDiff(args []string) error {
	fs := newFlags(e, "snapshot diff", "<alias> <label> [<label2>] [--component c] [--dist d1,d2] [--json]")
	comp := fs.String("component", "", "only this component")
	distFlag := fs.String("dist", "", "only these dists (comma separated)")
	asJSON := fs.Bool("json", false, "JSON output")
	pos, err := parse(fs, args)
	if err != nil || len(pos) < 2 || len(pos) > 3 {
		fs.Usage()
		return ErrUsage
	}
	cfg, p, cells, err := e.selectCells(pos[0], *comp, *distFlag)
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
	find := func(label string) (*labelInfo, error) {
		for _, l := range labels(snaps, p) {
			if l.Label == label {
				return l, nil
			}
		}
		return nil, fmt.Errorf("no snapshot labelled %q for publish.%s", label, p.Alias)
	}
	from, err := find(pos[1])
	if err != nil {
		return err
	}
	var to *labelInfo
	toName := "now"
	if len(pos) == 3 {
		if to, err = find(pos[2]); err != nil {
			return err
		}
		toName = to.Label
	}
	type entry struct {
		Dist      string `json:"dist"`
		Component string `json:"component"`
		Change    string `json:"change"` // "+" added since, "-" removed since
		Package   string `json:"package"`
		Version   string `json:"version"`
		Arch      string `json:"architecture"`
	}
	var out []entry
	for _, repo := range sortedKeys(cells) {
		cell := cells[repo]
		fromName, ok := from.Snaps[repo]
		if !ok {
			continue
		}
		before, err := c.SnapshotPackageKeys(e.ctx, fromName)
		if err != nil {
			return err
		}
		var after []string
		if to == nil {
			after, err = c.PackageKeys(e.ctx, repo)
		} else if n, ok := to.Snaps[repo]; ok {
			after, err = c.SnapshotPackageKeys(e.ctx, n)
		} else {
			continue
		}
		if err != nil {
			return err
		}
		added, removed := diffKeys(before, after)
		for _, k := range removed {
			ki := parseKey(k)
			out = append(out, entry{cell.Dist, cell.Component, "-", ki.Name, ki.Version, ki.Arch})
		}
		for _, k := range added {
			ki := parseKey(k)
			out = append(out, entry{cell.Dist, cell.Component, "+", ki.Name, ki.Version, ki.Arch})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Dist != b.Dist {
			return slices.Index(p.Dists, a.Dist) < slices.Index(p.Dists, b.Dist)
		}
		if a.Component != b.Component {
			return a.Component < b.Component
		}
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		return a.Version > b.Version
	})
	if *asJSON {
		if out == nil {
			out = []entry{}
		}
		return e.json(out)
	}
	fmt.Fprintf(e.stdout, "%s -> %s\n", from.Label, toName)
	if len(out) == 0 {
		fmt.Fprintln(e.stdout, "No differences.")
		return nil
	}
	tw := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)
	for _, x := range out {
		fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\t%s\n", x.Change, x.Dist, x.Component, x.Package, x.Version, x.Arch)
	}
	tw.Flush()
	return nil
}
