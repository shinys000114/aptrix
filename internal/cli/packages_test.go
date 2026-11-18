// SPDX-License-Identifier: GPL-3.0-only

package cli

import (
	"flag"
	"io"
	"strings"
	"testing"
)

func TestParseDebName(t *testing.T) {
	d, err := parseDebName("/x/linux-image-6.6.0-board-arm64_6.6.144-202610151627~resolute_arm64.deb")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "linux-image-6.6.0-board-arm64" || d.Version != "6.6.144-202610151627~resolute" || d.Arch != "arm64" {
		t.Fatalf("%+v", d)
	}
	for _, bad := range []string{"a.deb", "a_1.deb", "a_1_arm64.tar", "a__arm64.deb", "a_1_b_c.deb"} {
		if _, err := parseDebName(bad); err == nil {
			t.Errorf("%s should be rejected", bad)
		}
	}
}

func TestDetectDist(t *testing.T) {
	dists := []string{"jammy", "noble"}
	for v, want := range map[string]string{
		"0.1.3+260630103746~noble": "noble",
		"1.0~rc1~jammy":            "jammy",
		"1.0~rc1":                  "",
		"1.0":                      "",
		"1.0~focal":                "",
	} {
		if got := detectDist(v, dists); got != want {
			t.Errorf("detectDist(%q) = %q, want %q", v, got, want)
		}
	}
}

func TestInterspersedFlags(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dist := fs.String("dist", "", "")
	yes := fs.Bool("yes", false, "")
	pos, err := parse(fs, []string{"alias", "--dist", "all", "comp", "a.deb", "--yes", "b.deb"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pos, " ") != "alias comp a.deb b.deb" || *dist != "all" || !*yes {
		t.Fatalf("pos=%v dist=%q yes=%v", pos, *dist, *yes)
	}
}

func TestConfirmWithoutTTY(t *testing.T) {
	e := &env{stdin: strings.NewReader("yes\n"), stderr: io.Discard}
	if err := e.confirm(false, "do it"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("non-TTY must require --yes, got %v", err)
	}
	if err := e.confirm(true, "do it"); err != nil {
		t.Fatal(err)
	}
	e.isTTY = true
	if err := e.confirm(false, "do it"); err != nil {
		t.Fatalf("typed yes should pass: %v", err)
	}
}
