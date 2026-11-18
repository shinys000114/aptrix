// SPDX-License-Identifier: GPL-3.0-only

package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
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

func parseDebName(p string) (debFile, error) {
	base := filepath.Base(p)
	if !strings.HasSuffix(base, ".deb") {
		return debFile{}, fmt.Errorf("%s: not a .deb file", base)
	}
	parts := strings.Split(strings.TrimSuffix(base, ".deb"), "_")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return debFile{}, fmt.Errorf("%s: file name must be <name>_<version>_<arch>.deb", base)
	}
	return debFile{Path: p, Name: parts[0], Version: parts[1], Arch: parts[2]}, nil
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

func isURL(s string) bool { return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") }

// openLocation opens a local file or downloads an http(s) URL.
func openLocation(loc string) (io.ReadCloser, error) {
	if !isURL(loc) {
		return os.Open(loc)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Minute}).Get(loc)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: HTTP %d", loc, resp.StatusCode)
	}
	return resp.Body, nil
}

// fetchDebs downloads URL arguments into dir and returns local paths with
// their sha256 sums.
func fetchDebs(args []string, dir string) (paths []string, sums map[string]string, err error) {
	sums = map[string]string{}
	for _, a := range args {
		local := a
		var r io.ReadCloser
		if isURL(a) {
			u, perr := url.Parse(a)
			if perr != nil {
				return nil, nil, perr
			}
			local = filepath.Join(dir, path.Base(u.Path))
		}
		if r, err = openLocation(a); err != nil {
			return nil, nil, err
		}
		h := sha256.New()
		var w io.Writer = h
		var out *os.File
		if isURL(a) {
			if out, err = os.Create(local); err != nil {
				r.Close()
				return nil, nil, err
			}
			w = io.MultiWriter(h, out)
		}
		_, err = io.Copy(w, r)
		r.Close()
		if out != nil {
			if cerr := out.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", a, err)
		}
		paths = append(paths, local)
		sums[local] = hex.EncodeToString(h.Sum(nil))
	}
	return paths, sums, nil
}

