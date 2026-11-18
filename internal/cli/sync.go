// SPDX-License-Identifier: GPL-3.0-only

package cli

import (
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/shinys000114/aptrix/internal/aptly"
	"github.com/shinys000114/aptrix/internal/config"
	"github.com/shinys000114/aptrix/internal/plan"
)

func (e *env) cmdSync(args []string, dryRun bool) error {
	name := "sync"
	if dryRun {
		name = "plan"
	}
	fs := newFlags(e, name, "[alias...]")
	prune := fs.Bool("prune", false, "remove components, dists and empty repos that are not in the config")
	force := fs.Bool("force", false, "with --prune, also delete repos that still contain packages")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	discard := fs.Bool("discard-staged", false, "drop source changes left staged by another run")
	asJSON := fs.Bool("json", false, "JSON output (plan only)")
	aliases, err := parse(fs, args)
	if err != nil {
		return ErrUsage
	}
	cfg, err := e.load()
	if err != nil {
		return err
	}
	pubs, err := cfg.Select(aliases)
	if err != nil {
		return err
	}
	// Planning is always read-only; a separate client applies.
	ro, err := e.client(cfg, true)
	if err != nil {
		return err
	}
	st, err := plan.Load(e.ctx, ro, pubs)
	if err != nil {
		return err
	}
	pl := plan.Make(pubs, st, plan.Options{Prune: *prune, Force: *force})
	if *asJSON && dryRun {
		return e.planJSON(pl)
	}
	e.printPlan(pl)
	if dryRun || len(pl.Steps) == 0 {
		return nil
	}
	what := fmt.Sprintf("apply %d change(s) to %s", len(pl.Steps), cfg.Aptly.URL)
	if pl.Destructive() {
		what += " (includes removals)"
	}
	if err := e.confirm(*yes, what); err != nil {
		return err
	}
	rw, err := e.client(cfg, false)
	if err != nil {
		return err
	}
	return plan.Apply(e.ctx, rw, pl, plan.ApplyOptions{
		Signing:       signing(cfg),
		DiscardStaged: *discard,
		Log:           func(s string) { fmt.Fprintln(e.stdout, s) },
	})
}

func (e *env) printPlan(pl *plan.Plan) {
	for _, w := range pl.Warnings {
		fmt.Fprintln(e.stderr, "warning:", w)
	}
	for _, b := range pl.Blocked {
		fmt.Fprintln(e.stderr, "blocked:", b)
	}
	if len(pl.Steps) == 0 {
		fmt.Fprintln(e.stdout, "No changes.")
		return
	}
	for _, s := range pl.Steps {
		fmt.Fprintln(e.stdout, s.String())
	}
	fmt.Fprintf(e.stdout, "%d change(s)\n", len(pl.Steps))
}

func (e *env) planJSON(pl *plan.Plan) error {
	type step struct {
		Kind        string `json:"kind"`
		Publish     string `json:"publish"`
		Target      string `json:"target"`
		Dist        string `json:"dist,omitempty"`
		Component   string `json:"component,omitempty"`
		Repo        string `json:"repo,omitempty"`
		OldRepo     string `json:"old_repo,omitempty"`
		Destructive bool   `json:"destructive"`
		Text        string `json:"text"`
	}
	out := struct {
		Steps    []step   `json:"steps"`
		Warnings []string `json:"warnings"`
		Blocked  []string `json:"blocked"`
	}{Steps: []step{}, Warnings: pl.Warnings, Blocked: pl.Blocked}
	for _, s := range pl.Steps {
		out.Steps = append(out.Steps, step{string(s.Kind), s.Publish.Alias, s.Publish.Target(), s.Dist, s.Component, s.Repo, s.OldRepo, s.Destructive, strings.Join(strings.Fields(s.String()), " ")})
	}
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	if out.Blocked == nil {
		out.Blocked = []string{}
	}
	return e.json(out)
}

func (e *env) cmdStatus(args []string) error {
	fs := newFlags(e, "status", "[alias...] [--json]")
	asJSON := fs.Bool("json", false, "JSON output")
	aliases, err := parse(fs, args)
	if err != nil {
		return ErrUsage
	}
	cfg, err := e.load()
	if err != nil {
		return err
	}
	pubs, err := cfg.Select(aliases)
	if err != nil {
		return err
	}
	c, err := e.client(cfg, true)
	if err != nil {
		return err
	}
	st, err := plan.Load(e.ctx, c, pubs)
	if err != nil {
		return err
	}
	if *asJSON {
		type cell struct {
			Publish   string `json:"publish"`
			Dist      string `json:"dist"`
			Component string `json:"component"`
			Repo      string `json:"repo"`
			Exists    bool   `json:"exists"`
			Published bool   `json:"published"`
			Packages  int    `json:"packages"`
		}
		out := []cell{}
		for _, p := range pubs {
			for _, cl := range p.Cells() {
				_, exists := st.Repos[cl.Repo]
				cur, ok := st.Published(p, cl.Dist)
				pub := ok && slices.Contains(cur.Sources, aptly.Source{Component: cl.Component, Name: cl.Repo})
				out = append(out, cell{p.Alias, cl.Dist, cl.Component, cl.Repo, exists, pub, st.PackageCount[cl.Repo]})
			}
		}
		return e.json(out)
	}
	for i, p := range pubs {
		if i > 0 {
			fmt.Fprintln(e.stdout)
		}
		retired := ""
		if p.Retire {
			retired = "  (retired: 'sync --prune' removes it)"
		}
		fmt.Fprintf(e.stdout, "%s  %s%s\n", p.Alias, p.Target(), retired)
		tw := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
		fmt.Fprint(tw, "component\t"+strings.Join(p.Dists, "\t")+"\t\n")
		for _, comp := range p.Components {
			row := []string{comp}
			for _, d := range p.Dists {
				row = append(row, cellStatus(p, d, comp, st))
			}
			fmt.Fprint(tw, strings.Join(row, "\t")+"\t\n")
		}
		tw.Flush()
	}
	fmt.Fprintln(e.stdout, "\n(number = packages, . = not in config, ! = missing in aptly, ? = not published)")
	return nil
}

func cellStatus(p *config.Publish, dist, comp string, st *plan.State) string {
	if !p.HasDist(comp, dist) {
		return "."
	}
	repo := p.RepoFor(dist, comp)
	if _, ok := st.Repos[repo]; !ok {
		return "!"
	}
	n := fmt.Sprint(st.PackageCount[repo])
	cur, ok := st.Published(p, dist)
	if !ok || !slices.Contains(cur.Sources, aptly.Source{Component: comp, Name: repo}) {
		return n + "?"
	}
	return n
}
