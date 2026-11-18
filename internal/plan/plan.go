// SPDX-License-Identifier: GPL-3.0-only

// Package plan compares the desired matrix (config) with aptly's current
// state and produces an ordered list of steps, then applies them.
package plan

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/shinys000114/aptrix/internal/aptly"
	"github.com/shinys000114/aptrix/internal/config"
)

// State is a snapshot of what aptly currently has.
type State struct {
	Repos     map[string]aptly.Repo
	Publishes []aptly.Published
	// PackageCount holds the number of packages for every repo that is a
	// source of a managed publish or a desired cell.
	PackageCount map[string]int
}

// Load reads the state needed to plan the given publishes.
func Load(ctx context.Context, c *aptly.Client, pubs []*config.Publish) (*State, error) {
	repos, err := c.Repos(ctx)
	if err != nil {
		return nil, err
	}
	pl, err := c.Publishes(ctx)
	if err != nil {
		return nil, err
	}
	s := &State{Repos: map[string]aptly.Repo{}, Publishes: pl, PackageCount: map[string]int{}}
	for _, r := range repos {
		s.Repos[r.Name] = r
	}
	want := map[string]bool{}
	for _, p := range pubs {
		for _, cell := range p.Cells() {
			want[cell.Repo] = true
		}
		for _, cur := range s.managed(p) {
			for _, src := range cur.Sources {
				want[src.Name] = true
			}
		}
	}
	names := make([]string, 0, len(want))
	for n := range want {
		if _, ok := s.Repos[n]; ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		keys, err := c.PackageKeys(ctx, n)
		if err != nil {
			return nil, err
		}
		s.PackageCount[n] = len(keys)
	}
	return s, nil
}

func normPrefix(p string) string {
	if p == "" {
		return "."
	}
	return strings.Trim(p, "/")
}

// managed returns the current publishes that belong to p.
func (s *State) managed(p *config.Publish) []aptly.Published {
	var out []aptly.Published
	for _, cur := range s.Publishes {
		if cur.Storage == p.Storage && normPrefix(cur.Prefix) == normPrefix(p.Prefix) {
			out = append(out, cur)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Distribution < out[j].Distribution })
	return out
}

// Published returns the current publish of p for dist, if any.
func (s *State) Published(p *config.Publish, dist string) (aptly.Published, bool) {
	for _, cur := range s.managed(p) {
		if cur.Distribution == dist {
			return cur, true
		}
	}
	return aptly.Published{}, false
}

// Kind is a step type.
type Kind string

// Step kinds, in the order they are applied.
const (
	CreateRepo    Kind = "create-repo"
	CreatePublish Kind = "create-publish"
	AddSource     Kind = "add-source"
	ReplaceSource Kind = "replace-source"
	RemoveSource  Kind = "remove-source"
	UpdatePublish Kind = "update-publish"
	DropPublish   Kind = "drop-publish"
	DropRepo      Kind = "drop-repo"
)

// Step is one change.
type Step struct {
	Kind        Kind
	Publish     *config.Publish
	Dist        string
	Component   string
	Repo        string
	OldRepo     string
	Sources     []aptly.Source
	Packages    int
	Destructive bool
}

func (s Step) String() string {
	switch s.Kind {
	case CreateRepo:
		return fmt.Sprintf("+ repo     %s", s.Repo)
	case CreatePublish:
		comps := make([]string, len(s.Sources))
		for i, src := range s.Sources {
			comps[i] = src.Component
		}
		return fmt.Sprintf("+ publish  %s %s [%s]", s.Publish.Target(), s.Dist, strings.Join(comps, " "))
	case AddSource:
		return fmt.Sprintf("+ source   %s %s %s <- %s", s.Publish.Target(), s.Dist, s.Component, s.Repo)
	case ReplaceSource:
		return fmt.Sprintf("~ source   %s %s %s <- %s (was %s)", s.Publish.Target(), s.Dist, s.Component, s.Repo, s.OldRepo)
	case RemoveSource:
		return fmt.Sprintf("- source   %s %s %s (repo %s)", s.Publish.Target(), s.Dist, s.Component, s.OldRepo)
	case UpdatePublish:
		return fmt.Sprintf("  update   %s %s", s.Publish.Target(), s.Dist)
	case DropPublish:
		return fmt.Sprintf("- publish  %s %s", s.Publish.Target(), s.Dist)
	case DropRepo:
		return fmt.Sprintf("- repo     %s (%d packages)", s.Repo, s.Packages)
	}
	return string(s.Kind)
}

// Plan is the result of comparing config with state.
type Plan struct {
	Steps    []Step
	Warnings []string
	// Blocked lists destructive changes that were not planned because they
	// would delete packages; --force allows them.
	Blocked []string
}

// Destructive reports whether any step removes something.
func (p *Plan) Destructive() bool {
	return slices.ContainsFunc(p.Steps, func(s Step) bool { return s.Destructive })
}

// Options control planning.
type Options struct {
	// Prune removes components, dists and repos that are not in the config.
	Prune bool
	// Force allows pruning repos that still contain packages.
	Force bool
}