// readChecksums parses a sha256sum-style file ("<hex>  <name>").
func readChecksums(loc string) (map[string]string, error) {
	r, err := openLocation(loc)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && len(f[0]) == 64 {
			out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return out, sc.Err()
}

func (e *env) cmdAdd(args []string) error {
	fs := newFlags(e, "add", "<alias> <component> <file.deb|URL>... [--dist d1,d2|all]")
	distFlag := fs.String("dist", "", `target dists (comma separated or "all"); default: from the "~<dist>" version suffix`)
	forceReplace := fs.Bool("force-replace", false, "replace an existing package with the same name/version/arch but different content\n(also overwrites the published pool file; clients that cached the old file will see a hash mismatch)")
	noPublish := fs.Bool("no-publish", false, "add to the repos but do not republish")
	checksums := fs.String("checksums", "", "sha256sum-style file or URL (e.g. a release checksums.txt); every .deb must be listed and match")
	snap := fs.Bool("snapshot", false, "snapshot the target repos first even when nothing will be replaced or pruned")
	noSnap := fs.Bool("no-snapshot", false, "never snapshot (by default aptrix snapshots when --force-replace or keep may remove packages)")
	pos, err := parse(fs, args)
	if err != nil || len(pos) < 3 {
		fs.Usage()
		return ErrUsage
	}
	alias, comp, locs := pos[0], pos[1], pos[2:]
	cfg, err := e.load()
	if err != nil {
		return err
	}
	pubs, err := cfg.Select([]string{alias})
	if err != nil {
		return err
	}
	p := pubs[0]
	if p.Retire {
		return fmt.Errorf("publish.%s is retired; add packages to its replacement instead", alias)
	}
	if !slices.Contains(p.Components, comp) {
		return fmt.Errorf("component %q is not in publish.%s (components: %s)", comp, alias, strings.Join(p.Components, ", "))
	}
	explicit, err := targetDists(p, *distFlag)
	if err != nil {
		return err
	}
	// Validate names before downloading anything.
	for _, l := range locs {
		name := l
		if isURL(l) {
			u, err := url.Parse(l)
			if err != nil {
				return err
			}
			name = path.Base(u.Path)
		}
		if _, err := parseDebName(name); err != nil {
			return err
		}
	}
	tmp, err := os.MkdirTemp("", "aptrix-add-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	files, sums, err := fetchDebs(locs, tmp)
	if err != nil {
		return err
	}
	if *checksums != "" {
		want, err := readChecksums(*checksums)
		if err != nil {
			return fmt.Errorf("checksums: %w", err)
		}
		for _, f := range files {
			w, ok := want[filepath.Base(f)]
			if !ok {
				return fmt.Errorf("%s is not listed in %s", filepath.Base(f), *checksums)
			}
			if w != sums[f] {
				return fmt.Errorf("%s: sha256 %s does not match %s", filepath.Base(f), sums[f], w)
			}
		}
		fmt.Fprintf(e.stdout, "checksums ok (%d file(s))\n", len(files))
	} else {
		for _, f := range files {
			if strings.HasPrefix(f, tmp) {
				fmt.Fprintf(e.stdout, "sha256 %s  %s (not verified; use --checksums)\n", sums[f], filepath.Base(f))
			}
		}
	}

	// Resolve every file to its target dists before touching aptly.
	byDist := map[string][]debFile{}
	var names []string
	for _, f := range files {
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
		if !slices.Contains(names, d.Name) {
			names = append(names, d.Name)
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
	touched := map[string]config.Cell{}
	for _, dist := range dists {
		repo := p.RepoFor(dist, comp)
		if _, ok := st.Repos[repo]; !ok {
			return fmt.Errorf("repo %s does not exist yet; run 'aptrix sync %s' first", repo, alias)
		}
		if _, ok := st.Published(p, dist); !ok && !*noPublish {
			return fmt.Errorf("%s %s is not published yet; run 'aptrix sync %s' first", p.Target(), dist, alias)
		}
		touched[repo] = config.Cell{Dist: dist, Component: comp, Repo: repo}
	}
	if !*noSnap && (*snap || *forceReplace || p.Keep > 0) {
		if _, err := e.preSnapshot(c, p, sortedKeys(touched), "pre-add", "aptrix: before add "+strings.Join(names, " ")); err != nil {
			return err
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
	err = e.finish(c, cfg, p, touched, names, *forceReplace, *noPublish)
	if err != nil {
		return fmt.Errorf("%w\n(packages are in the repos; rerun the same command to retry)", err)
	}
	return nil
}

// preSnapshot snapshots repos under a new "<prefix>-<time>" label.
func (e *env) preSnapshot(c *aptly.Client, p *config.Publish, repos []string, prefix, desc string) (string, error) {
	if len(repos) == 0 {
		return "", nil
	}
	label := newLabel(prefix)
	if err := e.takeSnapshots(c, repos, label, desc); err != nil {
		return "", err
	}
	fmt.Fprintf(e.stdout, "snapshot %s taken; undo with 'aptrix rollback %s %s'\n", label, p.Alias, label)
	return label, nil
}

// finish runs after repos of p changed: enforces p.Keep for the given
// package names, republishes the touched dists and expires old automatic
// snapshots.
func (e *env) finish(c *aptly.Client, cfg *config.Config, p *config.Publish, touched map[string]config.Cell, names []string, forceOverwrite, noPublish bool) error {
	if p.Keep > 0 && len(names) > 0 {
		m, _ := newMatcher(names, "", "")
		var rows []pkgRow
		for _, repo := range sortedKeys(touched) {
			cell := touched[repo]
			pkgs, err := c.Packages(e.ctx, repo)
			if err != nil {
				return err
			}
			for _, pk := range pkgs {
				if m.match(pk.Package, pk.Version, pk.Architecture) {
					rows = append(rows, pkgRow{Dist: cell.Dist, Component: cell.Component, Repo: repo, Package: pk})
				}
			}
		}
		if err := e.deleteRows(c, beyondKeep(rows, p.Keep), fmt.Sprintf("keep %d", p.Keep)); err != nil {
			return err
		}
	}
	if !noPublish {
		if err := e.republishCells(c, cfg, p, touched, forceOverwrite); err != nil {
			return err
		}
	}
	// Expiry is housekeeping: never fail a change that already succeeded.
	if err := e.expireSnapshots(c, p); err != nil {
		fmt.Fprintf(e.stderr, "warning: snapshot expiry: %v\n", err)
	}
	return nil
}

// deleteRows removes packages, grouped by repo.
func (e *env) deleteRows(c *aptly.Client, rows []pkgRow, why string) error {
	byRepo := map[string][]string{}
	for _, r := range rows {
		byRepo[r.Repo] = append(byRepo[r.Repo], r.Key)
	}
	for _, repo := range sortedKeys(byRepo) {
		if err := c.DeletePackages(e.ctx, repo, byRepo[repo]); err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "removed %d from %s (%s)\n", len(byRepo[repo]), repo, why)
	}
	return nil
}

// republishCells updates the publishes of the dists in touched, in config order.
func (e *env) republishCells(c *aptly.Client, cfg *config.Config, p *config.Publish, touched map[string]config.Cell, forceOverwrite bool) error {
	st, err := plan.Load(e.ctx, c, nil)
	if err != nil {
		return err
	}
	var dists []string
	for _, d := range p.Dists {
		for _, cell := range touched {
			if cell.Dist == d {
				if _, ok := st.Published(p, d); ok && !slices.Contains(dists, d) {
					dists = append(dists, d)
				}
			}
		}
	}
	return e.republish(c, cfg, p, dists, forceOverwrite)
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
	Alias, Dist, Component, Repo string
	aptly.Package
}

// collect lists packages of the given cells, newest first within a package.
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
				rows = append(rows, pkgRow{Alias: p.Alias, Dist: d, Component: comp, Repo: repo, Package: pk})
			}
		}
	}
	sortRows(rows, p.Dists)
	return rows, nil
}

func sortRows(rows []pkgRow, distOrder []string) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Alias != b.Alias {
			return a.Alias < b.Alias
		}
		if a.Dist != b.Dist {
			return slices.Index(distOrder, a.Dist) < slices.Index(distOrder, b.Dist)
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
}

type jsonPkg struct {
	Alias        string `json:"alias"`
	Dist         string `json:"dist"`
	Component    string `json:"component"`
	Package      string `json:"package"`
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
	Key          string `json:"key"`
}

func (e *env) printRows(rows []pkgRow, withAlias, asJSON bool) error {
	if asJSON {
		out := make([]jsonPkg, len(rows))
		for i, r := range rows {
			out[i] = jsonPkg{r.Alias, r.Dist, r.Component, r.Package.Package, r.Version, r.Architecture, r.Key}
		}
		return e.json(out)
	}
	tw := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)
	head := "DIST\tCOMPONENT\tPACKAGE\tVERSION\tARCH"
	if withAlias {
		head = "ALIAS\t" + head
	}
	fmt.Fprintln(tw, head)
	for _, r := range rows {
		if withAlias {
			fmt.Fprintf(tw, "%s\t", r.Alias)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Dist, r.Component, r.Package.Package, r.Version, r.Architecture)
	}
	tw.Flush()
	fmt.Fprintf(e.stdout, "%d package(s)\n", len(rows))
	return nil
}

