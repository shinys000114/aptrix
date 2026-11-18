// SPDX-License-Identifier: GPL-3.0-only

package debver

import (
	"os/exec"
	"testing"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0", "1.0", 0},
		{"1.0", "1.1", -1},
		{"1.10", "1.9", 1},
		{"1.0~rc1", "1.0", -1},
		{"1.0~~", "1.0~", -1},
		{"1.0", "1.0+b1", -1},
		{"1:0.1", "9.9", 1},
		{"0:1.0", "1.0", 0},
		{"1.0-1", "1.0-2", -1},
		{"1.0-10", "1.0-9", 1},
		{"1.0a", "1.0", 1},
		{"1.0", "1.0.0", -1},
		{"9:6.6.144-202610151627~resolute", "9:6.6.144-202608241426~resolute", 1},
		{"0.1.3+260630103746~noble", "0.1.3+260708151659~noble", -1},
		{"0.1.3+260708151659~noble", "0.1.3+260708151659", -1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := Compare(c.b, c.a); got != -c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

// TestCompareAgainstDpkg cross-checks with dpkg when it is installed.
func TestCompareAgainstDpkg(t *testing.T) {
	if _, err := exec.LookPath("dpkg"); err != nil {
		t.Skip("dpkg not installed")
	}
	vs := []string{"1.0", "1.0~rc1", "1.0+b1", "1:0.1", "1.0-1", "1.0-1~bpo1", "2.0~", "2.0~~a",
		"1.0a", "1.0.0", "0.1.3+260630103746~noble", "0.1.3+260708151659~noble", "1.0-1.1", "1.0-1a",
		"9:6.6.144-202610151627~resolute", "9:6.6.144-202608241426~resolute", "1.2.3-0ubuntu1", "1.2.3-0ubuntu1.1"}
	for _, a := range vs {
		for _, b := range vs {
			want := 0
			if exec.Command("dpkg", "--compare-versions", a, "lt", b).Run() == nil {
				want = -1
			} else if exec.Command("dpkg", "--compare-versions", a, "gt", b).Run() == nil {
				want = 1
			}
			if got := Compare(a, b); got != want {
				t.Errorf("Compare(%q, %q) = %d, dpkg says %d", a, b, got, want)
			}
		}
	}
}
