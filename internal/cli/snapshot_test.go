// SPDX-License-Identifier: GPL-3.0-only

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/shinys000114/aptrix/internal/aptly"
	"github.com/shinys000114/aptrix/internal/config"
)

func TestDiffKeys(t *testing.T) {
	add, del := diffKeys([]string{"a", "b", "c"}, []string{"b", "c", "d", "e"})
	if strings.Join(add, ",") != "d,e" || strings.Join(del, ",") != "a" {
		t.Fatalf("add=%v del=%v", add, del)
	}
	add, del = diffKeys([]string{"x"}, []string{"x"})
	if len(add)+len(del) != 0 {
		t.Fatal("identical sets should produce no changes")
	}
}

func TestLabels(t *testing.T) {
	c, err := config.Parse([]byte(`
[aptly]
url = "http://127.0.0.1:1"
[signing]
skip = true
[publish.b]
prefix = "b"
dists = ["noble"]
components = ["main", "x-factory"]
architectures = ["arm64"]
`))
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	snaps := []aptly.Snapshot{
		{Name: "b-noble-main@pre-rm-1", CreatedAt: t0},
		{Name: "b-noble-x-factory@pre-rm-1", CreatedAt: t0.Add(time.Second)},
		{Name: "b-noble-main@manual-2", CreatedAt: t0.Add(time.Hour)},
		{Name: "other-repo@pre-rm-1", CreatedAt: t0},
		{Name: "no-label", CreatedAt: t0},
	}
	ls := labels(snaps, c.Publish["b"])
	if len(ls) != 2 || ls[0].Label != "manual-2" || ls[1].Label != "pre-rm-1" {
		t.Fatalf("labels = %+v", ls)
	}
	if len(ls[1].Snaps) != 2 || ls[1].Snaps["b-noble-x-factory"] != "b-noble-x-factory@pre-rm-1" {
		t.Fatalf("snaps = %v", ls[1].Snaps)
	}
	if !ls[1].Created.Equal(t0) {
		t.Fatal("label time should be its earliest snapshot")
	}
}