func (e *env) json(v any) error {
	enc := json.NewEncoder(e.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func filterRows(rows []pkgRow, m *matcher) []pkgRow {
	var out []pkgRow
	for _, r := range rows {
		if m.match(r.Package.Package, r.Version, r.Architecture) {
			out = append(out, r)
		}
	}
	return out
}

func (e *env) cmdLs(args []string) error {
	fs := newFlags(e, "ls", "<alias> [component] [--dist d1,d2] [--name glob] [--version cond] [--arch a] [--json]")
	distFlag := fs.String("dist", "", "only these dists (comma separated)")
	name := fs.String("name", "", `only package names matching this glob (e.g. "linux-image-*")`)
	version := fs.String("version", "", `version conditions, e.g. ">=1.2" or ">=1.0,<2.0" (dpkg ordering)`)
	arch := fs.String("arch", "", "only this architecture")
	asJSON := fs.Bool("json", false, "JSON output")
	pos, err := parse(fs, args)
	if err != nil || len(pos) < 1 || len(pos) > 2 {
		fs.Usage()
		return ErrUsage
	}
	var names []string
	if *name != "" {
		names = []string{*name}
	}
	m, err := newMatcher(names, *version, *arch)
	if err != nil {
		return err
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
	return e.printRows(filterRows(rows, m), false, *asJSON)
}

func (e *env) cmdRm(args []string) error {
	fs := newFlags(e, "rm", "<alias> <component> <glob>[=version]... [--version cond] [--dist d1,d2|all] [--keep N] [--yes]")
	distFlag := fs.String("dist", "all", `dists to remove from (comma separated or "all")`)
	keep := fs.Int("keep", -1, "keep the newest N versions per package, dist and architecture; remove the rest")
	arch := fs.String("arch", "", "only this architecture")
	version := fs.String("version", "", `version conditions, e.g. "<9:6.6.144-202609010000" (dpkg ordering: numbers compare by value, not as prefixes)`)
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	noPublish := fs.Bool("no-publish", false, "remove from the repos but do not republish")
	noSnap := fs.Bool("no-snapshot", false, "do not snapshot the affected repos before removing")
	pos, err := parse(fs, args)
	if err != nil || len(pos) < 3 {
		fs.Usage()
		return ErrUsage
	}
	alias, comp, specs := pos[0], pos[1], pos[2:]
	type spec struct{ pat, ver string }
	var sp []spec
	for _, a := range specs {
		n, v, hasV := strings.Cut(a, "=")
		if hasV && v == "" {
			return fmt.Errorf("empty version in %q: %w", a, ErrUsage)
		}
		if hasV && (*keep >= 0 || *version != "") {
			return fmt.Errorf("use either <package>=<version> or --keep/--version, not both: %w", ErrUsage)
		}
		if _, err := path.Match(n, ""); err != nil {
			return fmt.Errorf("bad package pattern %q: %v: %w", n, err, ErrUsage)
		}
		sp = append(sp, spec{n, v})
	}
	m, err := newMatcher(nil, *version, *arch)
	if err != nil {
		return fmt.Errorf("%v: %w", err, ErrUsage)
	}
	matchSpec := func(r pkgRow) bool {
		for _, s := range sp {
			if hit, _ := path.Match(s.pat, r.Package.Package); hit && (s.ver == "" || s.ver == r.Version) {
				return true
			}
		}
		return false
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
	var selected []pkgRow
	for _, r := range filterRows(rows, m) {
		if matchSpec(r) {
			selected = append(selected, r)
		}
	}
	rows = selected
	victims := rows
	if *keep >= 0 {
		victims = beyondKeep(rows, *keep)
		sortRows(victims, p.Dists)
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
	touched := map[string]config.Cell{}
	for _, r := range victims {
		touched[r.Repo] = config.Cell{Dist: r.Dist, Component: r.Component, Repo: r.Repo}
	}
	if !*noSnap {
		if _, err := e.preSnapshot(c, p, sortedKeys(touched), "pre-rm", "aptrix: before rm "+strings.Join(specs, " ")); err != nil {
			return err
		}
	}
	if err := e.deleteRows(c, victims, "rm"); err != nil {
		return err
	}
	return e.finish(c, cfg, p, touched, nil, false, *noPublish)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
