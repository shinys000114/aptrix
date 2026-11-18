// SPDX-License-Identifier: GPL-3.0-only

package plan

import (
	"strings"
	"testing"

	"github.com/shinys000114/aptrix/internal/aptly"
	"github.com/shinys000114/aptrix/internal/config"
)

func pub(t *testing.T, extra string) *config.Publish {
	t.Helper()
	c, err := config.Parse([]byte(`
[aptly]
url = "http://127.0.0.1:1"
[signing]
skip = true
[publish.b]
storage = "filesystem:public"
prefix = "b"
dists = ["jammy", "noble"]
architectures = ["arm64"]
` + extra))
	if err != nil {
		t.Fatal(err)
	}
	return c.Publish["b"]
}

func published(dist string, comps ...string) aptly.Published {
	p := aptly.Published{Storage: "filesystem:public", Prefix: "b", Distribution: dist, SourceKind: "local", Architectures: []string{"arm64"}}
	for _, c := range comps {
		p.Sources = append(p.Sources, aptly.Source{Component: c, Name: "b-" + dist + "-" + c})
	}
	return p
}

func state(pubs ...aptly.Published) *State {
	s := &State{Repos: map[string]aptly.Repo{}, Publishes: pubs, PackageCount: map[string]int{}}
	for _, p := range pubs {
		for _, src := range p.Sources {
			parts := strings.SplitN(strings.TrimPrefix(src.Name, "b-"), "-", 2)
			s.Repos[src.Name] = aptly.Repo{Name: src.Name, DefaultDistribution: parts[0], DefaultComponent: parts[1]}
		}
	}
	return s
}

func lines(pl *Plan) string {
	var out []string
	for _, s := range pl.Steps {
		out = append(out, strings.Join(strings.Fields(s.String()), " "))
	}
	return strings.Join(out, "\n")
}

func TestNoChanges(t *testing.T) {
	p := pub(t, `components = ["main", "x"]`)
	pl := Make([]*config.Publish{p}, state(published("jammy", "main", "x"), published("noble", "main", "x")), Options{Prune: true})
	if len(pl.Steps) != 0 || len(pl.Warnings) != 0 {
		t.Fatalf("expected nothing, got %q %v", lines(pl), pl.Warnings)
	}
}

func TestCreateFromScratch(t *testing.T) {
	p := pub(t, "components = [\"main\", \"x\"]\n[publish.b.only]\nx = [\"noble\"]")
	pl := Make([]*config.Publish{p}, state(), Options{})
	want := `+ repo b-jammy-main
+ repo b-noble-main
+ repo b-noble-x
+ publish filesystem:public:b jammy [main]
+ publish filesystem:public:b noble [main x]`
	if got := lines(pl); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if pl.Destructive() {
		t.Fatal("creation is not destructive")
	}
}

func TestAddComponentAndPrune(t *testing.T) {
	cur := state(published("jammy", "main", "old"), published("noble", "main", "old"), published("focal", "main"))
	cur.PackageCount["b-noble-old"] = 3
	p := pub(t, `components = ["main", "new"]`)

	pl := Make([]*config.Publish{p}, cur, Options{})
	want := `+ repo b-jammy-new
+ repo b-noble-new
+ source filesystem:public:b jammy new <- b-jammy-new
update filesystem:public:b jammy
+ source filesystem:public:b noble new <- b-noble-new
update filesystem:public:b noble`
	if got := lines(pl); got != want {
		t.Fatalf("without prune got\n%s\nwant\n%s", got, want)
	}
	if len(pl.Warnings) != 3 { // old in jammy, old in noble, focal published
		t.Fatalf("warnings = %v", pl.Warnings)
	}

	pl = Make([]*config.Publish{p}, cur, Options{Prune: true})
	got := lines(pl)
	for _, w := range []string{
		"- source filesystem:public:b jammy old (repo b-jammy-old)",
		"- publish filesystem:public:b focal",
		"- repo b-jammy-old (0 packages)",
		"- repo b-focal-main (0 packages)",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in\n%s", w, got)
		}
	}
	if strings.Contains(got, "- repo b-noble-old") || strings.Contains(got, "noble old") {
		t.Error("non-empty component must stay published and its repo kept without force")
	}
	if len(pl.Blocked) != 1 || !pl.Destructive() {
		t.Fatalf("blocked = %v", pl.Blocked)
	}
	if !strings.HasSuffix(got, "(0 packages)") || strings.Index(got, "- publish") > strings.Index(got, "- repo") {
		t.Fatalf("drops must come last, publish before repo:\n%s", got)
	}

	pl = Make([]*config.Publish{p}, cur, Options{Prune: true, Force: true})
	got = lines(pl)
	if !strings.Contains(got, "- source filesystem:public:b noble old (repo b-noble-old)") || !strings.Contains(got, "- repo b-noble-old (3 packages)") {
		t.Fatalf("force should unpublish and drop the non-empty repo:\n%s", got)
	}
}