// Make builds the plan for pubs.
func Make(pubs []*config.Publish, s *State, o Options) *Plan {
	pl := &Plan{}
	var creates, publishes, drops, repoDrops []Step
	for _, p := range pubs {
		desired := map[string]bool{}
		for _, cell := range p.Cells() {
			desired[cell.Repo] = true
			if r, ok := s.Repos[cell.Repo]; !ok {
				creates = append(creates, Step{Kind: CreateRepo, Publish: p, Dist: cell.Dist, Component: cell.Component, Repo: cell.Repo})
			} else if r.DefaultDistribution != "" && (r.DefaultDistribution != cell.Dist || r.DefaultComponent != cell.Component) {
				pl.Warnings = append(pl.Warnings, fmt.Sprintf("repo %s defaults to %s/%s, config cell is %s/%s",
					cell.Repo, r.DefaultDistribution, r.DefaultComponent, cell.Dist, cell.Component))
			}
		}

		// unpublished collects repos this plan stops publishing (candidates
		// for dropping); stillRef counts references that remain afterwards.
		unpublished := map[string]bool{}
		stillRef := map[string]int{}
		nonEmpty := func(repo string) int { return s.PackageCount[repo] }

		for _, dist := range p.Dists {
			cells := p.CellsFor(dist)
			cur, ok := s.Published(p, dist)
			if !ok {
				if len(cells) == 0 {
					continue
				}
				srcs := make([]aptly.Source, len(cells))
				for i, c := range cells {
					srcs[i] = aptly.Source{Component: c.Component, Name: c.Repo}
				}
				publishes = append(publishes, Step{Kind: CreatePublish, Publish: p, Dist: dist, Sources: srcs})
				continue
			}
			if cur.SourceKind != "local" {
				pl.Warnings = append(pl.Warnings, fmt.Sprintf("%s %s publishes %ss, not local repos; skipped", p.Target(), dist, cur.SourceKind))
				continue
			}
			pl.Warnings = append(pl.Warnings, drift(p, cur)...)
			have := map[string]string{}
			for _, src := range cur.Sources {
				have[src.Component] = src.Name
			}
			var staged []Step
			removed := map[string]bool{}
			for _, c := range cells {
				old, ok := have[c.Component]
				switch {
				case !ok:
					staged = append(staged, Step{Kind: AddSource, Publish: p, Dist: dist, Component: c.Component, Repo: c.Repo})
				case old != c.Repo:
					staged = append(staged, Step{Kind: ReplaceSource, Publish: p, Dist: dist, Component: c.Component, Repo: c.Repo, OldRepo: old})
					removed[c.Component] = true
					unpublished[old] = true
				}
			}
			for _, src := range cur.Sources {
				if p.HasDist(src.Component, dist) {
					continue
				}
				if !o.Prune {
					pl.Warnings = append(pl.Warnings, fmt.Sprintf("%s %s has component %s which is not in the config (use --prune to remove)", p.Target(), dist, src.Component))
					continue
				}
				if n := nonEmpty(src.Name); n > 0 && !o.Force {
					pl.Blocked = append(pl.Blocked, fmt.Sprintf("%s %s component %s (repo %s) still has %d packages; kept published (use --force to remove)",
						p.Target(), dist, src.Component, src.Name, n))
					continue
				}
				staged = append(staged, Step{Kind: RemoveSource, Publish: p, Dist: dist, Component: src.Component, OldRepo: src.Name, Destructive: true})
				removed[src.Component] = true
				unpublished[src.Name] = true
			}
			for _, src := range cur.Sources {
				if !removed[src.Component] {
					stillRef[src.Name]++
				}
			}
			if len(staged) == 0 {
				continue
			}
			if len(staged) == countKind(staged, RemoveSource) && len(cur.Sources) == len(staged) {
				// aptly cannot publish zero components; drop the dist instead.
				drops = append(drops, Step{Kind: DropPublish, Publish: p, Dist: dist, Destructive: true})
				continue
			}
			publishes = append(publishes, staged...)
			publishes = append(publishes, Step{Kind: UpdatePublish, Publish: p, Dist: dist})
		}

		for _, cur := range s.managed(p) {
			if slices.Contains(p.Dists, cur.Distribution) {
				continue
			}
			if !o.Prune {
				pl.Warnings = append(pl.Warnings, fmt.Sprintf("%s %s is published but not in the config (use --prune to remove)", p.Target(), cur.Distribution))
				for _, src := range cur.Sources {
					stillRef[src.Name]++
				}
				continue
			}
			full := 0
			for _, src := range cur.Sources {
				full += nonEmpty(src.Name)
			}
			if full > 0 && !o.Force {
				pl.Blocked = append(pl.Blocked, fmt.Sprintf("%s %s is not in the config but still has %d packages; kept published (use --force to remove)",
					p.Target(), cur.Distribution, full))
				for _, src := range cur.Sources {
					stillRef[src.Name]++
				}
				continue
			}
			drops = append(drops, Step{Kind: DropPublish, Publish: p, Dist: cur.Distribution, Destructive: true})
			for _, src := range cur.Sources {
				unpublished[src.Name] = true
			}
		}

		if o.Prune {
			names := make([]string, 0, len(unpublished))
			for n := range unpublished {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, name := range names {
				if desired[name] || stillRef[name] > 0 {
					continue
				}
				if _, exists := s.Repos[name]; !exists {
					continue
				}
				n := nonEmpty(name)
				if n > 0 && !o.Force {
					pl.Blocked = append(pl.Blocked, fmt.Sprintf("repo %s still has %d packages; kept (use --force to delete)", name, n))
					continue
				}
				repoDrops = append(repoDrops, Step{Kind: DropRepo, Publish: p, Repo: name, Packages: n, Destructive: true})
			}
		}
	}
	pl.Steps = slices.Concat(creates, publishes, drops, repoDrops)
	return pl
}

func countKind(steps []Step, k Kind) int {
	n := 0
	for _, s := range steps {
		if s.Kind == k {
			n++
		}
	}
	return n
}

func drift(p *config.Publish, cur aptly.Published) []string {
	var w []string
	a, b := slices.Clone(p.Architectures), slices.Clone(cur.Architectures)
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		w = append(w, fmt.Sprintf("%s %s architectures are %v, config says %v (aptly cannot change this in place)", p.Target(), cur.Distribution, b, a))
	}
	if p.Origin != "" && p.Origin != cur.Origin {
		w = append(w, fmt.Sprintf("%s %s origin is %q, config says %q", p.Target(), cur.Distribution, cur.Origin, p.Origin))
	}
	if p.Label != "" && p.Label != cur.Label {
		w = append(w, fmt.Sprintf("%s %s label is %q, config says %q", p.Target(), cur.Distribution, cur.Label, p.Label))
	}
	return w
}

