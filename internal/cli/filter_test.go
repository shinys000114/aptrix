// SPDX-License-Identifier: GPL-3.0-only

package cli

import (
	"strings"
	"testing"

	"github.com/shinys000114/aptrix/internal/aptly"
)

func TestParseKey(t *testing.T) {
	k := parseKey("Parm64 linux-image-6.6-dbg 9:6.6.144-2026~resolute 3f2a")
	if k.Arch != "arm64" || k.Name != "linux-image-6.6-dbg" || k.Version != "9:6.6.144-2026~resolute" {
		t.Fatalf("%+v", k)
	}
}

func TestMatcher(t *testing.T) {
	m, err := newMatcher([]string{"linux-image-*vendor*", "*-dbg"}, ">=9:6.6.144-202608000000,<9:6.6.144-202610000000", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, ver, arch string
		want            bool
	}{
		{"linux-image-6.6.0-vendor-board-arm64", "9:6.6.144-202609031112~resolute", "arm64", true},
		{"foo-dbg", "9:6.6.144-202609031112~resolute", "arm64", true},
		{"linux-headers-6.6.0-vendor", "9:6.6.144-202609031112~resolute", "arm64", false}, // name
		{"foo-dbg", "9:6.6.144-202610151627~resolute", "arm64", false},                    // too new
		{"foo-dbg", "9:6.6.144-202607310000~resolute", "arm64", false},                    // too old
		{"foo-dbg", "9:6.6.144-202609031112~resolute", "amd64", false},                    // arch
	}
	for _, c := range cases {
		if got := m.match(c.name, c.ver, c.arch); got != c.want {
			t.Errorf("match(%s %s %s) = %v", c.name, c.ver, c.arch, got)
		}
	}
	for _, bad := range []string{"[", ""} {
		if _, err := newMatcher([]string{bad}, "", ""); bad == "[" && err == nil {
			t.Error("bad glob accepted")
		}
	}
	for _, bad := range []string{">=", ",", "<<"} {
		if _, err := newMatcher(nil, bad, ""); err == nil {
			t.Errorf("bad version condition %q accepted", bad)
		}
	}
	for cond, v := range map[string]string{"1.0": "1.0", "=1.0": "1.0", "!=1.0": "1.1", ">>1.0": "1.1", "<<1.0": "1.0~rc1"} {
		m, err := newMatcher(nil, cond, "")
		if err != nil || !m.match("x", v, "all") {
			t.Errorf("%q should match %q (%v)", cond, v, err)
		}
	}
}

func row(repo, name, ver, arch string) pkgRow {
	return pkgRow{Repo: repo, Package: aptly.Package{Key: "P" + arch + " " + name + " " + ver, Package: name, Version: ver, Architecture: arch}}
}

func names(rows []pkgRow) string {
	var s []string
	for _, r := range rows {
		s = append(s, r.Repo+":"+r.Package.Package+"="+r.Version+"/"+r.Architecture)
	}
	return strings.Join(s, " ")
}

func TestBeyondKeepAndNewest(t *testing.T) {
	rows := []pkgRow{
		row("r1", "a", "1.0", "arm64"), row("r1", "a", "1.10", "arm64"), row("r1", "a", "1.9", "arm64"),
		row("r1", "a", "1.0", "amd64"),
		row("r2", "a", "1.0", "arm64"),
		row("r1", "b", "2.0~rc1", "arm64"), row("r1", "b", "2.0", "arm64"),
	}
	if got := names(beyondKeep(rows, 2)); got != "r1:a=1.0/arm64" {
		t.Fatalf("keep 2: %s", got)
	}
	if got := names(beyondKeep(rows, 1)); got != "r1:a=1.9/arm64 r1:a=1.0/arm64 r1:b=2.0~rc1/arm64" {
		t.Fatalf("keep 1: %s", got)
	}
	if got := names(newestOnly(rows)); got != "r1:a=1.10/arm64 r1:a=1.0/amd64 r2:a=1.0/arm64 r1:b=2.0/arm64" {
		t.Fatalf("newest: %s", got)
	}
	if len(beyondKeep(rows, 0)) != len(rows) {
		t.Fatal("keep 0 removes everything")
	}
}
