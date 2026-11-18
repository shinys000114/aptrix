// SPDX-License-Identifier: GPL-3.0-only

// Package debver compares Debian package versions using the dpkg algorithm
// ([epoch:]upstream[-revision], with '~' sorting before everything).
package debver

import "strings"

// Compare returns -1, 0 or 1 when a is older than, equal to or newer than b.
func Compare(a, b string) int {
	ea, ua, ra := split(a)
	eb, ub, rb := split(b)
	if c := cmpNum(ea, eb); c != 0 {
		return c
	}
	if c := cmpPart(ua, ub); c != 0 {
		return c
	}
	return cmpPart(ra, rb)
}

func split(v string) (epoch, upstream, revision string) {
	v = strings.TrimSpace(v)
	if i := strings.IndexByte(v, ':'); i >= 0 {
		epoch, v = v[:i], v[i+1:]
	}
	if i := strings.LastIndexByte(v, '-'); i >= 0 {
		return epoch, v[:i], v[i+1:]
	}
	return epoch, v, ""
}

func order(c byte) int {
	switch {
	case c == '~':
		return -1
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return int(c)
	case c == 0:
		return 0
	default:
		return int(c) + 256
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func cmpPart(a, b string) int {
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		// non-digit prefix
		for (i < len(a) && !isDigit(a[i])) || (j < len(b) && !isDigit(b[j])) {
			var ca, cb byte
			if i < len(a) && !isDigit(a[i]) {
				ca = a[i]
			}
			if j < len(b) && !isDigit(b[j]) {
				cb = b[j]
			}
			if oa, ob := order(ca), order(cb); oa != ob {
				if oa < ob {
					return -1
				}
				return 1
			}
			if ca != 0 {
				i++
			}
			if cb != 0 {
				j++
			}
		}
		// digit run
		si := i
		for i < len(a) && isDigit(a[i]) {
			i++
		}
		sj := j
		for j < len(b) && isDigit(b[j]) {
			j++
		}
		if c := cmpNum(a[si:i], b[sj:j]); c != 0 {
			return c
		}
	}
	return 0
}

func cmpNum(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}
