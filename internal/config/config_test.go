// SPDX-License-Identifier: GPL-3.0-only

package config

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const good = `
[aptly]
url = "http://127.0.0.1:8080"

[signing]
gpg_key = "ABCDEF"

[publish.board]
storage = "filesystem:public"
prefix = "board"
dists = ["jammy", "noble"]
components = ["main", "vendor-factory"]
architectures = ["arm64"]

[publish.board.only]
vendor-factory = ["noble"]

[publish.tool]
prefix = "tool"
repo_name = "tool-{dist}"
dists = ["noble"]
components = ["tool"]
architectures = ["amd64", "arm64"]
`

func TestParseGood(t *testing.T) {
	c, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	b := c.Publish["board"]
	if b.Alias != "board" || b.RepoName != "board-{dist}-{component}" {
		t.Fatalf("defaults not applied: %+v", b)
	}
	cells := b.Cells()
	var got []string
	for _, cell := range cells {
		got = append(got, cell.Repo)
	}
	want := "board-jammy-main board-noble-main board-noble-vendor-factory"
	if strings.Join(got, " ") != want {
		t.Fatalf("cells = %v, want %s", got, want)
	}
	if b.HasDist("vendor-factory", "jammy") {
		t.Fatal("only should restrict vendor-factory to noble")
	}
	if c.Publish["tool"].RepoFor("noble", "tool") != "tool-noble" {
		t.Fatal("repo_name template")
	}
	if got := strings.Join(c.Aliases(), ","); got != "board,tool" {
		t.Fatalf("aliases = %s", got)
	}
	if _, err := c.Select([]string{"nope"}); err == nil {
		t.Fatal("unknown alias should fail")
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unknown config keys":          strings.Replace(good, `prefix = "tool"`, "prefix = \"tool\"\ntypo = 1", 1),
		"signing.gpg_key is required":  strings.Replace(good, `gpg_key = "ABCDEF"`, "", 1),
		"architectures is empty":       strings.Replace(good, "architectures = [\"amd64\", \"arm64\"]", "", 1),
		"is not in components":         strings.Replace(good, `vendor-factory = ["noble"]`, `other = ["noble"]`, 1),
		"is not in dists":              strings.Replace(good, `vendor-factory = ["noble"]`, `vendor-factory = ["focal"]`, 1),
		"duplicate entry":              strings.Replace(good, `dists = ["jammy", "noble"]`, `dists = ["noble", "noble"]`, 1),
		"must contain {component}":     strings.Replace(good, `components = ["tool"]`, `components = ["tool", "x"]`, 1),
		"same storage/prefix":          strings.Replace(good, `prefix = "tool"`, "storage = \"filesystem:public\"\nprefix = \"board\"", 1),
		"also used by":                 strings.Replace(good, `repo_name = "tool-{dist}"`, `repo_name = "board-{dist}-main"`, 1),
		"invalid name":                 strings.Replace(good, `dists = ["noble"]`, `dists = ["no ble"]`, 1),
		"keep must be >= 0":            strings.Replace(good, `prefix = "tool"`, "prefix = \"tool\"\nkeep = -1", 1),
		"snapshot_max_age must be > 0": strings.Replace(good, `prefix = "tool"`, "prefix = \"tool\"\nsnapshot_max_age = \"0d\"", 1),
		"snapshot_max_age":             strings.Replace(good, `prefix = "tool"`, "prefix = \"tool\"\nsnapshot_max_age = \"soon\"", 1),
	}
	for want, data := range cases {
		_, err := Parse([]byte(data))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want error containing %q, got %v", want, err)
		}
	}
}

func TestLoadURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/aptrix.toml":
			w.Write([]byte(good))
		case "/big.toml":
			w.Write(bytes.Repeat([]byte("#"), maxRemoteConfig+1))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if !IsURL(srv.URL+"/aptrix.toml") || IsURL("/etc/aptrix/aptrix.toml") {
		t.Fatal("IsURL")
	}
	c, err := Load(srv.URL + "/aptrix.toml")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Publish["board"]; !ok {
		t.Fatal("remote config not parsed")
	}
	if _, err := Load(srv.URL + "/missing.toml"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("404 should fail, got %v", err)
	}
	if _, err := Load(srv.URL + "/big.toml"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversize should fail, got %v", err)
	}
	t.Setenv("APTRIX_CONFIG", srv.URL+"/aptrix.toml")
	if loc, err := Find(""); err != nil || loc != srv.URL+"/aptrix.toml" {
		t.Fatalf("Find with URL env = %q, %v", loc, err)
	}
}

func TestParseAge(t *testing.T) {
	for in, want := range map[string]string{"30d": "720h0m0s", "12h": "12h0m0s", "90m": "1h30m0s", "0d": "0s"} {
		d, err := ParseAge(in)
		if err != nil || d.String() != want {
			t.Errorf("ParseAge(%q) = %v, %v; want %s", in, d, err, want)
		}
	}
	for _, bad := range []string{"", "d", "-1d", "1.5d", "3w", "x"} {
		if _, err := ParseAge(bad); err == nil {
			t.Errorf("ParseAge(%q) should fail", bad)
		}
	}
}
