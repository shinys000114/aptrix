// SPDX-License-Identifier: GPL-3.0-only

package cli

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/shinys000114/aptrix/internal/debver"
)

// keyInfo is what an aptly package key ("P<arch> <name> <version> <hash>")
// says about a package.
type keyInfo struct {
	Arch, Name, Version string
}

func parseKey(key string) keyInfo {
	f := strings.Fields(key)
	if len(f) < 3 || !strings.HasPrefix(f[0], "P") {
		return keyInfo{Name: key}
	}
	return keyInfo{Arch: strings.TrimPrefix(f[0], "P"), Name: f[1], Version: f[2]}
}

// versionCond is one "--version" constraint, e.g. ">=1.2-3".
type versionCond struct {
	op, v string
}

var versionOps = []string{">=", "<=", "!=", ">>", "<<", ">", "<", "="}

func parseVersionCond(s string) (versionCond, error) {
	s = strings.TrimSpace(s)
	for _, op := range versionOps {
		if strings.HasPrefix(s, op) {
			v := strings.TrimSpace(strings.TrimPrefix(s, op))
			if v == "" {
				return versionCond{}, fmt.Errorf("version condition %q has no version", s)
			}
			switch op {
			case ">>":
				op = ">"
			case "<<":
				op = "<"
			}
			return versionCond{op: op, v: v}, nil
		}
	}
	if s == "" {
		return versionCond{}, fmt.Errorf("empty version condition")
	}
	return versionCond{op: "=", v: s}, nil
}

func (c versionCond) match(v string) bool {
	r := debver.Compare(v, c.v)
	switch c.op {
	case ">=":
		return r >= 0
	case "<=":
		return r <= 0
	case ">":
		return r > 0
	case "<":
		return r < 0
	case "!=":
		return r != 0
	}
	return r == 0
}

// matcher selects packages by name globs, version conditions and arch.
type matcher struct {
	names    []string
	versions []versionCond
	arch     string
}

// newMatcher validates globs (shell style: * ? [...]) and version conditions
// (comma separated, all must hold: ">=1.0,<2.0").
func newMatcher(names []string, versions, arch string) (*matcher, error) {
	m := &matcher{names: names, arch: arch}
	for _, n := range names {
		if _, err := path.Match(n, ""); err != nil {
			return nil, fmt.Errorf("bad package pattern %q: %v", n, err)
		}
	}
	if versions != "" {
		for _, part := range strings.Split(versions, ",") {
			c, err := parseVersionCond(part)
			if err != nil {
				return nil, err
			}
			m.versions = append(m.versions, c)
		}
	}
	return m, nil
}

func (m *matcher) match(name, version, arch string) bool {
	if m.arch != "" && arch != m.arch {
		return false
	}
	if len(m.names) > 0 {
		ok := false
		for _, p := range m.names {
			if hit, _ := path.Match(p, name); hit {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, c := range m.versions {
		if !c.match(version) {
			return false
		}
	}
	return true
}

// beyondKeep returns the rows that are not among the newest n versions of
// their (repo, package, arch). Rows may be in any order.
func beyondKeep(rows []pkgRow, n int) []pkgRow {
	groups := map[string][]pkgRow{}
	var order []string
	for _, r := range rows {
		k := r.Repo + "\x00" + r.Package.Package + "\x00" + r.Architecture
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	var out []pkgRow
	for _, k := range order {
		g := groups[k]
		sort.SliceStable(g, func(i, j int) bool { return debver.Compare(g[i].Version, g[j].Version) > 0 })
		if len(g) > n {
			out = append(out, g[n:]...)
		}
	}
	return out
}

// newestOnly keeps only the newest version per (repo, package, arch).
func newestOnly(rows []pkgRow) []pkgRow {
	old := beyondKeep(rows, 1)
	drop := map[string]bool{}
	for _, r := range old {
		drop[r.Repo+"\x00"+r.Key] = true
	}
	var out []pkgRow
	for _, r := range rows {
		if !drop[r.Repo+"\x00"+r.Key] {
			out = append(out, r)
		}
	}
	return out
}