// ApplyOptions control Apply.
type ApplyOptions struct {
	Signing aptly.Signing
	// DiscardStaged drops source changes left staged by someone else
	// (e.g. a crashed run). Without it Apply refuses to touch that dist.
	DiscardStaged bool
	// Log receives one line per step.
	Log func(string)
}

// Apply executes the plan in order. It stops at the first error.
func Apply(ctx context.Context, c *aptly.Client, pl *Plan, o ApplyOptions) error {
	log := o.Log
	if log == nil {
		log = func(string) {}
	}
	checked := map[string]bool{}
	for _, s := range pl.Steps {
		p := s.Publish
		key := p.Target() + " " + s.Dist
		switch s.Kind {
		case AddSource, ReplaceSource, RemoveSource:
			if !checked[key] {
				pending, err := c.PendingSources(ctx, p.Storage, p.Prefix, s.Dist)
				if err != nil {
					return err
				}
				if pending != nil {
					if !o.DiscardStaged {
						return fmt.Errorf("%s has staged source changes from another run; inspect them or rerun with --discard-staged", key)
					}
					log("  discard staged changes " + key)
					if err := c.DropPendingSources(ctx, p.Storage, p.Prefix, s.Dist); err != nil {
						return err
					}
				}
				checked[key] = true
			}
		}
		log(s.String())
		var err error
		switch s.Kind {
		case CreateRepo:
			err = c.CreateRepo(ctx, aptly.Repo{Name: s.Repo, Comment: p.CommentFor(s.Dist, s.Component),
				DefaultDistribution: s.Dist, DefaultComponent: s.Component})
		case CreatePublish:
			err = c.CreatePublish(ctx, p.Storage, p.Prefix, aptly.PublishCreate{Sources: s.Sources, Distribution: s.Dist,
				Architectures: p.Architectures, Origin: p.Origin, Label: p.Label, Signing: o.Signing})
		case AddSource:
			err = c.StageAddSource(ctx, p.Storage, p.Prefix, s.Dist, aptly.Source{Component: s.Component, Name: s.Repo})
		case ReplaceSource:
			err = c.StageReplaceSource(ctx, p.Storage, p.Prefix, s.Dist, aptly.Source{Component: s.Component, Name: s.Repo})
		case RemoveSource:
			err = c.StageRemoveSource(ctx, p.Storage, p.Prefix, s.Dist, s.Component)
		case UpdatePublish:
			err = c.UpdatePublish(ctx, p.Storage, p.Prefix, s.Dist, o.Signing, false)
		case DropPublish:
			err = c.DropPublish(ctx, p.Storage, p.Prefix, s.Dist)
		case DropRepo:
			// Re-check right before deleting: aptly drops non-empty repos.
			keys, kerr := c.PackageKeys(ctx, s.Repo)
			if kerr != nil {
				return kerr
			}
			if len(keys) != s.Packages {
				return fmt.Errorf("repo %s changed since planning (%d packages now, %d planned); aborting", s.Repo, len(keys), s.Packages)
			}
			err = c.DropRepo(ctx, s.Repo)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", strings.TrimSpace(s.String()), err)
		}
	}
	return nil
}
