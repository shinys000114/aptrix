// SPDX-License-Identifier: GPL-3.0-only

package cli

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal reports whether f is a terminal. /dev/null is a character
// device too, so a mode check is not enough; ask the tty driver instead.
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