func TestRemovingAllComponentsDropsDist(t *testing.T) {
	cur := state(published("jammy", "old"), published("noble", "main"))
	p := pub(t, "components = [\"main\"]\n[publish.b.only]\nmain = [\"noble\"]")
	pl := Make([]*config.Publish{p}, cur, Options{Prune: true})
	got := lines(pl)
	if !strings.Contains(got, "- publish filesystem:public:b jammy") || strings.Contains(got, "update filesystem:public:b jammy") {
		t.Fatalf("got\n%s", got)
	}
}

func TestReplaceAndDrift(t *testing.T) {
	cur := state(published("jammy", "main"), published("noble", "main"))
	cur.Publishes[0].Sources[0].Name = "legacy-main"
	cur.Repos["legacy-main"] = aptly.Repo{Name: "legacy-main"}
	cur.Publishes[1].Architectures = []string{"amd64", "arm64"}
	cur.Publishes[1].Label = "Old"
	p := pub(t, "components = [\"main\"]\nlabel = \"New\"")
	pl := Make([]*config.Publish{p}, cur, Options{})
	if !strings.Contains(lines(pl), "~ source filesystem:public:b jammy main <- b-jammy-main (was legacy-main)") {
		t.Fatalf("got\n%s", lines(pl))
	}
	w := strings.Join(pl.Warnings, "\n")
	if !strings.Contains(w, "architectures") || !strings.Contains(w, `label is "Old"`) {
		t.Fatalf("warnings = %s", w)
	}
}

func TestOtherPrefixesIgnored(t *testing.T) {
	other := published("noble", "main")
	other.Prefix = "someone-else"
	p := pub(t, `components = ["main"]`)
	cur := state(published("jammy", "main"), published("noble", "main"), other)
	if pl := Make([]*config.Publish{p}, cur, Options{Prune: true}); len(pl.Steps) != 0 {
		t.Fatalf("foreign publish touched:\n%s", lines(pl))
	}
}

func TestPruneKeepsNonEmptyDist(t *testing.T) {
	cur := state(published("jammy", "main"), published("noble", "main"), published("focal", "main"))
	cur.PackageCount["b-focal-main"] = 2
	p := pub(t, `components = ["main"]`)
	pl := Make([]*config.Publish{p}, cur, Options{Prune: true})
	if len(pl.Steps) != 0 || len(pl.Blocked) != 1 || !strings.Contains(pl.Blocked[0], "focal") {
		t.Fatalf("steps=%q blocked=%v", lines(pl), pl.Blocked)
	}
	pl = Make([]*config.Publish{p}, cur, Options{Prune: true, Force: true})
	if got := lines(pl); got != "- publish filesystem:public:b focal\n- repo b-focal-main (2 packages)" {
		t.Fatalf("got\n%s", got)
	}
}

func TestRetire(t *testing.T) {
	cur := state(published("jammy", "main"), published("noble", "main"))
	cur.Repos["b-focal-main"] = aptly.Repo{Name: "b-focal-main"} // leftover, never published
	foreign := aptly.Published{Storage: "filesystem:public", Prefix: "other", Distribution: "noble", SourceKind: "local",
		Sources: []aptly.Source{{Component: "main", Name: "b-noble-main"}}}
	cur.Publishes = append(cur.Publishes, foreign)
	c, err := config.Parse([]byte(`
[aptly]
url = "http://127.0.0.1:1"
[signing]
skip = true
[publish.b]
storage = "filesystem:public"
prefix = "b"
retire = true
dists = ["focal", "jammy", "noble"]
components = ["main"]
architectures = ["arm64"]
`))
	if err != nil {
		t.Fatal(err)
	}
	p := c.Publish["b"]

	pl := Make([]*config.Publish{p}, cur, Options{})
	if len(pl.Steps) != 0 || len(pl.Warnings) != 2 || !strings.Contains(pl.Warnings[0], "retired") {
		t.Fatalf("without prune: steps=%q warnings=%v", lines(pl), pl.Warnings)
	}

	pl = Make([]*config.Publish{p}, cur, Options{Prune: true})
	want := `- publish filesystem:public:b jammy
- publish filesystem:public:b noble
- repo b-focal-main (0 packages)
- repo b-jammy-main (0 packages)`
	if got := lines(pl); got != want {
		t.Fatalf("prune got\n%s\nwant\n%s", got, want)
	}
	if !strings.Contains(strings.Join(pl.Warnings, "\n"), "b-noble-main is also published by filesystem:public:other noble") {
		t.Fatalf("foreign reference must keep the repo: %v", pl.Warnings)
	}

	cur.PackageCount["b-jammy-main"] = 2
	pl = Make([]*config.Publish{p}, cur, Options{Prune: true})
	if strings.Contains(lines(pl), "jammy") || len(pl.Blocked) != 1 {
		t.Fatalf("non-empty dist must be blocked: %q %v", lines(pl), pl.Blocked)
	}
	pl = Make([]*config.Publish{p}, cur, Options{Prune: true, Force: true})
	if !strings.Contains(lines(pl), "- repo b-jammy-main (2 packages)") {
		t.Fatalf("force: %q", lines(pl))
	}
}
