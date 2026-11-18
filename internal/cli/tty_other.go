// SPDX-License-Identifier: GPL-3.0-only

//go:build !linux

package cli

import "os"

// isTerminal is conservative off Linux: never prompt, require --yes.
func isTerminal(*os.File) bool { return false }
